package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// RunFlow is available only to top-level orchestration. It creates a namespace,
// not another lease or workflow state. A called Flow cannot invoke another Flow.
// Returning includes joining the Flow's async Actions. err is whatever the
// Flow returned; a waiting Flow halts the caller instead of returning.
// RunFlow 仅允许顶层编排调用；被调用的 Flow 不可继续调用 Flow。
// 返回前会汇合该 Flow 的异步 Action。err 即 Flow 返回的错误；
// 处于等待中的 Flow 会停止调用方，而不是返回。
func (w *WorkflowContext) RunFlow[I, O any](key string, flow Flow[I, O], input I) (O, error) {
	f := w.state
	resolved := f.claim(key)
	if flow == nil {
		f.abort(fatal(ErrInvalid))
	}
	return f.runChildFlow(resolved, flow, input)
}

// runChildFlow runs a Flow on the CURRENT strand and joins its async tasks.
// A mounted Flow adds a "flow[I,O]" segment to the type path, so changing its
// I/O signature is drift even when every inner step keeps its own types. The
// root of NewWorkflowFromFlow (resolved == "") adds no segment, like no key.
// runChildFlow 在当前 strand 上运行 Flow，并汇合其异步任务。挂载的 Flow 会向类型路径
// 追加 "flow[I,O]" 段，因此即使内部每个步骤类型不变，改变 Flow 的 I/O 签名也属于漂移。
// NewWorkflowFromFlow 的根（resolved == ""）与不加 key 一样，不追加类型段。
func (f *FlowContext) runChildFlow[I, O any](resolved string, flow Flow[I, O], input I) (output O, err error) {
	scope := &flowScope{}
	types := f.types
	if resolved != "" {
		types = f.typePath(signature[I, O]("flow"))
	}
	child := &FlowContext{execution: f.execution, prefix: resolved, types: types, scope: scope, strand: f.strand}
	defer func() {
		// Join descendants even while unwinding, so Drive never returns early.
		// 即使在展开过程中也要汇合后代，确保 Drive 不会提前返回。
		scope.wg.Wait()
		f.execution.mu.Lock()
		scope.closed = true
		f.execution.mu.Unlock()
	}()
	output, err = flow.Run(child, input)
	scope.wg.Wait()
	if f.execution.ctx.Err() != nil {
		// A joined task aborted the drive; do not hand partial results onward.
		// 已汇合的任务中止了 drive；不要把部分结果继续向后传递。
		f.halt()
	}
	return output, err
}

// WaitFor registers a durable request in the Flow, outside any Action lease.
// Until a response is resolved it halts the calling orchestration; the drive
// then reports Suspended. On a later drive it returns the persisted response.
// WaitFor 在 Flow 中注册一个持久请求，该请求位于任何 Action 租约之外。
// 在响应被解析前，它会停止调用方编排，drive 随后报告 Suspended。
// 之后的 drive 中，它返回已持久化的响应。
func (f *FlowContext) WaitFor[Response, Request any](key string, request Request) Response {
	resolved := f.claim(key)
	raw, err := json.Marshal(request)
	f.must(fatal(err))
	typ := f.typePath(signature[Request, Response]("signal"))
	e := f.execution.engine
	record := f.execution.records[resolved]
	if record == nil {
		if record, err = e.store.Put(f.Context(), &Record{
			Key: resolved, Kind: SignalKind, Status: Suspended, Type: typ, Input: raw, StartedAt: e.options.Now(),
		}); err != nil {
			f.must(&OperationError{resolved, "Put", err})
		}
	}
	if !sameStep(record, SignalKind, typ, raw) {
		f.must(ErrChanged)
	}
	// A changed request is not re-sent: the registered one is still answered.
	// 变化后的请求不会重新发送：仍以已注册的请求等待回答。
	f.inputChanged(record, raw)
	if record.Status == Completed {
		var output Response
		f.must(decode(record.Result, &output))
		return output
	}
	if record.Status != Suspended {
		f.must(ErrInvalid)
	}
	e.emit(f.Context(), Event{
		Type: WaitSuspended, Key: resolved, Kind: SignalKind, Status: Suspended,
		StartedAt: record.StartedAt, RecordVersion: record.Version,
		Pending: []Pending{{Key: resolved, Kind: SignalKind}},
	}, nil)
	f.suspend(Pending{Key: resolved, Kind: SignalKind})
	panic("unreachable")
}

// Sleep durably waits for duration; the deadline is fixed on first registration.
// Sleep 持久地等待 duration；截止时间在首次注册时固定。
func (f *FlowContext) Sleep(key string, duration time.Duration) {
	resolved := f.claim(key)
	f.sleep(resolved, f.execution.engine.options.Now().Add(duration))
}

func (f *FlowContext) SleepUntil(key string, until time.Time) {
	f.sleep(f.claim(key), until)
}

