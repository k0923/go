package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WaitCase describes a signal. A zero Kind means an external WaitFor signal,
// preserving existing callers. Duration is used only for relative Sleep;
// a nonzero Until selects an absolute sleep deadline.
type WaitCase[R any] struct {
	Key      string
	Request  R
	Kind     Kind
	Duration time.Duration
	Until    time.Time
}

// Signal is the heterogeneous, transportable description accepted by WaitAny.
// Custom kinds require an Infrastructure implementation supporting their
// registration and resolution semantics; unknown kinds are never silently
// treated as WaitFor by the reference memory store.
type Signal = WaitCase[any]

// WaitForSignal describes a signal resolved by an external JSON response.
func WaitForSignal[R any](key string, request R) Signal {
	return Signal{Key: key, Kind: SignalKind, Request: request}
}

// SleepSignal describes a timer whose relative deadline is fixed on registration.
func SleepSignal(key string, duration time.Duration) Signal {
	return Signal{Key: key, Kind: SleepKind, Duration: duration}
}

// SleepUntilSignal describes a timer with an absolute deadline.
func SleepUntilSignal(key string, until time.Time) Signal {
	return Signal{Key: key, Kind: SleepKind, Until: until}
}

type WaitFailure struct {
	Key     string
	Message string
}

type WaitAnyResult[R any] struct {
	Key      string // resolved winner key, e.g. approval|primary
	Kind     Kind   // SleepKind winners have a zero Response, not a JSON payload
	Response R
	Failures []WaitFailure // isolated candidate errors; not a drive failure
}

// WaitRequest is the transport representation consumed by storage adapters.
type WaitRequest struct {
	Key      string
	Request  json.RawMessage
	Kind     Kind
	Duration time.Duration
	Until    time.Time
}

type WaitAnyState struct {
	Status     Status
	WinnerKey  string
	WinnerKind Kind
	Response   json.RawMessage
	Pending    []Pending
	Failures   []WaitFailure
}

// WaitAnyInfrastructure is OPTIONAL. Existing Infrastructure implementations
// remain source-compatible; WaitAny requires an adapter implementing this API.
//
// WaitAny atomically registers ALL signals + request notifications, or reads an
// existing group's snapshot after checking its ordered keys and request bytes.
// Group ownership must be persisted. The existing Resolve operation MUST, in
// the same transaction, choose the first successful member, cancel every other
// pending member, remove obsolete notifications and enqueue CANCEL notifications
// for external providers. Subsequent callbacks cannot
// overwrite the winner or resurrect a cancelled member.
//
// FailWait atomically fails one member without cancelling healthy members.
// If no candidates remain it fails the group and enqueues a wake. Panic/IO
// failures of the adapter are group-level infrastructure errors, not candidate
// failures. Returned snapshots must be deep copies.
type WaitAnyInfrastructure interface {
	WaitAny(context.Context, string, []WaitRequest) (*WaitAnyState, error)
	FailWait(context.Context, string, int64, string) (bool, error)
}

var ErrWaitAnyUnsupported = errors.New("infrastructure does not support WaitAny")
var ErrSignalUnsupported = errors.New("signal kind unsupported")

// WaitAny registers all candidates simultaneously. It returns a suspended error
// until one signal completes; Engine.Drive reports ALL pending keys. The first
// accepted callback cancels losers immediately in storage. On the next Drive
// the workflow resumes and returns the durable winner, without awaiting losers.
//
// Invalid key topology is rejected before registration. A candidate whose
// request cannot be encoded is isolated in Failures while healthy cases remain.
// Payload-bearing signals share response type R; callbacks must send matching
// JSON. A Sleep winner has Kind=SleepKind and a zero response.
func (f *FlowContext) WaitAny[R, Q any](key string, cases ...WaitCase[Q]) (result WaitAnyResult[R], err error) {
	defer f.remember(&err)
	group, err := f.claim(key)
	if err != nil {
		return result, err
	}
	api, ok := f.execution.engine.store.(WaitAnyInfrastructure)
	if !ok {
		return result, ErrWaitAnyUnsupported
	}
	if len(cases) == 0 {
		return result, fmt.Errorf("%w: empty waiting group", ErrInvalid)
	}
	child := &FlowContext{execution: f.execution, prefix: group}
	// Validate every key before any external registration.
	keys := make([]string, len(cases))
	for i, candidate := range cases {
		keys[i], err = child.claim(candidate.Key)
		if err != nil {
			return result, err
		}
	}
	var requests []WaitRequest
	var failures []WaitFailure
	for i, candidate := range cases {
		kind := candidate.Kind
		if kind == "" {
			kind = SignalKind
		}
		var raw json.RawMessage
		var marshalErr error
		if kind != SleepKind {
			raw, marshalErr = invoke(func() (json.RawMessage, error) { return json.Marshal(candidate.Request) })
		}
		if marshalErr != nil {
			failures = append(failures, WaitFailure{Key: keys[i], Message: marshalErr.Error()})
			continue
		}
		requests = append(requests, WaitRequest{
			Key: keys[i], Request: raw, Kind: kind,
			Duration: candidate.Duration, Until: candidate.Until,
		})
	}
	if len(requests) == 0 {
		result.Failures = failures
		return result, Permanent(fmt.Errorf("wait group %s has no usable candidates: %v", group, failures))
	}
	state, stateErr := api.WaitAny(f.Context(), group, requests)
	if stateErr != nil {
		return result, &OperationError{group, "WaitAny", stateErr}
	}
	if state == nil {
		return result, fmt.Errorf("%w: nil WaitAny snapshot", ErrInvalid)
	}
	result.Failures = append(failures, state.Failures...)
	switch state.Status {
	case Completed:
		result.Key = state.WinnerKey
		result.Kind = state.WinnerKind
		if result.Kind == "" {
			result.Kind = SignalKind
		}
		if result.Kind == SleepKind {
			return result, nil
		}
		err = json.Unmarshal(state.Response, &result.Response)
		return result, err
	case Failed:
		return result, Permanent(fmt.Errorf("wait group %s failed: %v", group, state.Failures))
	case Suspended:
		if len(state.Pending) == 0 {
			return result, fmt.Errorf("%w: suspended group has no pending members", ErrInvalid)
		}
		f.execution.mu.Lock()
		for _, pending := range state.Pending {
			f.execution.pending[pending.Key] = pending
		}
		f.execution.mu.Unlock()
		for _, pending := range state.Pending {
			f.execution.engine.emit(f.Context(), Event{Type: WaitSuspended, Key: pending.Key, Kind: pending.Kind,
				Status: Suspended, Pending: []Pending{pending},
			}, nil)
		}
		return result, &SuspendedError{Pending: state.Pending[0]}
	default:
		return result, ErrInvalid
	}
}

// FailWait reports an independent candidate failure. It does not return a
// workflow error when other candidates are still waiting. Duplicate reports or
// reports for already resolved/cancelled members return false.
func (e *Engine) FailWait(ctx context.Context, key string, version int64, cause error) (bool, error) {
	if cause == nil {
		return false, ErrInvalid
	}
	api, ok := e.store.(WaitAnyInfrastructure)
	if !ok {
		return false, ErrWaitAnyUnsupported
	}
	return api.FailWait(ctx, key, version, cause.Error())
}
