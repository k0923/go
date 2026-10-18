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

// RunFlow mounts a Flow under name[input.StepKey()] (or name) on the current
// strand. It creates a namespace, not another lease or workflow state.
// Returning includes joining the Flow's async Actions. err is whatever the
// Flow returned; a waiting Flow halts the caller instead of returning.
// Nesting deeper than Options.MaxFlowDepth fails the drive with ErrInvalid.
// RunFlow 在当前 strand 上把 Flow 挂载到 name[input.StepKey()]（或 name）之下。它创建的是
// 命名空间，而不是另一个租约或工作流状态。返回前会汇合该 Flow 的异步 Action。err 即 Flow
// 返回的错误；处于等待中的 Flow 会停止调用方，而不是返回。嵌套超过 Options.MaxFlowDepth
// 时 drive 以 ErrInvalid 失败。
func (f *FlowContext) RunFlow[I, O any](name string, flow Flow[I, O], input I) (O, error) {
	path, _ := f.claimInput(name, input)
	if invalidFlow(flow) {
		f.abort(fatal(ErrInvalid))
	}
	return f.runChildFlow(path, flow, input)
}

// runChildFlow runs a Flow on the CURRENT strand and joins its async tasks.
// The workflow root (path == nil) adds no namespace and depth.
// runChildFlow 在当前 strand 上运行 Flow，并汇合其异步任务。
// 工作流根（path == nil）不追加命名空间，也不增加深度。
func (f *FlowContext) runChildFlow[I, O any](path Path, flow Flow[I, O], input I) (output O, err error) {
	depth := f.depth
	if path != nil {
		depth++
	}
	if depth > f.execution.engine.options.MaxFlowDepth {
		f.abort(fatal(fmt.Errorf("%w: flow %s exceeds MaxFlowDepth %d", ErrInvalid, path, f.execution.engine.options.MaxFlowDepth)))
	}
	scope := &flowScope{}
	child := &FlowContext{execution: f.execution, path: path, depth: depth, scope: scope, strand: f.strand}
	if path == nil {
		child.path = f.path
	}
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
// The address is name[request.StepKey()] when the request implements StepKeyer.
// WaitFor 在 Flow 中注册一个持久请求，该请求位于任何 Action 租约之外。
// 在响应被解析前，它会停止调用方编排，drive 随后报告 Suspended。
// 之后的 drive 中，它返回已持久化的响应。请求实现 StepKeyer 时地址为 name[request.StepKey()]。
func (f *FlowContext) WaitFor[Response, Request any](name string, request Request) Response {
	path, resolved := f.claimInput(name, request)
	raw, err := json.Marshal(request)
	f.must(fatal(err))
	typ := signature[Request, Response]("signal")
	e := f.execution.engine
	record := f.execution.records[resolved]
	if record == nil {
		if record, err = e.store.Put(f.Context(), &Record{
			Key: resolved, Path: path, Kind: SignalKind, Status: Suspended, Type: typ, Input: raw, StartedAt: e.options.Now(),
		}); err != nil {
			f.must(&OperationError{resolved, "Put", err})
		}
	}
	if !sameStep(record, SignalKind, path) {
		f.must(ErrChanged)
	}
	// A changed request is not re-sent: the registered one is still answered.
	// 变化后的请求不会重新发送：仍以已注册的请求等待回答。
	f.replayed(record, typ, raw)
	if record.Status == Completed {
		var output Response
		f.must(decode(record.Result, &output))
		return output
	}
	if record.Status != Suspended {
		f.must(ErrInvalid)
	}
	pending := Pending{Key: resolved, Path: path, Kind: SignalKind}
	e.emit(f.Context(), Event{
		Type: WaitSuspended, Key: resolved, Path: path, Kind: SignalKind, Status: Suspended, StepType: typ,
		StartedAt: record.StartedAt, RecordVersion: record.Version, Pending: []Pending{pending},
	}, nil)
	f.suspend(pending)
	panic("unreachable")
}

// Sleep durably waits for duration; the deadline is fixed on first registration.
// Sleep 持久地等待 duration；截止时间在首次注册时固定。
// A Sleep has no input to derive an identity from: its name is fixed and its
// uniqueness comes from the enclosing namespace (a Scope, a keyed Flow, a
// WaitAny group, or Loop's own sleep_N).
// Sleep 没有可推导身份的输入：名字固定，唯一性来自外层命名空间
// （Scope、带 key 的 Flow、WaitAny 组，或 Loop 自带的 sleep_N）。
func (f *FlowContext) Sleep(name string, duration time.Duration) {
	path, key := f.claim(name, "")
	f.sleep(path, key, f.execution.engine.options.Now().Add(duration))
}

func (f *FlowContext) SleepUntil(name string, until time.Time) {
	path, key := f.claim(name, "")
	f.sleep(path, key, until)
}

func (f *FlowContext) sleep(path Path, key string, until time.Time) {
	e := f.execution.engine
	record := f.execution.records[key]
	if record == nil {
		var err error
		if record, err = e.store.Put(f.Context(), &Record{
			Key: key, Path: path, Kind: SleepKind, Status: Suspended, Type: "sleep", Until: until, StartedAt: e.options.Now(),
		}); err != nil {
			f.must(&OperationError{key, "Put", err})
		}
	}
	if !sameStep(record, SleepKind, path) {
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
			e.emit(f.Context(), Event{Type: WaitResolved, Key: key, Path: path, Kind: SleepKind, StepType: "sleep",
				Status: Completed, StartedAt: record.StartedAt, RecordVersion: record.Version + 1,
			}, nil)
		}
		return
	}
	pending := Pending{Key: key, Path: path, Kind: SleepKind, Until: record.Until}
	e.emit(f.Context(), Event{
		Type: WaitSuspended, Key: key, Path: path, Kind: SleepKind, Status: Suspended, StepType: "sleep",
		StartedAt: record.StartedAt, RecordVersion: record.Version, Pending: []Pending{pending},
	}, nil)
	f.suspend(pending)
}

