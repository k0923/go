package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// 本文件固化「重放相关」的当前行为。每个用例对应一个已知问题，注释里写明期望行为；
// 修复后需要把断言翻转为期望行为，并去掉测试名中的 KnownIssue 后缀。

func waitForEvent(t *testing.T, infrastructure *DebugInfrastructure, key string) Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		event, err := infrastructure.GetEvent(context.Background(), key)
		if err != nil {
			t.Fatalf("get %s failed: %v", key, err)
		}
		if event != nil {
			return event
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event %s never appeared", key)
	return nil
}

// Loop 只在自己的等待间隔到期后才推进：其它 Loop 的 sleep 到期、WaitFor 恢复、
// 或调度器重复驱动，都不得推进本 Loop 的 attempt，也不得绕过间隔。
func TestLoopOnlyAdvancesWhenItsOwnIntervalElapsed(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)
	actionCalls := 0
	handler := func(ctx *WorkflowContext) string {
		return ctx.Loop(
			"poll",
			testAction[struct{}, testLoopOutput](func(actionCtx *ActionContext, input struct{}) testLoopOutput {
				actionCalls++
				return testLoopOutput{DoneValue: false}
			}),
			struct{}{},
			LoopRunOption{Interval: 5 * time.Second, MaxAttempts: 5},
		).Value
	}

	// 首次驱动：进入 attempt 0，未完成，建立 poll|sleep|1 并挂起。
	catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })

	// 间隔未到期：重复驱动三次，都不得推进。
	for drive := 0; drive < 3; drive++ {
		signal := catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })
		if signal.GetKey() != "poll|sleep|1" {
			t.Fatalf("expected the pending poll|sleep|1 signal, got %q", signal.GetKey())
		}
	}

	event, err := infrastructure.GetEvent(context.Background(), "poll")
	if err != nil {
		t.Fatalf("get poll failed: %v", err)
	}
	if loopEvent := event.(*LoopEvent); loopEvent.Attempt != 1 {
		t.Fatalf("expected attempt to stay at 1 before the interval elapses, got %d", loopEvent.Attempt)
	}
	if actionCalls != 1 {
		t.Fatalf("expected loop action to run once, got %d", actionCalls)
	}

	for _, key := range []string{"poll|sleep|2", "poll|sleep|3"} {
		sleep, err := infrastructure.GetEvent(context.Background(), key)
		if err != nil {
			t.Fatalf("get %s failed: %v", key, err)
		}
		if sleep != nil {
			t.Fatalf("expected no orphan sleep event %s, got %+v", key, sleep)
		}
	}
	messages, err := infrastructure.PendingOutbox(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(messages) != 1 || messages[0].Key != "poll|sleep|1" {
		t.Fatalf("expected a single delivery registration for poll|sleep|1, got %+v", messages)
	}

	// 让间隔到期后再驱动：这时才允许推进到 attempt 1。
	sleep := waitForEvent(t, infrastructure, "poll|sleep|1").(*SleepSignalEvent)
	sleep.Until = time.Now().Add(-time.Millisecond)
	if _, err := infrastructure.UpdateEvent(context.Background(), sleep); err != nil {
		t.Fatalf("update loop sleep event failed: %v", err)
	}

	catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })

	event, err = infrastructure.GetEvent(context.Background(), "poll")
	if err != nil {
		t.Fatalf("get poll failed: %v", err)
	}
	if loopEvent := event.(*LoopEvent); loopEvent.Attempt != 2 {
		t.Fatalf("expected attempt to advance to 2 after the interval elapsed, got %d", loopEvent.Attempt)
	}
	if actionCalls != 2 {
		t.Fatalf("expected loop action to run twice, got %d", actionCalls)
	}
}

