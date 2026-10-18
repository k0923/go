package cflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// strictInfrastructure 断言信号事件只能经 Suspend 持久化，
// 绝不能被运行时的 AddEvent / UpdateEvent 直接写入。
type strictInfrastructure struct {
	*DebugInfrastructure

	mu         sync.Mutex
	violations []string
}

var _ Infrastructure = (*strictInfrastructure)(nil)

func newStrictInfrastructure() *strictInfrastructure {
	return &strictInfrastructure{DebugInfrastructure: NewDebugInfrastructure()}
}

func (infrastructure *strictInfrastructure) record(format string, args ...any) {
	infrastructure.mu.Lock()
	infrastructure.violations = append(infrastructure.violations, fmt.Sprintf(format, args...))
	infrastructure.mu.Unlock()
}

func (infrastructure *strictInfrastructure) Violations() []string {
	infrastructure.mu.Lock()
	defer infrastructure.mu.Unlock()
	return append([]string(nil), infrastructure.violations...)
}

func (infrastructure *strictInfrastructure) AddEvent(ctx context.Context, event Event) (Event, error) {
	if _, ok := event.(Signal); ok {
		infrastructure.record("signal %s must be persisted via Suspend, not AddEvent", event.GetKey())
	}
	return infrastructure.DebugInfrastructure.AddEvent(ctx, event)
}

func (infrastructure *strictInfrastructure) UpdateEvent(ctx context.Context, event Event) (Event, error) {
	if _, ok := event.(Signal); ok {
		infrastructure.record("signal %s must be persisted via Suspend, not UpdateEvent", event.GetKey())
	}
	return infrastructure.DebugInfrastructure.UpdateEvent(ctx, event)
}

func waitForHandler() func(ctx *WorkflowContext) string {
	return func(ctx *WorkflowContext) string {
		return ctx.Run(
			"approval",
			testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
				return actionCtx.WaitFor[string, string](
					"callback",
					WaitForOption[string]{Request: "review"},
				)
			}),
			struct{}{},
			ActionRunOption{},
		)
	}
}

func TestSuspendCommitsAtomically(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)

	signal := catchSignal(t, func() { RunFlow(ctx, waitForHandler(), struct{}{}) })
	if signal.GetKey() != "approval|callback" {
		t.Fatalf("expected callback signal, got %q", signal.GetKey())
	}

	step, err := infrastructure.GetEvent(context.Background(), "approval")
	if err != nil {
		t.Fatalf("get step failed: %v", err)
	}
	if step.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected step suspended, got %s", step.GetStatus())
	}

	wait, err := infrastructure.GetEvent(context.Background(), "approval|callback")
	if err != nil {
		t.Fatalf("get callback failed: %v", err)
	}
	if wait == nil || wait.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected visible suspended signal, got %+v", wait)
	}

	lease, err := infrastructure.ClaimLease(context.Background(), "approval", time.Minute)
	if err != nil {
		t.Fatalf("claim lease failed: %v", err)
	}
	if lease == nil {
		t.Fatal("expected lease to be released by the suspend commit")
	}

	messages, err := infrastructure.PendingOutbox(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(messages) != 1 || messages[0].SignalKey != "approval|callback" {
		t.Fatalf("expected one outbox message for the signal, got %+v", messages)
	}
}

// TestSignalVisibleOnlyAfterUnitResumable 在信号可见的瞬间断言：
// 执行单元必须已经是 SUSPENDED，且租约已经释放。若挂起不是原子的，
// 这里会观察到"信号可见但租约仍被持有"。
func TestSignalVisibleOnlyAfterUnitResumable(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)

	type observation struct {
		err       error
		status    FlowStatus
		claimable bool
	}
	observed := make(chan observation, 1)
	go func() {
		for {
			signal, err := infrastructure.GetEvent(context.Background(), "approval|callback")
			if err != nil {
				observed <- observation{err: err}
				return
			}
			if signal == nil {
				time.Sleep(time.Millisecond)
				continue
			}
			step, err := infrastructure.GetEvent(context.Background(), "approval")
			if err != nil {
				observed <- observation{err: err}
				return
			}
			lease, err := infrastructure.ClaimLease(context.Background(), "approval", time.Minute)
			if err != nil {
				observed <- observation{err: err}
				return
			}
			observed <- observation{status: step.GetStatus(), claimable: lease != nil}
			return
		}
	}()

	catchSignal(t, func() { RunFlow(ctx, waitForHandler(), struct{}{}) })

	select {
	case got := <-observed:
		if got.err != nil {
			t.Fatalf("observation failed: %v", got.err)
		}
		if got.status != FlowStatusSuspended {
			t.Fatalf("signal became visible while unit was %s", got.status)
		}
		if !got.claimable {
			t.Fatal("signal became visible while lease was still held")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("signal never became visible")
	}
}