func (f *FlowContext) sleep(key string, until time.Time) {
	e := f.execution.engine
	typ := f.typePath("sleep")
	record := f.execution.records[key]
	if record == nil {
		var err error
		if record, err = e.store.Put(f.Context(), &Record{
			Key: key, Kind: SleepKind, Status: Suspended, Type: typ, Until: until, StartedAt: e.options.Now(),
		}); err != nil {
			f.must(&OperationError{key, "Put", err})
		}
	}
	if !sameStep(record, SleepKind, typ, nil) {
		f.must(ErrChanged)
	}
	if record.Status == Completed {
		return
	}
	if record.Status != Suspended {
		f.must(ErrInvalid)
	}
	if !e.options.Now().Before(record.Until) {
		resolved, err := e.store.Resolve(f.Context(), key, record.Version, nil)
		if err != nil {
			f.must(&OperationError{key, "Resolve", err})
		}
		if resolved {
			e.emit(f.Context(), Event{Type: WaitResolved, Key: key, Kind: SleepKind,
				Status: Completed, StartedAt: record.StartedAt, RecordVersion: record.Version + 1,
			}, nil)
		}
		return
	}
	e.emit(f.Context(), Event{
		Type: WaitSuspended, Key: key, Kind: SleepKind, Status: Suspended,
		StartedAt: record.StartedAt, RecordVersion: record.Version,
		Pending: []Pending{{Key: key, Kind: SleepKind, Until: record.Until}},
	}, nil)
	f.suspend(Pending{Key: key, Kind: SleepKind, Until: record.Until})
}

func (f *FlowContext) checkpoint[T any](key, primitive string, generate func() T) T {
	resolved := f.claim(key)
	e := f.execution.engine
	record := f.execution.records[resolved]
	var err error
	if record == nil {
		if record, err = e.store.Get(f.Context(), resolved); err != nil {
			f.must(&OperationError{resolved, "Get", err})
		}
	}
	tag, _ := json.Marshal(primitive)
	typ := f.typePath(primitive + "[" + reflect.TypeFor[T]().String() + "]")
	if record == nil {
		raw, marshalErr := json.Marshal(generate())
		f.must(fatal(marshalErr))
		record, err = e.store.Put(f.Context(), &Record{
			Key: resolved, Kind: CheckpointKind, Status: Completed, Type: typ, Input: tag, Result: raw,
			StartedAt: e.options.Now(), EndedAt: e.options.Now(),
		})
		if err != nil {
			f.must(&OperationError{resolved, "Put", err})
		}
	}
	if !sameStep(record, CheckpointKind, typ, tag) {
		f.must(ErrChanged)
	}
	var output T
	f.must(decode(record.Result, &output))
	return output
}

// Now returns a durable timestamp: the first drive's value on every replay.
// Now 返回持久化时间戳：每次回放都返回首次 drive 的值。
func (f *FlowContext) Now(key string) time.Time {
	return f.checkpoint(key, "now", f.execution.engine.options.Now)
}

// UUID returns a durable random identifier.
// UUID 返回持久化的随机标识。
func (f *FlowContext) UUID(key string) string {
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
// err is nil, an attempt's *ActionError, or ErrLoopExhausted (wrapped).
// Loop 将每一次 attempt 都记录为一个 Action，因此 attempt 之后崩溃不会重复
// 已完成的 attempt。它的间隔是一个独立的持久化 sleep。
// err 只会是 nil、某次 attempt 的 *ActionError，或包装后的 ErrLoopExhausted。
func (f *FlowContext) Loop[I any, O LoopOutput](key string, action Action[I, O], input I, options LoopOptions) (output O, err error) {
	resolved := f.claim(key)
	if options.Interval <= 0 || options.MaxAttempts <= 0 {
		f.must(ErrInvalid)
	}
	child := &FlowContext{execution: f.execution, prefix: resolved, types: f.typePath(signature[I, O]("loop")),
		scope: f.scope, strand: f.strand}
	for attempt := 0; attempt < options.MaxAttempts; attempt++ {
		if attempt > 0 {
			child.Sleep(fmt.Sprintf("sleep_%d", attempt), options.Interval)
		}
		output, err = child.Run(fmt.Sprintf("attempt_%d", attempt), action, input, options.Action)
		if err != nil {
			return output, err
		}
		// Done is user code on replayed data; a panic here is a definition bug.
		// Done 是作用于回放数据的用户代码；这里的 panic 属于定义缺陷。
		if output.Done() {
			return output, nil
		}
	}
	return output, fmt.Errorf("%w: %s after %d attempts", ErrLoopExhausted, resolved, options.MaxAttempts)
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

// Signal resolves a registered WaitFor without reading its version first; the
// store's own version check still runs. It is idempotent: a repeated or late
// callback returns false. Use ResolveSignal when you hold a version and want a
// stale-request check.
// Signal 无需先读取版本即可解析已注册的 WaitFor；存储自身的版本校验仍会执行。
// 它是幂等的：重复或迟到的回调返回 false。若持有版本并希望校验过期请求，请用 ResolveSignal。
func (e *Engine) Signal[T any](ctx context.Context, key string, response T) (bool, error) {
	for {
		record, err := e.store.Get(ctx, key)
		if err != nil {
			return false, err
		}
		if record == nil {
			return false, ErrNotFound
		}
		if record.Kind != SignalKind {
			return false, ErrInvalid
		}
		if record.Status != Suspended {
			return false, nil
		}
		resolved, err := e.ResolveSignal(ctx, key, record.Version, response)
		if errors.Is(err, ErrConflict) && ctx.Err() == nil {
			continue // concurrently advanced; re-read and let the store decide
		}
		return resolved, err
	}
}
