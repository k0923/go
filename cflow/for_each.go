package cflow

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// EachOption configures ForEach and ForItems.
// EachOption 配置 ForEach 与 ForItems。
type EachOption func(*eachOptions)

type eachOptions struct{ concurrency int }

// Concurrency admits up to n items at a time (default 1, sequential). It is a
// sliding window: whenever an admitted item terminates, the next queued item
// is admitted. Only admission is controlled here; how admitted items actually
// run (goroutines, a message queue, ...) is the dispatcher's choice. The window
// is fixed when the batch is first registered; changing it later is accepted
// and has no effect on that batch.
// Concurrency 同时最多激活 n 个元素（默认 1，即顺序执行）。它是滑动窗口：每当一个已激活元素终结，
// 就激活下一个排队元素。这里只控制激活，被激活的元素如何实际运行（goroutine、消息队列等）由调度器决定。
// 窗口在批次首次登记时固定；之后修改不会报错，也不影响该批次。
func Concurrency(n int) EachOption {
	return func(o *eachOptions) { o.concurrency = n }
}

// ForEach registers independently resumable item Flows and joins their results.
//
// Experimental: this API and its storage format are unstable.
// 实验性（不稳定）：API 及持久化格式可能发生不兼容变更。
//
// body must be registered in Options.Flows on
// every worker. key is required, pure, stable, nonempty and unique in this batch.
// The entire ordered input is fixed at registration. Every item's namespace is
// name|item[key(item)]; StepKeyer is not used for this namespace.
// body 必须在每个 worker 的 Options.Flows 中注册。
// key 必填、纯函数、稳定、非空且批内唯一；首次登记固定整批输入和顺序。
// 每项命名空间为 name|item[key(item)]，不通过 StepKeyer 推导。
//
// Items are admitted via durable wake messages within the Concurrency window
// (sequential by default). Until the batch terminates, this call suspends its
// owner; a dispatcher drives each item with Engine.Resume. Item waits resume only
// that item. Completion saves its output, updates the loop counters and admits
// the next item atomically. A failed item stops admission; once the running
// items finish, the owner replays and gets nil plus ForEachError. On success the
// outputs have one entry per input, in input order.
// 元素通过持久唤醒在 Concurrency 窗口内激活（默认顺序）；批次终结前挂起父执行单元。
// 调度器用 Resume 驱动元素，等待仅恢复该元素。完成时原子保存输出、更新计数并激活下一项；
// 失败停止激活，运行中的元素结束后父级回放得到 nil 和 ForEachError。成功时输出与输入一一对应、顺序一致。
//
// Returned values are restored from JSON. Body closures must not capture mutable
// parent state. Small ID arrays are suitable; initial registration and final
// result collection still use O(n) data. Requires FlowInfrastructure.
// 返回值从 JSON 恢复；body 闭包不能捕获可变父级状态。适合 ID 数组；
// 首次登记和最终取结果仍为 O(n)，需要 FlowInfrastructure。
func (f *FlowContext) ForEach[I, O any](name string, body Flow[I, O], items []I, key func(I) string, opts ...EachOption) ([]O, error) {
	child, loop := runLoop(f, name, body, items, key, false, opts)
	if loop.Status == Failed {
		return nil, loopError(loop)
	}
	outputs := make([]O, len(loop.Outputs))
	for i := range loop.Outputs {
		child.must(decode(loop.Outputs[i], &outputs[i]))
	}
	return outputs, nil
}

// ForItems is ForEach without results: every item runs body and its output is
// discarded, so any registered Flow can be reused. Item outputs are not stored
// and the owner never loads them. Experimental, like ForEach.
// ForItems 是没有返回值的 ForEach：每个元素运行 body 后丢弃输出，因此可以复用任意已注册的 Flow。
// 元素输出不会被保存，父级也不会加载它们。与 ForEach 一样为实验性。
func (f *FlowContext) ForItems[I, O any](name string, body Flow[I, O], items []I, key func(I) string, opts ...EachOption) error {
	_, loop := runLoop(f, name, body, items, key, true, opts)
	if loop.Status == Failed {
		return loopError(loop)
	}
	return nil
}

