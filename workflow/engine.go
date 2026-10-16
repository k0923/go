package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/k0923/go/lease"
)

type Options struct {
	LeaseTTL       time.Duration
	CleanupTimeout time.Duration
	Observer       Observer
	// Now supplies workflow clock reads; production must agree with the store
	// clock. Lease validity is ALWAYS decided by the store.
	Now func() time.Time
}

type Engine struct {
	store   Infrastructure
	options Options
	leases  *lease.Runner
	eventID string
	events  eventHub
}

func NewEngine(store Infrastructure, options Options) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: infrastructure required", ErrInvalid)
	}
	if options.LeaseTTL == 0 {
		options.LeaseTTL = 30 * time.Second
	}
	if options.CleanupTimeout == 0 {
		options.CleanupTimeout = 5 * time.Second
	}
	if options.LeaseTTL < 3*time.Nanosecond || options.CleanupTimeout <= 0 {
		return nil, ErrInvalid
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	leases, err := lease.NewRunner(store, lease.Options{
		TTL: options.LeaseTTL, CleanupTimeout: options.CleanupTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &Engine{store: store, options: options, leases: leases, eventID: uuid.NewString()}, nil
}

// ActionKey is stable within a scoped run. For external idempotency compose
// it with your run identity; do not use the generation as an idempotency key.
func ActionKey(ctx context.Context) string {
	key, _ := ctx.Value(actionKey{}).(string)
	return key
}

type actionKey struct{}

type execution struct {
	engine  *Engine
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	keys    map[string]bool
	errs    []error
	pending map[string]Pending
	closed  bool
	wg      sync.WaitGroup
}

var localKey = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// FlowContext is deliberately NOT context.Context. Context() is the only
// standard context passed to external clients; workflow APIs live here.
type FlowContext struct {
	execution *execution
	prefix    string
}

func (f *FlowContext) Context() context.Context { return f.execution.ctx }
func (f *FlowContext) Key() string              { return f.prefix }

func (f *FlowContext) claim(key string) (string, error) {
	if !localKey.MatchString(key) {
		return "", fmt.Errorf("%w: key %q", ErrInvalid, key)
	}
	resolved := key
	if f.prefix != "" {
		resolved = f.prefix + "|" + key
	}
	e := f.execution
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", fmt.Errorf("%w: execution closed", ErrInvalid)
	}
	if err := e.ctx.Err(); err != nil {
		return "", err
	}
	if e.keys[resolved] {
		return "", fmt.Errorf("%w: duplicate key %s", ErrInvalid, resolved)
	}
	e.keys[resolved] = true
	return resolved, nil
}

func (f *FlowContext) remember(err *error) {
	if *err == nil {
		return
	}
	e := f.execution
	e.mu.Lock()
	defer e.mu.Unlock()
	var pending *SuspendedError
	if errors.As(*err, &pending) {
		e.pending[pending.Pending.Key] = pending.Pending
		return
	}
	e.errs = append(e.errs, *err)
	// Waiting is a normal outcome. Actual errors cancel ongoing sibling IO.
	e.cancel()
}

func (e *Engine) observe(ctx context.Context, key, phase string, started time.Time, err error) {
	if e.options.Observer == nil {
		return
	}
	// Instrumentation failures do not turn a committed action into a retry.
	defer func() { _ = recover() }()
	now := e.options.Now()
	e.options.Observer(ctx, Observation{Key: key, Phase: phase, At: now, Duration: now.Sub(started), Err: err})
}

// Drive replays the definition from its entry point. It joins all registered
// async work before returning, including work the caller forgot to Wait().
// Non-nil error is retryable unless it is PermanentError or ReplayError.
func (e *Engine) Drive[I, O any](ctx context.Context, workflow Workflow[I, O], input I) (result Result[O], err error) {
	result.Status = Running
	eventCtx := ctx
	if eventCtx == nil {
		eventCtx = context.Background()
	}
	eventCtx = context.WithValue(eventCtx, eventContextKey{}, eventContext{
		executionID: uuid.NewString(), name: workflow.Name, version: workflow.Version,
	})
	eventStarted := e.options.Now()
	e.emit(eventCtx, Event{Type: DriveStarted, Key: workflow.Name, Status: Running, StartedAt: eventStarted}, nil)
	defer func() {
		e.emit(eventCtx, Event{
			Type: DriveFinished, Key: workflow.Name, Status: result.Status,
			StartedAt: eventStarted, Pending: result.Pending,
		}, err)
	}()
	if ctx == nil || workflow.Flow == nil || !localKey.MatchString(workflow.Name) || workflow.Version == "" {
		return result, ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	identity, err := json.Marshal(struct {
		Name, Version string
		Input         I
	}{workflow.Name, workflow.Version, input})
	if err != nil {
		return result, err
	}
	definition, err := e.store.Put(ctx, &Record{
		Key: "$definition", Kind: DefinitionKind, Status: Completed, Input: identity,
	})
	if err != nil {
		return result, &OperationError{Key: "$definition", Op: "Put", Cause: err}
	}
	if !bytes.Equal(definition.Input, identity) {
		return result, ErrChanged
	}
	runCtx, cancel := context.WithCancel(eventCtx)
	defer cancel()
	ex := &execution{engine: e, ctx: runCtx, cancel: cancel, keys: make(map[string]bool), pending: make(map[string]Pending)}
	flowCtx := &FlowContext{execution: ex}
	started := e.options.Now()
	e.observe(runCtx, workflow.Name, "drive_started", started, nil)
	output, handlerErr := invoke(func() (O, error) { return workflow.Flow.Run(flowCtx, input) })
	flowCtx.remember(&handlerErr)
	ex.wg.Wait()
	ex.mu.Lock()
	ex.closed = true
	err = errors.Join(ex.errs...)
	for _, pending := range ex.pending {
		result.Pending = append(result.Pending, pending)
	}
	ex.mu.Unlock()
	if err == nil {
		err = ctx.Err()
	}
	sort.Slice(result.Pending, func(i, j int) bool { return result.Pending[i].Key < result.Pending[j].Key })
	switch {
	case err != nil:
		var permanent *PermanentError
		var replay *ReplayError
		if errors.As(err, &permanent) || errors.As(err, &replay) {
			result.Status = Failed
		}
	case len(result.Pending) > 0:
		result.Status = Suspended
	default:
		result.Output = output
		result.Status = Completed
	}
	e.observe(ctx, workflow.Name, "drive_finished", started, err)
	return result, err
}

func (f *FlowContext) Run[I, O any](key string, action Action[I, O], input I, options ...ActionOptions) (output O, err error) {
	defer f.remember(&err)
	resolved, err := f.claim(key)
	if err != nil {
		return output, err
	}
	return f.runAction(resolved, action, input, options...)
}

func (f *FlowContext) runAction[I, O any](key string, action Action[I, O], input I, options ...ActionOptions) (output O, err error) {
	e := f.execution.engine
	eventStarted := e.options.Now()
	actionStatus := Running
	var recordVersion int64
	replayed := false
	defer func() {
		e.emit(f.Context(), Event{
			Type: ActionFinished, Key: key, Kind: ActionKind, Status: actionStatus,
			StartedAt: eventStarted, Replayed: replayed, RecordVersion: recordVersion,
		}, err)
	}()
	if action == nil || len(options) > 1 {
		return output, ErrInvalid
	}
	var option ActionOptions
	if len(options) == 1 {
		option = options[0]
	}
	if option.Timeout < 0 {
		return output, ErrInvalid
	}
	data, err := json.Marshal(input)
	if err != nil {
		return output, err
	}
	started := e.options.Now()
	defer func() { e.observe(f.Context(), key, "action_finished", started, err) }()
	record, err := e.store.Get(f.Context(), key)
	if err != nil {
		return output, &OperationError{key, "Get", err}
	}
	replay := func(r *Record) (bool, error) {
		if r == nil {
			return false, nil
		}
		if r.Kind != ActionKind || !bytes.Equal(r.Input, data) {
			return true, ErrChanged
		}
		switch r.Status {
		case Completed:
			e.observe(f.Context(), key, "action_replayed", started, nil)
			decodeErr := json.Unmarshal(r.Result, &output)
			replayed, actionStatus, recordVersion = true, r.Status, r.Version
			e.emit(f.Context(), Event{Type: ActionReplayed, Key: key, Kind: ActionKind,
				Status: r.Status, StartedAt: eventStarted, Replayed: true, RecordVersion: r.Version,
			}, decodeErr)
			return true, decodeErr
		case Failed:
			replayErr := &ReplayError{Key: key, Message: r.Error}
			replayed, actionStatus, recordVersion = true, r.Status, r.Version
			e.emit(f.Context(), Event{Type: ActionReplayed, Key: key, Kind: ActionKind,
				Status: r.Status, StartedAt: eventStarted, Replayed: true, RecordVersion: r.Version,
			}, replayErr)
			return true, replayErr
		case Running:
			return false, nil
		default:
			return true, ErrInvalid
		}
	}
	if done, replayErr := replay(record); done {
		return output, replayErr
	}
	err = e.leases.Run(f.Context(), key, func(executionCtx context.Context, token lease.Token) error {
		record, err = e.store.Get(executionCtx, key)
		if err != nil {
			return &OperationError{key, "Get", err}
		}
		if done, replayErr := replay(record); done {
			return replayErr
		}
		if record == nil {
			record, err = e.store.Commit(executionCtx, token, &Record{
				Key: key, Kind: ActionKind, Status: Running, Input: data, StartedAt: started,
			}, 0)
			if err != nil {
				return &OperationError{key, "Commit", err}
			}
		}
		recordVersion = record.Version
		actionCtx := context.WithValue(executionCtx, actionKey{}, key)
		if option.Timeout > 0 {
			var cancel context.CancelFunc
			actionCtx, cancel = context.WithTimeout(actionCtx, option.Timeout)
			defer cancel()
		}
		e.observe(actionCtx, key, "action_started", started, nil)
		e.emit(actionCtx, Event{Type: ActionStarted, Key: key, Kind: ActionKind,
			Status: Running, StartedAt: eventStarted, RecordVersion: record.Version,
		}, nil)
		var actionErr error
		output, actionErr = invoke(func() (O, error) { return action(actionCtx, input) })
		if contextErr := actionCtx.Err(); contextErr != nil {
			return errors.Join(actionErr, contextErr)
		}
		var permanent *PermanentError
		if actionErr != nil && !errors.As(actionErr, &permanent) {
			return actionErr
		}
		if actionErr != nil {
			record.Status = Failed
			record.Error = actionErr.Error()
		} else {
			var marshalErr error
			record.Result, marshalErr = json.Marshal(output)
			if marshalErr != nil {
				return marshalErr
			}
			record.Status = Completed
		}
		record.EndedAt = e.options.Now()
		// Renewal remains active during the commit. The storage boundary still
		// atomically validates current generation, expiry and record version.
		committed, commitErr := e.store.Commit(executionCtx, token, record, record.Version)
		if commitErr != nil {
			return errors.Join(actionErr, &OperationError{key, "Commit", commitErr})
		}
		actionStatus, recordVersion = record.Status, committed.Version
		return actionErr
	})
	return output, err
}
