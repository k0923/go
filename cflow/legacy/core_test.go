package cflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type testAction[Input any, Output any] func(ctx *ActionContext, input Input) Output

func (action testAction[Input, Output]) Run(ctx *ActionContext, input Input) Output {
	return action(ctx, input)
}

type testFlow[Input any, Output any] func(ctx *FlowContext, input Input) Output

func (flow testFlow[Input, Output]) Run(ctx *FlowContext, input Input) Output {
	return flow(ctx, input)
}

type testLoopOutput struct {
	Value     string
	DoneValue bool
}

func (output testLoopOutput) Done() bool {
	return output.DoneValue
}

type trackingInfrastructure struct {
	*DebugInfrastructure
	lock      sync.Mutex
	claimKeys []string
}

func newTrackingInfrastructure() *trackingInfrastructure {
	return &trackingInfrastructure{
		DebugInfrastructure: NewDebugInfrastructure(),
	}
}

func (infrastructure *trackingInfrastructure) ClaimLease(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (*ExecutionLease, error) {
	infrastructure.lock.Lock()
	infrastructure.claimKeys = append(infrastructure.claimKeys, key)
	infrastructure.lock.Unlock()
	return infrastructure.DebugInfrastructure.ClaimLease(ctx, key, ttl)
}

func (infrastructure *trackingInfrastructure) ClaimKeys() []string {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	return append([]string(nil), infrastructure.claimKeys...)
}

func catchSignal(t *testing.T, handler func()) Signal {
	t.Helper()
	var signal Signal
	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("expected signal panic")
			}
			var ok bool
			signal, ok = recovered.(Signal)
			if !ok {
				t.Fatalf("expected Signal, got %T %[1]v", recovered)
			}
		}()
		handler()
	}()
	return signal
}

func catchError(t *testing.T, handler func()) error {
	t.Helper()
	var caught error
	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("expected error panic")
			}
			var ok bool
			caught, ok = recovered.(error)
			if !ok {
				t.Fatalf("expected error, got %T %[1]v", recovered)
			}
		}()
		handler()
	}()
	return caught
}