// 由 WaitFor 恢复触发的驱动，不得推进与之无关的 Loop。
func TestWaitForResumeDoesNotAdvanceLoop(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)
	loopActionCalls := 0

	handler := func(ctx *WorkflowContext) string {
		async := ctx.Go(
			"approval",
			testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
				return actionCtx.WaitFor[string, string](
					"callback",
					WaitForOption[string]{Request: "review"},
				)
			}),
			struct{}{},
		)

		polled := ctx.Loop(
			"poll",
			testAction[struct{}, testLoopOutput](func(actionCtx *ActionContext, input struct{}) testLoopOutput {
				loopActionCalls++
				return testLoopOutput{DoneValue: false}
			}),
			struct{}{},
			LoopRunOption{Interval: 5 * time.Second, MaxAttempts: 5},
		)

		return async.Wait() + polled.Value
	}

	// 首次驱动：Go 分支挂起，Loop 进入 attempt 0 后挂起在 poll|sleep|1。
	catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })
	waitForEvent(t, infrastructure, "approval|callback")

	// 外部解决 WaitFor，触发一次与 Loop 无关的驱动。
	wait := waitForEvent(t, infrastructure, "approval|callback").(*WaitForSignalEvent)
	wait.Response = json.RawMessage(`"approved"`)
	wait.SetStatus(FlowStatusCompleted)
	if _, err := infrastructure.UpdateEvent(context.Background(), wait); err != nil {
		t.Fatalf("update callback failed: %v", err)
	}

	signal := catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })
	if signal.GetKey() != "poll|sleep|1" {
		t.Fatalf("expected the loop to re-suspend on its own pending sleep, got %q", signal.GetKey())
	}

	event, err := infrastructure.GetEvent(context.Background(), "poll")
	if err != nil {
		t.Fatalf("get poll failed: %v", err)
	}
	if loopEvent := event.(*LoopEvent); loopEvent.Attempt != 1 {
		t.Fatalf("expected loop attempt to stay at 1, got %d", loopEvent.Attempt)
	}
	if loopActionCalls != 1 {
		t.Fatalf("expected loop action to run once, got %d", loopActionCalls)
	}
}

// 已知问题 #2：处于 SUSPENDED 的分支在每个驱动上都会被重新 claim 并重新执行 action，
// 一直执行到它自己的挂起点为止（挂起点信号未变，所以这些执行都是白做的）。
// 期望行为：挂起分支的 action 只在首次执行一次。
func TestSuspendedBranchReExecutesOnEveryDrive_KnownIssue(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)
	executions := 0
	handler := func(ctx *WorkflowContext) string {
		return ctx.Go(
			"go_wait",
			testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
				executions++
				return actionCtx.WaitFor[string, string](
					"callback",
					WaitForOption[string]{Request: "review"},
				)
			}),
			struct{}{},
		).Wait()
	}

	catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })
	catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })

	if executions != 2 {
		t.Fatalf("known issue #2: expected action to be re-executed on replay, got %d execution(s)", executions)
	}

	// 幂等性仍然成立：重复提交不会产生第二条投递。
	messages, err := infrastructure.PendingOutbox(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected a single outbox message, got %+v", messages)
	}

	t.Log("known issue #2: 挂起分支每次驱动都重跑 action（结果幂等，代价随驱动次数线性增长）")
}