func TestSignalsArePersistedOnlyThroughSuspend(t *testing.T) {
	t.Run("wait for", func(t *testing.T) {
		infrastructure := newStrictInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		catchSignal(t, func() { RunFlow(ctx, waitForHandler(), struct{}{}) })
		if violations := infrastructure.Violations(); len(violations) != 0 {
			t.Fatalf("unexpected signal writes: %v", violations)
		}
	})

	t.Run("sleep", func(t *testing.T) {
		infrastructure := newStrictInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		handler := func(ctx *WorkflowContext) string {
			return ctx.Run(
				"nap",
				testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
					actionCtx.Sleep("backoff", time.Hour)
					return "done"
				}),
				struct{}{},
				ActionRunOption{},
			)
		}
		catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })
		if violations := infrastructure.Violations(); len(violations) != 0 {
			t.Fatalf("unexpected signal writes: %v", violations)
		}
	})

	t.Run("loop", func(t *testing.T) {
		infrastructure := newStrictInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		handler := func(ctx *WorkflowContext) string {
			return ctx.Loop(
				"poll",
				testAction[struct{}, testLoopOutput](func(actionCtx *ActionContext, input struct{}) testLoopOutput {
					return testLoopOutput{Value: "pending", DoneValue: false}
				}),
				struct{}{},
				LoopRunOption{Interval: 5 * time.Second, MaxAttempts: 3},
			).Value
		}
		catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })
		if violations := infrastructure.Violations(); len(violations) != 0 {
			t.Fatalf("unexpected signal writes: %v", violations)
		}
	})
}

// TestDuplicateDriveWhilePendingDoesNotFail 覆盖 P0-1b：信号尚未被外部解决时
// 重复驱动，绝不能把执行单元打成 FAILED。
func TestDuplicateDriveWhilePendingDoesNotFail(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)

	catchSignal(t, func() { RunFlow(ctx, waitForHandler(), struct{}{}) })
	catchSignal(t, func() { RunFlow(ctx, waitForHandler(), struct{}{}) })

	step, err := infrastructure.GetEvent(context.Background(), "approval")
	if err != nil {
		t.Fatalf("get step failed: %v", err)
	}
	if step.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected step to stay SUSPENDED, got %s (%s)", step.GetStatus(), step.GetError())
	}

	wait, err := infrastructure.GetEvent(context.Background(), "approval|callback")
	if err != nil {
		t.Fatalf("get callback failed: %v", err)
	}
	if wait.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected signal to stay SUSPENDED, got %s", wait.GetStatus())
	}

	callback := wait.(*WaitForSignalEvent)
	callback.Response = json.RawMessage(`"approved"`)
	callback.SetStatus(FlowStatusCompleted)
	if _, err := infrastructure.UpdateEvent(context.Background(), callback); err != nil {
		t.Fatalf("update callback failed: %v", err)
	}

	// 重复驱动不得消耗挂起点：解决信号后仍能正常完成。
	if result := RunFlow(ctx, waitForHandler(), struct{}{}); result != "approved" {
		t.Fatalf("expected approved, got %q", result)
	}
}

// TestRecoversWhenCrashHappensAfterStepFlip 模拟"执行单元已置 SUSPENDED 但挂起
// 尚未提交"的中间态（提交前崩溃），驱动后应重新回到可恢复状态。
func TestRecoversWhenCrashHappensAfterStepFlip(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)

	catchSignal(t, func() { RunFlow(ctx, waitForHandler(), struct{}{}) })

	step, err := infrastructure.GetEvent(context.Background(), "approval")
	if err != nil {
		t.Fatalf("get step failed: %v", err)
	}
	stepEvent, ok := step.(ResultEvent)
	if !ok {
		t.Fatalf("expected result event, got %T", step)
	}
	stepEvent.SetStatus(FlowStatusRunning)
	if _, err := infrastructure.UpdateEvent(context.Background(), stepEvent); err != nil {
		t.Fatalf("update step failed: %v", err)
	}

	catchSignal(t, func() { RunFlow(ctx, waitForHandler(), struct{}{}) })

	step, err = infrastructure.GetEvent(context.Background(), "approval")
	if err != nil {
		t.Fatalf("get step failed: %v", err)
	}
	if step.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected step back to SUSPENDED, got %s", step.GetStatus())
	}

	messages, err := infrastructure.PendingOutbox(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected a single idempotent outbox message, got %+v", messages)
	}
}

