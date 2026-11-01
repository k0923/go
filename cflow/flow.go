package cflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// RunFlow starts or joins an independently persisted registered Flow at
// name[input.StepKey()] (or name). An empty name uses the registered type name.
// While the child runs, the caller suspends; Resume(key) resumes only the child.
// RunFlow 在 name[input.StepKey()]（或 name）启动或汇合独立持久化的已注册 Flow。
// name 为空时使用注册类型名；子级运行期间父级挂起，Resume(key) 只恢复子级。
func (f *FlowContext) RunFlow[I, O any](name string, flow Flow[I, O], input I) (O, error) {
	if name == "" {
		var err error
		name, err = f.execution.engine.flowName(flow)
		f.must(err)
	}
	path, _ := f.claimInput(name, input)
	if invalidFlow(flow) {
		f.abort(fatal(ErrInvalid))
	}
	return f.callFlow(path, flow, input)
}

// callFlow joins a registered, durable child. A terminal child preloaded with
// the parent's boundary is replayed directly from its own execution, without
// StartFlow; its identity is still checked against the current call.
// callFlow 汇合已注册的持久子级。随父级预加载的终态子级直接从其自身执行回放，不调用 StartFlow；
// 仍按当前调用校验其身份。
func (f *FlowContext) callFlow[I, O any](path Path, flow Flow[I, O], input I) (output O, err error) {
	e := f.execution.engine
	if f.depth+1 > e.options.MaxFlowDepth {
		f.fail(fmt.Errorf("%w: flow %s exceeds MaxFlowDepth", ErrInvalid, path))
	}
	name, err := e.flowName(flow)
	f.must(err)
	api, ok := e.store.(FlowInfrastructure)
	if !ok {
		f.must(ErrFlowUnsupported)
	}
	raw, err := json.Marshal(input)
	f.must(fatal(err))
	event, _ := f.Context().Value(eventContextKey{}).(eventContext)
	spec := FlowExecution{Key: e.buildKey(path), Path: path, ParentKey: f.execution.flowKey,
		Workflow: event.name, FlowName: name, Depth: f.depth + 1, Input: raw}
	item := f.execution.flows[spec.Key]
	if item == nil {
		item, err = api.StartFlow(f.Context(), spec)
		f.must(err)
	}
	if item == nil || !SameFlow(spec, *item) {
		f.fail(ErrChanged)
	}
	switch item.Status {
	case Completed:
		f.must(decode(item.Output, &output))
		return output, nil
	case Failed:
		if item.Failure == nil || item.Failure.Fatal {
			f.fail(fmt.Errorf("%s: %w", item.Key, item.Failure.Err()))
		}
		return output, item.Failure.Err()
	case Running:
		f.suspend(Pending{Key: item.Key, Path: path, ExecutionKey: spec.ParentKey, Kind: FlowKind})
	default:
		f.fail(ErrInvalid)
	}
	panic("unreachable")
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
			Key: resolved, Path: path, ExecutionKey: f.execution.flowKey, Kind: SignalKind, Status: Suspended, Type: typ, Input: raw, StartedAt: e.options.Now(),
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
	pending := Pending{Key: resolved, Path: path, ExecutionKey: f.execution.flowKey, Kind: SignalKind}
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
// WaitAny group, or Poll's own sleep_N).
// Sleep 没有可推导身份的输入：名字固定，唯一性来自外层命名空间
// （Scope、带 key 的 Flow、WaitAny 组，或 Poll 自带的 sleep_N）。
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
			Key: key, Path: path, ExecutionKey: f.execution.flowKey, Kind: SleepKind, Status: Suspended, Type: "sleep", Until: until, StartedAt: e.options.Now(),
		}); err != nil {
			f.must(&OperationError{key, "Put", err})
		}
	}
	if !sameStep(record, SleepKind, path) || record.ExecutionKey != f.execution.flowKey {
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
	pending := Pending{Key: key, Path: path, ExecutionKey: f.execution.flowKey, Kind: SleepKind, Until: record.Until}
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
			Key: resolved, Path: path, ExecutionKey: f.execution.flowKey, Kind: CheckpointKind, Status: Completed, Type: typ, Input: tag, Result: raw,
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

type PollOptions struct {
	Interval    time.Duration
	MaxAttempts int
	Action      ActionOptions
}

type PollOutput interface{ Done() bool }

// Poll runs action until its output is Done. It records EACH attempt as an
// Action, so a crash after an attempt cannot
// repeat a completed attempt. Its interval is a separate durable sleep.
// err is nil, an attempt's *ActionError, or ErrPollExhausted (wrapped).
// Poll 反复执行 action 直到输出 Done。每一次 attempt 都记录为一个 Action，因此 attempt 之后崩溃不会重复
// 已完成的 attempt。它的间隔是一个独立的持久化 sleep。
// err 只会是 nil、某次 attempt 的 *ActionError，或包装后的 ErrPollExhausted。
func (f *FlowContext) Poll[I any, O PollOutput](name string, action Action[I, O], input I, options PollOptions) (output O, err error) {
	path, resolved := f.claimInput(name, input)
	if options.Interval <= 0 || options.MaxAttempts <= 0 {
		f.must(ErrInvalid)
	}
	child := &FlowContext{execution: f.execution, path: path, depth: f.depth, strand: f.strand}
	for attempt := 0; attempt < options.MaxAttempts; attempt++ {
		if attempt > 0 {
			child.Sleep(fmt.Sprintf("sleep_%d", attempt), options.Interval)
		}
		// Attempts are keyed by their own name, not by input.StepKey(): the
		// loop namespace already carries it.
		// 每次尝试按自身名字寻址，而不是 input.StepKey()：Poll 命名空间已带上它。
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
	return output, fmt.Errorf("%w: %s after %d attempts", ErrPollExhausted, resolved, options.MaxAttempts)
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