// 已知问题 #3（使用约束）：Wait() 是驱动中止点。
// Go 分支挂起期间，Wait() 之后的编排完全不会执行——循环、子流程连事件都不会被创建。
// 期望行为（约束而非缺陷）：需要独立推进的挂起点不要放在 Wait() 之后；
// 或者由调度侧按 outbox/信号驱动，而不是依赖驱动返回的信号。
func TestOrchestrationAfterWaitSkippedWhileBranchPending_KnownIssue(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)
	reachedAfterWait := 0
	handler := func(ctx *WorkflowContext) string {
		answer := ctx.Go(
			"go_wait",
			testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
				return actionCtx.WaitFor[string, string](
					"callback",
					WaitForOption[string]{Request: "review"},
				)
			}),
			struct{}{},
		).Wait()

		reachedAfterWait++
		ctx.Loop(
			"poll",
			testAction[struct{}, testLoopOutput](func(actionCtx *ActionContext, input struct{}) testLoopOutput {
				return testLoopOutput{DoneValue: true, Value: "ok"}
			}),
			struct{}{},
			LoopRunOption{Interval: 5 * time.Second, MaxAttempts: 3},
		)
		return answer
	}

	first := catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })
	if _, ok := first.(*WaitForSignalEvent); !ok {
		t.Fatalf("expected wait signal, got %T", first)
	}
	if reachedAfterWait != 0 {
		t.Fatalf("known issue #3: expected orchestration after Wait() to be skipped, ran %d time(s)", reachedAfterWait)
	}
	if event, err := infrastructure.GetEvent(context.Background(), "poll"); err != nil {
		t.Fatalf("get poll failed: %v", err)
	} else if event != nil {
		t.Fatalf("known issue #3: expected no loop event before the branch resolves, got %+v", event)
	}

	// 外部解决信号之后，Wait() 之后的编排才开始执行。
	wait := waitForEvent(t, infrastructure, "go_wait|callback").(*WaitForSignalEvent)
	wait.Response = json.RawMessage(`"approved"`)
	wait.SetStatus(FlowStatusCompleted)
	if _, err := infrastructure.UpdateEvent(context.Background(), wait); err != nil {
		t.Fatalf("update callback failed: %v", err)
	}

	if result := RunFlow(ctx, handler, struct{}{}); result != "approved" {
		t.Fatalf("expected approved after resume, got %q", result)
	}
	if reachedAfterWait != 1 {
		t.Fatalf("expected orchestration to run once after resume, ran %d time(s)", reachedAfterWait)
	}
	if event, err := infrastructure.GetEvent(context.Background(), "poll"); err != nil {
		t.Fatalf("get poll failed: %v", err)
	} else if event == nil {
		t.Fatal("expected loop event after the branch resolves")
	}

	t.Log("known issue #3: Wait() 之前照跑，之后的编排在分支解决前完全跳过")
}

// 已知问题 #4：一次驱动只会抛出 panic 链上「第一个」挂起信号。
// Go 与 Loop 同时挂起时，若 Loop 的 sleep 先中止驱动，Go 分支的 WaitFor 信号
// 不会暴露给驱动者；驱动者只能通过存储/outbox 才能知道 Go 分支在等什么。
// 期望行为：调度以存储/outbox 为准，不依赖驱动返回的信号。
func TestOnlyFirstSuspendSignalEscapesPerDrive_KnownIssue(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)
	handler := func(ctx *WorkflowContext) string {
		ctx.Go(
			"go_wait",
			testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
				return actionCtx.WaitFor[string, string](
					"callback",
					WaitForOption[string]{Request: "review"},
				)
			}),
			struct{}{},
		)
		return ctx.Loop(
			"poll",
			testAction[struct{}, testLoopOutput](func(actionCtx *ActionContext, input struct{}) testLoopOutput {
				return testLoopOutput{DoneValue: false}
			}),
			struct{}{},
			LoopRunOption{Interval: 5 * time.Second, MaxAttempts: 5},
		).Value
	}

	signal := catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })
	if _, ok := signal.(*SleepSignalEvent); !ok {
		t.Fatalf("known issue #4: expected the loop sleep signal to escape, got %T", signal)
	}

	// Go 分支的信号确实落库了，只是没有作为本次驱动的返回值暴露出来。
	wait := waitForEvent(t, infrastructure, "go_wait|callback")
	if _, ok := wait.(*WaitForSignalEvent); !ok {
		t.Fatalf("expected wait event, got %T", wait)
	}
	if wait.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected suspended wait event, got %s", wait.GetStatus())
	}

	t.Log("known issue #4: 驱动只暴露了 Loop 的 sleep 信号，Go 的 WaitFor 信号只能从存储侧看到")
}