func (f *FlowContext) checkpoint[T any](name, primitive string, generate func() T) T {
	path, resolved := f.claim(name, "")
	e := f.execution.engine
	record := f.execution.records[resolved]
	var err error
	if record == nil {
		if record, err = e.store.Get(f.Context(), resolved); err != nil {
			f.must(&OperationError{resolved, "Get", err})
		}
	}
	tag, _ := json.Marshal(primitive)
	typ := primitive + "[" + reflect.TypeFor[T]().String() + "]"
	if record == nil {
		raw, marshalErr := json.Marshal(generate())
		f.must(fatal(marshalErr))
		record, err = e.store.Put(f.Context(), &Record{
			Key: resolved, Path: path, Kind: CheckpointKind, Status: Completed, Type: typ, Input: tag, Result: raw,
			StartedAt: e.options.Now(), EndedAt: e.options.Now(),
		})
		if err != nil {
			f.must(&OperationError{resolved, "Put", err})
		}
	}
	// Now and UUID share CheckpointKind; the primitive tag tells them apart.
	// Now 与 UUID 共用 CheckpointKind，由原语标签区分。
	if !sameStep(record, CheckpointKind, path) || !sameJSON(record.Input, tag) {
		f.must(ErrChanged)
	}
	f.replayed(record, typ, tag)
	var output T
	f.must(decode(record.Result, &output))
	return output
}

// Now returns a durable timestamp: the first drive's value on every replay.
// Now 返回持久化时间戳：每次回放都返回首次 drive 的值。
func (f *FlowContext) Now(name string) time.Time {
	return f.checkpoint(name, "now", f.execution.engine.options.Now)
}

// UUID returns a durable random identifier.
// UUID 返回持久化的随机标识。
func (f *FlowContext) UUID(name string) string {
	return f.checkpoint(name, "uuid", uuid.NewString)
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
func (f *FlowContext) Loop[I any, O LoopOutput](name string, action Action[I, O], input I, options LoopOptions) (output O, err error) {
	path, resolved := f.claimInput(name, input)
	if options.Interval <= 0 || options.MaxAttempts <= 0 {
		f.must(ErrInvalid)
	}
	child := &FlowContext{execution: f.execution, path: path, depth: f.depth, scope: f.scope, strand: f.strand}
	for attempt := 0; attempt < options.MaxAttempts; attempt++ {
		if attempt > 0 {
			child.Sleep(fmt.Sprintf("sleep_%d", attempt), options.Interval)
		}
		// Attempts are keyed by their own name, not by input.StepKey(): the
		// loop namespace already carries it.
		// 每次尝试按自身名字寻址，而不是 input.StepKey()：Loop 命名空间已带上它。
		attemptPath, attemptKey := child.claim(fmt.Sprintf("attempt_%d", attempt), "")
		var business *ActionError
		var runErr error
		output, business, runErr = child.runAction(attemptPath, attemptKey, action, input, options.Action)
		child.must(runErr)
		if business != nil {
			return output, business
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
