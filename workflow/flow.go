package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RunFlow is available only to top-level orchestration. It creates a namespace,
// not another lease or workflow state. A called Flow cannot invoke another Flow.
// Returning includes joining the Flow's async Actions.
// RunFlow 仅允许顶层编排调用；被调用的 Flow 不可继续调用 Flow。
func (w *WorkflowContext) RunFlow[I, O any](key string, flow Flow[I, O], input I) (output O, err error) {
	f := w.state
	defer f.remember(&err)
	resolved, err := f.claim(key)
	if err != nil {
		return output, err
	}
	if flow == nil {
		return output, ErrInvalid
	}
	return f.runChildFlow(resolved, flow, input)
}

func (f *FlowContext) runChildFlow[I, O any](resolved string, flow Flow[I, O], input I) (output O, err error) {
	scope := &flowScope{}
	child := &FlowContext{execution: f.execution, prefix: resolved, scope: scope}
	output, err = invoke(func() (O, error) { return flow.Run(child, input) })
	child.remember(&err) // Includes a handler error even if it launched no tasks.
	scope.wg.Wait()
	f.execution.mu.Lock()
	scope.closed = true
	err = errors.Join(scope.errs...)
	f.execution.mu.Unlock()
	if err != nil {
		var zero O
		return zero, err
	}
	return output, nil
}

// WaitFor registers a durable request in the Flow, outside any Action lease.
// Ignoring its returned error still cannot mark the drive completed.
// WaitFor 在 Flow 中注册一个持久请求，该请求位于任何 Action 租约之外。
// 即使忽略它返回的错误，也无法把本次 drive 标记为 completed。
func (f *FlowContext) WaitFor[Response, Request any](key string, request Request) (output Response, err error) {
	defer f.remember(&err)
	resolved, err := f.claim(key)
	if err != nil {
		return output, err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return output, err
	}
	e := f.execution.engine
	record, err := e.store.Put(f.Context(), &Record{
		Key: resolved, Kind: SignalKind, Status: Suspended, Input: raw, StartedAt: e.options.Now(),
	})
	if err != nil {
		return output, &OperationError{resolved, "Put", err}
	}
	if !bytes.Equal(record.Input, raw) {
		return output, ErrChanged
	}
	if record.Status == Completed {
		err = json.Unmarshal(record.Result, &output)
		return output, err
	}
	if record.Status != Suspended {
		return output, ErrInvalid
	}
	e.observe(f.Context(), resolved, "suspended", record.StartedAt, nil)
	e.emit(f.Context(), Event{
		Type: WaitSuspended, Key: resolved, Kind: SignalKind, Status: Suspended,
		StartedAt: record.StartedAt, RecordVersion: record.Version,
		Pending: []Pending{{Key: resolved, Kind: SignalKind}},
	}, nil)
	return output, &SuspendedError{Pending: Pending{Key: resolved, Kind: SignalKind}}
}

func (f *FlowContext) Sleep(key string, duration time.Duration) (err error) {
	defer f.remember(&err)
	resolved, err := f.claim(key)
	if err != nil {
		return err
	}
	return f.sleep(resolved, f.execution.engine.options.Now().Add(duration))
}

func (f *FlowContext) SleepUntil(key string, until time.Time) (err error) {
	defer f.remember(&err)
	resolved, err := f.claim(key)
	if err != nil {
		return err
	}
	return f.sleep(resolved, until)
}

func (f *FlowContext) sleep(key string, until time.Time) error {
	e := f.execution.engine
	record, err := e.store.Put(f.Context(), &Record{
		Key: key, Kind: SleepKind, Status: Suspended, Until: until, StartedAt: e.options.Now(),
	})
	if err != nil {
		return &OperationError{key, "Put", err}
	}
	if record.Status == Completed {
		return nil
	}
	if record.Status != Suspended {
		return ErrInvalid
	}
	if !e.options.Now().Before(record.Until) {
		var resolved bool
		resolved, err = e.store.Resolve(f.Context(), key, record.Version, nil)
		if err == nil && resolved {
			e.emit(f.Context(), Event{Type: WaitResolved, Key: key, Kind: SleepKind,
				Status: Completed, StartedAt: record.StartedAt, RecordVersion: record.Version + 1,
			}, nil)
		}
		return err
	}
	e.emit(f.Context(), Event{
		Type: WaitSuspended, Key: key, Kind: SleepKind, Status: Suspended,
		StartedAt: record.StartedAt, RecordVersion: record.Version,
		Pending: []Pending{{Key: key, Kind: SleepKind, Until: record.Until}},
	}, nil)
	return &SuspendedError{Pending: Pending{Key: key, Kind: SleepKind, Until: record.Until}}
}