func TestRunFlow(t *testing.T) {
	t.Run("runs handler with workflow context", func(t *testing.T) {
		output := RunFlow(
			context.Background(),
			func(ctx *WorkflowContext) string {
				if ctx.GetKey() != "" {
					t.Fatalf("expected root key to be empty, got %q", ctx.GetKey())
				}
				return "done"
			},
			"input",
		)

		if output != "done" {
			t.Fatalf("expected handler output, got %q", output)
		}
	})

	t.Run("runs action with default debug infrastructure", func(t *testing.T) {
		output := RunFlow(
			context.Background(),
			func(ctx *WorkflowContext) string {
				return ctx.Run(
					"step",
					testAction[string, string](func(actionCtx *ActionContext, input string) string {
						if actionCtx.GetKey() != "step" {
							t.Fatalf("expected action key step, got %q", actionCtx.GetKey())
						}
						return input + "-done"
					}),
					"input",
					ActionRunOption{},
				)
			},
			struct{}{},
		)

		if output != "input-done" {
			t.Fatalf("expected action output, got %q", output)
		}
	})

	t.Run("nested flow does not claim parent flow lease", func(t *testing.T) {
		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)

		output := RunFlow(
			ctx,
			func(ctx *WorkflowContext) string {
				return ctx.RunFlow(
					"child",
					testFlow[string, string](func(flowCtx *FlowContext, input string) string {
						return flowCtx.Run(
							"step",
							testAction[string, string](func(actionCtx *ActionContext, input string) string {
								if actionCtx.GetKey() != "child|step" {
									t.Fatalf("expected namespaced action key, got %q", actionCtx.GetKey())
								}
								return input + "-child"
							}),
							input,
							ActionRunOption{},
						)
					}),
					"input",
					FlowRunOption{},
				).Wait()
			},
			struct{}{},
		)

		if output != "input-child" {
			t.Fatalf("expected nested flow output, got %q", output)
		}
		if !reflect.DeepEqual(infrastructure.ClaimKeys(), []string{"child|step"}) {
			t.Fatalf("expected only child action lease claim, got %v", infrastructure.ClaimKeys())
		}
		if event, err := infrastructure.GetEvent(context.Background(), "child|step"); err != nil {
			t.Fatalf("get event failed: %v", err)
		} else if event.GetStatus() != FlowStatusCompleted {
			t.Fatalf("expected child step completed, got %+v", event)
		}
	})

	t.Run("canceled context stops before handler", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("expected canceled context panic")
			}
			var contextErr *ContextError
			if err, ok := recovered.(error); !ok || !errors.As(err, &contextErr) {
				t.Fatalf("expected ContextError, got %T %[1]v", recovered)
			}
		}()

		RunFlow(
			ctx,
			func(ctx *WorkflowContext) string {
				t.Fatal("handler must not run")
				return ""
			},
			struct{}{},
		)
	})

	t.Run("completed action replays stored result without rerunning side effects", func(t *testing.T) {
		type replayOutput struct {
			UUID    string
			Now     string
			Attempt int
		}

		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		actionCalls := 0
		handler := func(ctx *WorkflowContext) replayOutput {
			return ctx.Run(
				"deterministic",
				testAction[struct{}, replayOutput](func(actionCtx *ActionContext, input struct{}) replayOutput {
					actionCalls++
					return replayOutput{
						UUID:    actionCtx.UUID("uuid"),
						Now:     actionCtx.Now("now").Format(time.RFC3339Nano),
						Attempt: actionCalls,
					}
				}),
				struct{}{},
				ActionRunOption{},
			)
		}

		first := RunFlow(ctx, handler, struct{}{})
		second := RunFlow(ctx, handler, struct{}{})

		if first != second {
			t.Fatalf("expected replayed result to stay stable, got first=%+v second=%+v", first, second)
		}
		if actionCalls != 1 {
			t.Fatalf("expected completed action to run once, got %d", actionCalls)
		}
		if !reflect.DeepEqual(infrastructure.ClaimKeys(), []string{"deterministic"}) {
			t.Fatalf("expected only initial lease claim, got %v", infrastructure.ClaimKeys())
		}
	})

	t.Run("suspended action resumes from signal and then replays completed result", func(t *testing.T) {
		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		actionCalls := 0
		handler := func(ctx *WorkflowContext) string {
			return ctx.Run(
				"approval",
				testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
					actionCalls++
					return actionCtx.WaitFor[string, string](
						"callback",
						WaitForOption[string]{Request: "review"},
					)
				}),
				struct{}{},
				ActionRunOption{},
			)
		}

		signal := catchSignal(t, func() {
			RunFlow(ctx, handler, struct{}{})
		})
		if signal.GetKey() != "approval|callback" {
			t.Fatalf("expected wait signal key approval|callback, got %q", signal.GetKey())
		}
		stepEvent, err := infrastructure.GetEvent(context.Background(), "approval")
		if err != nil {
			t.Fatalf("get approval event failed: %v", err)
		}
		if stepEvent.GetStatus() != FlowStatusSuspended {
			t.Fatalf("expected approval step suspended, got %+v", stepEvent)
		}

		waitEvent, err := infrastructure.GetEvent(context.Background(), "approval|callback")
		if err != nil {
			t.Fatalf("get callback event failed: %v", err)
		}
		callback := waitEvent.(*WaitForSignalEvent)
		callback.Response = json.RawMessage(`"approved"`)
		callback.SetStatus(FlowStatusCompleted)
		if _, err := infrastructure.UpdateEvent(context.Background(), callback); err != nil {
			t.Fatalf("update callback event failed: %v", err)
		}

		resumed := RunFlow(ctx, handler, struct{}{})
		replayed := RunFlow(ctx, handler, struct{}{})

		if resumed != "approved" || replayed != "approved" {
			t.Fatalf("expected approved result after resume and replay, got resumed=%q replayed=%q", resumed, replayed)
		}
		if actionCalls != 2 {
			t.Fatalf("expected action to run once before suspension and once on resume, got %d", actionCalls)
		}
		if !reflect.DeepEqual(infrastructure.ClaimKeys(), []string{"approval", "approval"}) {
			t.Fatalf("expected lease claim only for suspend and resume, got %v", infrastructure.ClaimKeys())
		}
	})

	t.Run("concurrent resume attempts allow only one worker to advance a running branch", func(t *testing.T) {
		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		release := make(chan struct{})
		started := make(chan struct{}, 2)
		actionCalls := 0
		var callsLock sync.Mutex
		handler := func(ctx *WorkflowContext) string {
			return ctx.Run(
				"exclusive",
				testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
					callsLock.Lock()
					actionCalls++
					callsLock.Unlock()
					started <- struct{}{}
					<-release
					return "done"
				}),
				struct{}{},
				ActionRunOption{},
			)
		}

		results := make(chan string, 2)
		panics := make(chan any, 2)
		runAttempt := func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					panics <- recovered
				}
			}()
			results <- RunFlow(ctx, handler, struct{}{})
		}

		go runAttempt()
		<-started
		go runAttempt()

		var conflict *ConflictError
		select {
		case recovered := <-panics:
			var ok bool
			conflict, ok = recovered.(*ConflictError)
			if !ok {
				t.Fatalf("expected conflict panic, got %T %[1]v", recovered)
			}
		case <-time.After(time.Second):
			t.Fatal("expected concurrent attempt to conflict")
		}
		if conflict.Event.GetKey() != "exclusive" {
			t.Fatalf("expected conflict on exclusive event, got %q", conflict.Event.GetKey())
		}

		close(release)
		select {
		case result := <-results:
			if result != "done" {
				t.Fatalf("expected winning attempt result done, got %q", result)
			}
		case <-time.After(time.Second):
			t.Fatal("expected winning attempt to complete")
		}

		callsLock.Lock()
		defer callsLock.Unlock()
		if actionCalls != 1 {
			t.Fatalf("expected only one worker to execute action body, got %d", actionCalls)
		}
		if !reflect.DeepEqual(infrastructure.ClaimKeys(), []string{"exclusive", "exclusive"}) {
			t.Fatalf("expected two claim attempts for one branch, got %v", infrastructure.ClaimKeys())
		}
	})

	t.Run("async go replays completed result without rerunning action", func(t *testing.T) {
		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		actionCalls := 0
		handler := func(ctx *WorkflowContext) string {
			return ctx.Go(
				"async",
				testAction[string, string](func(actionCtx *ActionContext, input string) string {
					actionCalls++
					return input + ":" + actionCtx.UUID("id")
				}),
				"payload",
			).Wait()
		}

		first := RunFlow(ctx, handler, struct{}{})
		second := RunFlow(ctx, handler, struct{}{})

		if first != second {
			t.Fatalf("expected async result to replay, got first=%q second=%q", first, second)
		}
		if actionCalls != 1 {
			t.Fatalf("expected async action to run once, got %d", actionCalls)
		}
		if !reflect.DeepEqual(infrastructure.ClaimKeys(), []string{"async"}) {
			t.Fatalf("expected one async lease claim, got %v", infrastructure.ClaimKeys())
		}
	})

	t.Run("sleeping action resumes after deadline and then replays", func(t *testing.T) {
		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		actionCalls := 0
		handler := func(ctx *WorkflowContext) string {
			return ctx.Run(
				"sleepy",
				testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
					actionCalls++
					actionCtx.SleepUntil("nap", time.Now().Add(5*time.Second))
					return "awake"
				}),
				struct{}{},
				ActionRunOption{},
			)
		}

		signal := catchSignal(t, func() {
			RunFlow(ctx, handler, struct{}{})
		})
		if signal.GetKey() != "sleepy|nap" {
			t.Fatalf("expected sleep signal key sleepy|nap, got %q", signal.GetKey())
		}
		stepEvent, err := infrastructure.GetEvent(context.Background(), "sleepy")
		if err != nil {
			t.Fatalf("get sleepy event failed: %v", err)
		}
		if stepEvent.GetStatus() != FlowStatusSuspended {
			t.Fatalf("expected sleepy step suspended, got %+v", stepEvent)
		}

		sleepEvent, err := infrastructure.GetEvent(context.Background(), "sleepy|nap")
		if err != nil {
			t.Fatalf("get sleep event failed: %v", err)
		}
		sleep := sleepEvent.(*SleepSignalEvent)
		sleep.Until = time.Now().Add(-time.Millisecond)
		if _, err := infrastructure.UpdateEvent(context.Background(), sleep); err != nil {
			t.Fatalf("update sleep event failed: %v", err)
		}

		resumed := RunFlow(ctx, handler, struct{}{})
		replayed := RunFlow(ctx, handler, struct{}{})

		if resumed != "awake" || replayed != "awake" {
			t.Fatalf("expected awake result after resume and replay, got resumed=%q replayed=%q", resumed, replayed)
		}
		if actionCalls != 2 {
			t.Fatalf("expected sleeping action to run once before suspension and once on resume, got %d", actionCalls)
		}
		if !reflect.DeepEqual(infrastructure.ClaimKeys(), []string{"sleepy", "sleepy"}) {
			t.Fatalf("expected lease claim only for suspend and resume, got %v", infrastructure.ClaimKeys())
		}
	})

	t.Run("loop resumes next attempt and replays completed result", func(t *testing.T) {
		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		actionCalls := 0
		handler := func(ctx *WorkflowContext) testLoopOutput {
			return ctx.Loop(
				"poll",
				testAction[struct{}, testLoopOutput](func(actionCtx *ActionContext, input struct{}) testLoopOutput {
					actionCalls++
					return testLoopOutput{
						Value:     actionCtx.GetKey(),
						DoneValue: actionCalls >= 2,
					}
				}),
				struct{}{},
				LoopRunOption{Interval: minLoopInterval, MaxAttempts: 3},
			)
		}

		signal := catchSignal(t, func() {
			RunFlow(ctx, handler, struct{}{})
		})
		if signal.GetKey() != "poll|sleep|1" {
			t.Fatalf("expected loop sleep signal key poll|sleep|1, got %q", signal.GetKey())
		}
		loopEvent, err := infrastructure.GetEvent(context.Background(), "poll")
		if err != nil {
			t.Fatalf("get loop event failed: %v", err)
		}
		loop := loopEvent.(*LoopEvent)
		if loop.GetStatus() != FlowStatusSuspended || loop.Attempt != 1 {
			t.Fatalf("expected loop suspended at attempt 1, got %+v", loop)
		}

		sleepEvent, err := infrastructure.GetEvent(context.Background(), "poll|sleep|1")
		if err != nil {
			t.Fatalf("get loop sleep event failed: %v", err)
		}
		sleep := sleepEvent.(*SleepSignalEvent)
		sleep.Until = time.Now().Add(-time.Millisecond)
		if _, err := infrastructure.UpdateEvent(context.Background(), sleep); err != nil {
			t.Fatalf("update loop sleep event failed: %v", err)
		}

		resumed := RunFlow(ctx, handler, struct{}{})
		replayed := RunFlow(ctx, handler, struct{}{})

		if resumed.Value != "poll|1" || !resumed.DoneValue {
			t.Fatalf("expected loop to complete on second attempt, got %+v", resumed)
		}
		if replayed != resumed {
			t.Fatalf("expected loop result to replay, got resumed=%+v replayed=%+v", resumed, replayed)
		}
		if actionCalls != 2 {
			t.Fatalf("expected loop action to run twice before completion, got %d", actionCalls)
		}
		if !reflect.DeepEqual(infrastructure.ClaimKeys(), []string{"poll", "poll"}) {
			t.Fatalf("expected loop lease claim for initial run and resume, got %v", infrastructure.ClaimKeys())
		}
	})

	t.Run("failed action replays stored failure without rerunning action", func(t *testing.T) {
		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		actionCalls := 0
		handler := func(ctx *WorkflowContext) string {
			return ctx.Run(
				"failing",
				testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
					actionCalls++
					panic(errors.New("boom"))
				}),
				struct{}{},
				ActionRunOption{},
			)
		}

		firstErr := catchError(t, func() {
			RunFlow(ctx, handler, struct{}{})
		})
		var bizErr *BizError
		if !errors.As(firstErr, &bizErr) || bizErr.Event.GetKey() != "failing" {
			t.Fatalf("expected first run biz error on failing event, got %T %[1]v", firstErr)
		}

		secondErr := catchError(t, func() {
			RunFlow(ctx, handler, struct{}{})
		})
		var legacyErr *LegacyBizError
		if !errors.As(secondErr, &legacyErr) || legacyErr.Event.GetKey() != "failing" {
			t.Fatalf("expected replay legacy biz error on failing event, got %T %[1]v", secondErr)
		}
		if actionCalls != 1 {
			t.Fatalf("expected failed action to run only once, got %d", actionCalls)
		}
		event, err := infrastructure.GetEvent(context.Background(), "failing")
		if err != nil {
			t.Fatalf("get failing event failed: %v", err)
		}
		if event.GetStatus() != FlowStatusFailed || event.GetError() != "boom" {
			t.Fatalf("expected failed event with boom error, got %+v", event)
		}
		if !reflect.DeepEqual(infrastructure.ClaimKeys(), []string{"failing"}) {
			t.Fatalf("expected only initial failing lease claim, got %v", infrastructure.ClaimKeys())
		}
	})

	t.Run("immediate sleep completes in one run and replays", func(t *testing.T) {
		infrastructure := newTrackingInfrastructure()
		ctx := WithInfrastructure(context.Background(), infrastructure)
		actionCalls := 0
		handler := func(ctx *WorkflowContext) string {
			return ctx.Run(
				"quick-sleep",
				testAction[struct{}, string](func(actionCtx *ActionContext, input struct{}) string {
					actionCalls++
					actionCtx.Sleep("skip", -time.Millisecond)
					return "done"
				}),
				struct{}{},
				ActionRunOption{},
			)
		}

		first := RunFlow(ctx, handler, struct{}{})
		second := RunFlow(ctx, handler, struct{}{})

		if first != "done" || second != "done" {
			t.Fatalf("expected immediate sleep result to replay, got first=%q second=%q", first, second)
		}
		sleepEvent, err := infrastructure.GetEvent(context.Background(), "quick-sleep|skip")
		if err != nil {
			t.Fatalf("get quick sleep event failed: %v", err)
		}
		if sleepEvent.GetStatus() != FlowStatusCompleted {
			t.Fatalf("expected immediate sleep completed, got %+v", sleepEvent)
		}
		if actionCalls != 1 {
			t.Fatalf("expected immediate sleep action to run once, got %d", actionCalls)
		}
	})
}