func loopError(loop *LoopSnapshot) error {
	return &ForEachError{Key: loop.FailedKey, Index: loop.FailedIndex, Cause: loop.Failure.Err()}
}

// runLoop validates and registers or joins the batch, and returns it once terminal.
// runLoop 校验并登记或汇合批次，终态后返回。
func runLoop[I, O any](f *FlowContext, name string, body Flow[I, O], items []I, key func(I) string,
	discard bool, opts []EachOption) (*FlowContext, *LoopSnapshot) {
	child := f.Scope(name, "")
	var options eachOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if invalidFlow(body) || key == nil {
		child.fail(fmt.Errorf("%w: ForEach requires a body and key function", ErrInvalid))
	}
	if options.concurrency < 0 {
		child.fail(fmt.Errorf("%w: ForEach concurrency must not be negative", ErrInvalid))
	}
	flowName, err := f.execution.engine.flowName(body)
	child.must(err)
	if f.depth+1 > f.execution.engine.options.MaxFlowDepth {
		child.fail(fmt.Errorf("%w: ForEach exceeds MaxFlowDepth", ErrInvalid))
	}
	api, ok := f.execution.engine.store.(FlowInfrastructure)
	if !ok {
		child.must(ErrFlowUnsupported)
	}
	event, _ := f.Context().Value(eventContextKey{}).(eventContext)
	spec := LoopSpec{Key: child.Key(), Path: child.Path(), ParentKey: f.execution.flowKey, Workflow: event.name,
		FlowName: flowName, Total: len(items), Depth: f.depth + 1, Concurrency: options.concurrency, DiscardOutputs: discard}
	batch := make([]FlowExecution, len(items))
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for i, item := range items {
		id, err := invoke(func() (string, error) { return key(item), nil })
		if err != nil || id == "" {
			child.fail(fmt.Errorf("%w: ForEach key at index %d: %v", ErrInvalid, i, err))
		}
		path, itemKey := child.claim("item", id)
		raw, err := json.Marshal(item)
		child.must(fatal(err))
		batch[i] = FlowExecution{Key: itemKey, LoopKey: spec.Key, Path: path, Workflow: spec.Workflow,
			FlowName: flowName, Index: i, Depth: spec.Depth, Input: raw}
		child.must(fatal(encoder.Encode(struct {
			Key   string
			Input json.RawMessage
		}{itemKey, raw})))
	}
	spec.ItemsHash = fmt.Sprintf("%x", hash.Sum(nil))
	return child, child.loopOutcome(api, spec, batch)
}

// loopOutcome registers or joins the batch and returns it once terminal. A
// terminal loop preloaded with its owner, outputs included, needs no reads.
// loopOutcome 登记或汇合批次，终态后返回。随父级预加载的终态循环已带输出，无需再读存储。
func (f *FlowContext) loopOutcome(api FlowInfrastructure, spec LoopSpec, batch []FlowExecution) *LoopSnapshot {
	loop := f.execution.loops[spec.Key]
	if loop == nil {
		state, err := api.GetLoop(f.Context(), spec.Key)
		f.must(err)
		if state == nil {
			state, err = api.StartLoop(f.Context(), spec, batch)
			f.must(err)
		}
		if state == nil {
			f.fail(ErrChanged)
		}
		loop = &LoopSnapshot{LoopState: *state}
	}
	if !SameLoop(spec, loop.LoopSpec) {
		f.fail(ErrChanged)
	}
	if loop.Status == Suspended {
		f.suspend(Pending{Key: spec.Key, Path: spec.Path, ExecutionKey: spec.ParentKey, Kind: LoopKind})
	}
	if loop.Status != Completed && loop.Status != Failed {
		f.fail(ErrInvalid)
	}
	if loop.Status == Failed {
		if loop.Failure == nil || loop.Failure.Fatal {
			f.fail(fmt.Errorf("%s: %w", loop.FailedKey, loop.Failure.Err()))
		}
		return loop
	}
	if spec.DiscardOutputs {
		return loop
	}
	if f.execution.loops[spec.Key] == nil {
		raw, err := api.LoopResults(f.Context(), spec.Key)
		f.must(err)
		loop.Outputs = raw
	}
	if len(loop.Outputs) != loop.Total {
		f.fail(ErrChanged)
	}
	return loop
}