func (f *FlowContext) checkpoint[T any](key, primitive string, generate func() T) (output T, err error) {
	resolved, err := f.claim(key)
	if err != nil {
		return output, err
	}
	e := f.execution.engine
	record, err := e.store.Get(f.Context(), resolved)
	if err != nil {
		return output, err
	}
	tag, _ := json.Marshal(primitive)
	if record == nil {
		raw, marshalErr := json.Marshal(generate())
		if marshalErr != nil {
			return output, marshalErr
		}
		record, err = e.store.Put(f.Context(), &Record{
			Key: resolved, Kind: CheckpointKind, Status: Completed, Input: tag, Result: raw,
			StartedAt: e.options.Now(), EndedAt: e.options.Now(),
		})
		if err != nil {
			return output, err
		}
	}
	if record.Kind != CheckpointKind || !bytes.Equal(record.Input, tag) {
		return output, ErrChanged
	}
	err = json.Unmarshal(record.Result, &output)
	return output, err
}

func (f *FlowContext) Now(key string) (output time.Time, err error) {
	defer f.remember(&err)
	return f.checkpoint(key, "now", f.execution.engine.options.Now)
}

func (f *FlowContext) UUID(key string) (output string, err error) {
	defer f.remember(&err)
	return f.checkpoint(key, "uuid", uuid.NewString)
}

type LoopOptions struct {
	Interval    time.Duration
	MaxAttempts int
	Action      ActionOptions
}

type LoopOutput interface{ Done() bool }

// Loop records EACH attempt as an Action, so a crash after an attempt cannot
// repeat a completed attempt. Its interval is a separate durable sleep.
// Loop 将每一次 attempt 都记录为一个 Action，因此 attempt 之后崩溃不会重复
// 已完成的 attempt。它的间隔是一个独立的持久化 sleep。
func (f *FlowContext) Loop[I any, O LoopOutput](key string, action Action[I, O], input I, options LoopOptions) (output O, err error) {
	defer f.remember(&err)
	resolved, err := f.claim(key)
	if err != nil {
		return output, err
	}
	if options.Interval <= 0 || options.MaxAttempts <= 0 {
		return output, ErrInvalid
	}
	child := &FlowContext{execution: f.execution, prefix: resolved, scope: f.scope}
	for attempt := 0; attempt < options.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err = child.Sleep(fmt.Sprintf("sleep_%d", attempt), options.Interval); err != nil {
				return output, err
			}
		}
		output, err = child.Run(fmt.Sprintf("attempt_%d", attempt), action, input, options.Action)
		if err != nil {
			return output, err
		}
		done, doneErr := invoke(func() (bool, error) { return output.Done(), nil })
		if doneErr != nil {
			return output, doneErr
		}
		if done {
			return output, nil
		}
	}
	return output, Permanent(fmt.Errorf("loop %s exceeded %d attempts", resolved, options.MaxAttempts))
}

// ResolveSignal atomically persists a typed callback and a durable Wake.
// expectedVersion comes from the registered request. A repeated callback
// cannot replace the first response. Missing registration returns ErrNotFound.
// ResolveSignal 原子地持久化类型化回调和持久 Wake。expectedVersion 来自已注册的请求。
// 重复回调无法替换第一次响应。缺少注册时返回 ErrNotFound。
func (e *Engine) ResolveSignal[T any](ctx context.Context, key string, expectedVersion int64, response T) (bool, error) {
	raw, err := json.Marshal(response)
	if err != nil {
		return false, err
	}
	resolved, err := e.store.Resolve(ctx, key, expectedVersion, raw)
	if err == nil && resolved {
		// Resolve's existing interface returns no record kind; do not invent
		// one (custom stores may also accept sleep signals here).
		// Resolve 的现有接口不返回 record kind；不要凭空构造一个。
		// 自定义存储也可能在这里接受 sleep 信号。
		e.emit(ctx, Event{Type: WaitResolved, Key: key,
			Status: Completed, RecordVersion: expectedVersion + 1,
		}, nil)
	}
	return resolved, err
}