func TestDebugInfrastructure(t *testing.T) {
	t.Run("renew lease and release use generation fencing", func(t *testing.T) {
		infrastructure := NewDebugInfrastructure()
		ctx := context.Background()

		lease, err := infrastructure.ClaimLease(ctx, "branch", time.Minute)
		if err != nil {
			t.Fatalf("claim lease failed: %v", err)
		}
		if lease == nil {
			t.Fatal("expected lease")
		}

		renewed, err := infrastructure.RenewLease(ctx, *lease, time.Minute)
		if err != nil {
			t.Fatalf("renew lease failed: %v", err)
		}
		if renewed == nil || renewed.Generation != lease.Generation {
			t.Fatalf("expected renewed lease with same generation, got %+v", renewed)
		}

		stale := *lease
		stale.Generation++
		if renewed, err := infrastructure.RenewLease(ctx, stale, time.Minute); err != nil || renewed != nil {
			t.Fatalf("expected stale renew conflict, got lease=%+v err=%v", renewed, err)
		}
		if released, err := infrastructure.ReleaseLease(ctx, stale); err != nil || released {
			t.Fatalf("expected stale release conflict, got released=%t err=%v", released, err)
		}
		if released, err := infrastructure.ReleaseLease(ctx, *lease); err != nil || !released {
			t.Fatalf("expected release success, got released=%t err=%v", released, err)
		}
	})

	t.Run("event add and update enforce key and version compare and swap", func(t *testing.T) {
		infrastructure := NewDebugInfrastructure()
		ctx := context.Background()

		added, err := infrastructure.AddEvent(ctx, &StepResultEvent{
			BaseEvent{Key: "step", Status: FlowStatusRunning},
			nil,
		})
		if err != nil {
			t.Fatalf("add event failed: %v", err)
		}
		if added.GetVersion() != 1 {
			t.Fatalf("expected added version 1, got %d", added.GetVersion())
		}
		duplicate, duplicateErr := infrastructure.AddEvent(ctx, &StepResultEvent{
			BaseEvent{Key: "step", Status: FlowStatusRunning},
			nil,
		})
		if duplicateErr != nil || duplicate != nil {
			t.Fatalf("expected duplicate add conflict, got event=%+v err=%v", duplicate, duplicateErr)
		}

		step := added.(*StepResultEvent)
		step.SetStatus(FlowStatusCompleted)
		updated, err := infrastructure.UpdateEvent(ctx, step)
		if err != nil {
			t.Fatalf("update event failed: %v", err)
		}
		if updated.GetVersion() != 2 || updated.GetStatus() != FlowStatusCompleted {
			t.Fatalf("expected completed version 2, got %+v", updated)
		}
		if stale, err := infrastructure.UpdateEvent(ctx, step); err != nil || stale != nil {
			t.Fatalf("expected stale update conflict, got event=%+v err=%v", stale, err)
		}
	})

	t.Run("records errors and returns a copy", func(t *testing.T) {
		infrastructure := NewDebugInfrastructure()
		infrastructure.OnError(nil)
		infrastructure.OnError(errors.New("first"))

		errs := infrastructure.Errors()
		if len(errs) != 1 || errs[0].Error() != "first" {
			t.Fatalf("expected one recorded error, got %v", errs)
		}
		errs[0] = errors.New("mutated")
		if got := infrastructure.Errors(); got[0].Error() != "first" {
			t.Fatalf("expected errors copy, got %v", got)
		}
	})
}
