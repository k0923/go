package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k0923/go/lease"
)

type EventType string

const (
	DriveStarted   EventType = "drive_started"
	DriveFinished  EventType = "drive_finished"
	ActionStarted  EventType = "action_started"
	ActionFinished EventType = "action_finished"
	ActionReplayed EventType = "action_replayed"
	WaitSuspended  EventType = "wait_suspended"
	WaitResolved   EventType = "wait_resolved"
)

// Event is a versioned, JSON-serializable observation, not a durable outbox
// message. It excludes input/output payloads. Error text comes from the caller;
// applications must avoid including secrets in their error messages.
// ExecutionID identifies ONE Drive invocation, including rejected invocations.
// ResolveSignal outside Drive has no ExecutionID; correlate it by scoped key.
// Sequence orders production within one Engine, not concurrent callback delivery.
type Event struct {
	SchemaVersion   int           `json:"schema_version"`
	ID              string        `json:"id"`
	EngineID        string        `json:"engine_id"`
	Sequence        uint64        `json:"sequence"`
	ExecutionID     string        `json:"execution_id,omitempty"`
	WorkflowName    string        `json:"workflow_name,omitempty"`
	WorkflowVersion string        `json:"workflow_version,omitempty"`
	Type            EventType     `json:"type"`
	Key             string        `json:"key"`
	Kind            Kind          `json:"kind,omitempty"`
	Status          Status        `json:"status"`
	At              time.Time     `json:"at"`
	StartedAt       time.Time     `json:"started_at"`
	Duration        time.Duration `json:"duration_ns"`
	Replayed        bool          `json:"replayed,omitempty"`
	RecordVersion   int64         `json:"record_version,omitempty"`
	Pending         []Pending     `json:"pending,omitempty"`
	Error           *EventError   `json:"error,omitempty"`
}

// EventError contains transportable diagnostic data. It does not retain the
// original Go error or claim that sentinel identities survive JSON encoding.
type EventError struct {
	Category  string `json:"category"`
	Message   string `json:"message"`
	Operation string `json:"operation,omitempty"`
}

// EventListener must be concurrency-safe and return promptly. Delivery is
// synchronous, so events are captured before the emitting API returns.
// Panics are isolated independently for each listener.
type EventListener func(context.Context, Event)

type eventSubscription struct {
	id       uint64
	listener EventListener
	types    map[EventType]bool
}

type eventHub struct {
	mu        sync.Mutex
	next      uint64
	sequence  atomic.Uint64
	listeners []eventSubscription
}

// Subscribe registers a listener, optionally filtered to event types. No types
// means all events. The returned unsubscribe function is idempotent and safe
// inside a callback. A delivery already snapshotted may finish after unsubscribe.
// No historical events are replayed. A nil listener is a harmless no-op.
// Separate registrations are separate subscriptions, even for the same function.
func (e *Engine) Subscribe(listener EventListener, types ...EventType) func() {
	if listener == nil {
		return func() {}
	}
	filter := make(map[EventType]bool, len(types))
	for _, eventType := range types {
		filter[eventType] = true
	}
	hub := &e.events
	hub.mu.Lock()
	hub.next++
	id := hub.next
	hub.listeners = append(hub.listeners, eventSubscription{id: id, listener: listener, types: filter})
	hub.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			hub.mu.Lock()
			defer hub.mu.Unlock()
			for i, subscription := range hub.listeners {
				if subscription.id == id {
					copy(hub.listeners[i:], hub.listeners[i+1:])
					hub.listeners[len(hub.listeners)-1] = eventSubscription{}
					hub.listeners = hub.listeners[:len(hub.listeners)-1]
					break
				}
			}
		})
	}
}

type eventContextKey struct{}
type eventContext struct{ executionID, name, version string }

func (e *Engine) emit(ctx context.Context, event Event, err error) {
	// Even formatting an application-defined error must not break execution.
	defer func() { _ = recover() }()
	event.SchemaVersion = 1
	event.EngineID = e.eventID
	event.Sequence = e.events.sequence.Add(1)
	event.ID = fmt.Sprintf("%s:%d", e.eventID, event.Sequence)
	metadata, _ := ctx.Value(eventContextKey{}).(eventContext)
	event.ExecutionID, event.WorkflowName, event.WorkflowVersion = metadata.executionID, metadata.name, metadata.version
	event.At = e.options.Now()
	if event.StartedAt.IsZero() {
		event.StartedAt = event.At
	}
	event.Duration = event.At.Sub(event.StartedAt)
	if event.Duration < 0 {
		event.Duration = 0
	}
	if err != nil {
		event.Error = describeEventError(err)
	}
	e.events.mu.Lock()
	listeners := append([]eventSubscription(nil), e.events.listeners...)
	e.events.mu.Unlock()
	for _, subscription := range listeners {
		if len(subscription.types) > 0 && !subscription.types[event.Type] {
			continue
		}
		// Prevent one listener from corrupting another listener's snapshot.
		copyEvent := event
		copyEvent.Pending = append([]Pending(nil), event.Pending...)
		if event.Error != nil {
			copyError := *event.Error
			copyEvent.Error = &copyError
		}
		func() {
			defer func() { _ = recover() }()
			subscription.listener(ctx, copyEvent)
		}()
	}
}

func describeEventError(err error) (result *EventError) {
	result = &EventError{Category: "execution", Message: "error description unavailable"}
	// User-defined Error/Is/As methods may panic. Retain a diagnostic event
	// with fallback text rather than skipping delivery to every subscriber.
	defer func() { _ = recover() }()
	result.Message = err.Error()
	var operation *OperationError
	var leaseErr *lease.Error
	var permanent *PermanentError
	var replay *ReplayError
	var panicErr *PanicError
	hasOperation := errors.As(err, &operation)
	hasLease := errors.As(err, &leaseErr)
	if hasOperation {
		result.Operation = operation.Op
	} else if hasLease {
		result.Operation = leaseErr.Op
	}
	switch {
	case errors.As(err, &panicErr):
		result.Category = "panic"
	case errors.Is(err, ErrConflict):
		result.Category = "conflict"
	case errors.Is(err, context.Canceled):
		result.Category = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		result.Category = "timeout"
	case hasOperation:
		result.Category = "infrastructure"
	case hasLease:
		result.Category = "lease"
	case errors.As(err, &replay):
		result.Category = "replay"
	case errors.As(err, &permanent):
		result.Category = "permanent"
	case errors.Is(err, ErrInvalid):
		result.Category = "invalid"
	case errors.Is(err, ErrChanged):
		result.Category = "changed"
	}
	return result
}
