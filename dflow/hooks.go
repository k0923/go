package dflow

import (
	"context"
	"time"
)

// Hooks observes node lifecycle transitions for metrics, tracing and logging.
// Every field is optional; a nil hook is skipped. Hooks must not block for long,
// must treat all arguments as read-only, and must never panic: a panicking hook
// is recovered and ignored so observation can never corrupt execution.
//
// Hooks are a side channel. They never influence scheduling, commit or retry
// decisions, and the engine ignores any error a hook might imply.
//
// Hooks 观测节点生命周期转换，用于 metrics、trace 和日志。所有字段都是可选的；
// nil hook 会被跳过。Hooks 不能长时间阻塞，必须将所有参数视为只读，并且绝不能
// panic：发生 panic 的 hook 会被 recover 并忽略，因此观测逻辑永远不会破坏执行。
//
// Hooks 是旁路通道。它们绝不影响调度、commit 或 retry 决策，引擎也会忽略 hook
// 可能暗示的任何错误。
type Hooks struct {
	// OnClaim fires after a node is claimed and marked Running, before input
	// resolution. Attempt carries the node lease generation, which advances on
	// each claim, so a rising Attempt for one node indicates retries/takeovers.
	//
	// OnClaim 在节点被 claim 并标记为 Running 后、输入解析前触发。Attempt 携带
	// 节点租约 generation，每次 claim 都会递增，因此同一节点的 Attempt 增长表示
	// 发生了重试或接管。
	OnClaim func(context.Context, NodeEvent)
	// OnStart fires immediately before Run or Resume is invoked. Resume reports
	// IsResume true and carries the suspension token.
	//
	// OnStart 在调用 Run 或 Resume 前立即触发。Resume 会报告 IsResume true，并携带
	// suspension token。
	OnStart func(context.Context, NodeEvent)
	// OnFinish fires once per invoked Run/Resume, after panic recovery, timeout
	// and result validation, before Commit. Outcome classifies the validated
	// result; Err is set for Failed. Duration measures Run/Resume only. This is
	// not a durable-commit event. Skipped/input-resolution failures don't invoke
	// Run/Resume and therefore produce no OnStart/OnFinish pair.
	//
	// OnFinish 对每次实际调用的 Run/Resume 触发一次，发生在 panic recovery、超时和
	// 结果校验之后、Commit 之前。Outcome 对已校验结果分类；Failed 时会设置 Err。
	// Duration 只统计 Run/Resume。本事件不是持久化 commit 事件。Skipped 或输入解析
	// 失败不会调用 Run/Resume，因此不会产生 OnStart/OnFinish 对。
	OnFinish func(context.Context, NodeEvent)
}

// NodeEvent carries read-only node lifecycle data. Fields that do not apply to a
// given hook are zero (for example Duration/Outcome are unset before OnFinish).
//
// NodeEvent 携带只读的节点生命周期数据。不适用于某个 hook 的字段为零值
// （例如 OnFinish 前 Duration/Outcome 尚未设置）。
type NodeEvent struct {
	NodeID   string
	Attempt  int64
	IsResume bool
	Token    string
	Duration time.Duration
	Outcome  Status
	Err      error
}

func (h Hooks) claim(ctx context.Context, event NodeEvent) { safeHook(ctx, h.OnClaim, event) }
func (h Hooks) start(ctx context.Context, event NodeEvent) { safeHook(ctx, h.OnStart, event) }
func (h Hooks) finish(ctx context.Context, event NodeEvent) {
	safeHook(ctx, h.OnFinish, event)
}

func safeHook(ctx context.Context, fn func(context.Context, NodeEvent), event NodeEvent) {
	if fn == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(ctx, event)
}