func TestConcurrentDrivesProduceSingleSuspension(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)

	catchSignal(t, func() { RunFlow(ctx, waitForHandler(), struct{}{}) })

	const workers = 8
	var waitGroup sync.WaitGroup
	unexpected := make(chan any, workers)
	for i := 0; i < workers; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}
				switch recovered.(type) {
				case Signal, *ConflictError:
				default:
					unexpected <- recovered
				}
			}()
			RunFlow(ctx, waitForHandler(), struct{}{})
		}()
	}
	waitGroup.Wait()
	close(unexpected)
	for recovered := range unexpected {
		t.Fatalf("unexpected panic: %T %[1]v", recovered)
	}

	step, err := infrastructure.GetEvent(context.Background(), "approval")
	if err != nil {
		t.Fatalf("get step failed: %v", err)
	}
	if step.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected step suspended, got %s", step.GetStatus())
	}

	signal, err := infrastructure.GetEvent(context.Background(), "approval|callback")
	if err != nil {
		t.Fatalf("get callback failed: %v", err)
	}
	if signal == nil || signal.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected single suspended signal, got %+v", signal)
	}

	messages, err := infrastructure.PendingOutbox(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected a single outbox message, got %+v", messages)
	}
}

func TestGoBranchCommitsBeforeWaitReturns(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)
	handler := func(ctx *WorkflowContext) string {
		result := ctx.Go(
			"async",
			testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
				return actionCtx.WaitFor[string, string](
					"callback",
					WaitForOption[string]{Request: "review"},
				)
			}),
			struct{}{},
		)
		return result.Wait()
	}

	catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })

	step, err := infrastructure.GetEvent(context.Background(), "async")
	if err != nil {
		t.Fatalf("get async failed: %v", err)
	}
	if step.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected async step suspended before Wait returns, got %s", step.GetStatus())
	}

	signal, err := infrastructure.GetEvent(context.Background(), "async|callback")
	if err != nil {
		t.Fatalf("get callback failed: %v", err)
	}
	if signal == nil || signal.GetStatus() != FlowStatusSuspended {
		t.Fatalf("expected visible signal, got %+v", signal)
	}

	lease, err := infrastructure.ClaimLease(context.Background(), "async", time.Minute)
	if err != nil {
		t.Fatalf("claim lease failed: %v", err)
	}
	if lease == nil {
		t.Fatal("expected lease released before Wait returns")
	}
}

func TestLoopSuspendIsAtomic(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := WithInfrastructure(context.Background(), infrastructure)
	interval := 5 * time.Second
	handler := func(ctx *WorkflowContext) string {
		return ctx.Loop(
			"poll",
			testAction[struct{}, testLoopOutput](func(actionCtx *ActionContext, input struct{}) testLoopOutput {
				return testLoopOutput{Value: "pending", DoneValue: false}
			}),
			struct{}{},
			LoopRunOption{Interval: interval, MaxAttempts: 3},
		).Value
	}

	catchSignal(t, func() { RunFlow(ctx, handler, struct{}{}) })

	loop, err := infrastructure.GetEvent(context.Background(), "poll")
	if err != nil {
		t.Fatalf("get loop failed: %v", err)
	}
	loopEvent, ok := loop.(*LoopEvent)
	if !ok {
		t.Fatalf("expected loop event, got %T", loop)
	}
	if loopEvent.GetStatus() != FlowStatusSuspended || loopEvent.Attempt != 1 {
		t.Fatalf("expected suspended loop at attempt 1, got status=%s attempt=%d", loopEvent.GetStatus(), loopEvent.Attempt)
	}

	sleep, err := infrastructure.GetEvent(context.Background(), "poll|sleep|1")
	if err != nil {
		t.Fatalf("get sleep failed: %v", err)
	}
	sleepEvent, ok := sleep.(*SleepSignalEvent)
	if !ok {
		t.Fatalf("expected sleep event, got %T", sleep)
	}

	lease, err := infrastructure.ClaimLease(context.Background(), "poll", time.Minute)
	if err != nil {
		t.Fatalf("claim lease failed: %v", err)
	}
	if lease == nil {
		t.Fatal("expected loop lease to be released")
	}

	messages, err := infrastructure.PendingOutbox(context.Background(), sleepEvent.Until.Add(time.Second))
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(messages) != 1 || messages[0].SignalKey != "poll|sleep|1" {
		t.Fatalf("expected one sleep outbox message, got %+v", messages)
	}
	if !messages[0].AvailableAt.Equal(sleepEvent.Until) {
		t.Fatalf("expected AvailableAt to equal Until, got %s vs %s", messages[0].AvailableAt, sleepEvent.Until)
	}
}
