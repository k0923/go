package flow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type blockingGetEventInfrastructure struct {
	*InMemoryInfrastructure[struct{}, string]
	started chan struct{}
	release chan struct{}
}

type trackingLeaseInfrastructure struct {
	*InMemoryInfrastructure[struct{}, string]
	claims          atomic.Int64
	renewals        atomic.Int64
	releases        atomic.Int64
	leaseDuration   time.Duration
	renewError      error
	renewalConflict bool
}

func (infrastructure *trackingLeaseInfrastructure) ClaimLease(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (*ExecutionLease, error) {
	infrastructure.claims.Add(1)
	lease, err := infrastructure.InMemoryInfrastructure.ClaimLease(ctx, key, ttl)
	if lease != nil && infrastructure.leaseDuration > 0 {
		lease.ExpiresAt = time.Now().Add(infrastructure.leaseDuration)
	}
	return lease, err
}

func (infrastructure *trackingLeaseInfrastructure) RenewLease(
	ctx context.Context,
	lease ExecutionLease,
	ttl time.Duration,
) (*ExecutionLease, error) {
	infrastructure.renewals.Add(1)
	if infrastructure.renewError != nil {
		return nil, infrastructure.renewError
	}
	if infrastructure.renewalConflict {
		_, _ = infrastructure.InMemoryInfrastructure.ReleaseLease(ctx, lease)
		_, _ = infrastructure.InMemoryInfrastructure.ClaimLease(ctx, lease.Key, ttl)
		return nil, nil
	}
	return infrastructure.InMemoryInfrastructure.RenewLease(ctx, lease, ttl)
}

func (infrastructure *trackingLeaseInfrastructure) ReleaseLease(
	ctx context.Context,
	lease ExecutionLease,
) (bool, error) {
	infrastructure.releases.Add(1)
	return infrastructure.InMemoryInfrastructure.ReleaseLease(ctx, lease)
}

func (infrastructure *blockingGetEventInfrastructure) GetEvent(ctx context.Context, key string) (Event, error) {
	close(infrastructure.started)
	select {
	case <-infrastructure.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return infrastructure.InMemoryInfrastructure.GetEvent(ctx, key)
}

func TestRuntimeLeaseLifecycle(t *testing.T) {
	t.Run("nested step reuses go lease", func(t *testing.T) {
		infrastructure := &trackingLeaseInfrastructure{
			InMemoryInfrastructure: NewInMemoryInfrastructure[struct{}, string](),
		}
		var inherited atomic.Bool
		target := Flow[struct{}, string]{
			Handler: func(ctx *WorkflowContext[struct{}, string]) string {
				return ctx.Go("branch", func(goContext *GoContext[struct{}, string]) string {
					return goContext.Step(
						"step",
						func(stepContext StepContext[struct{}, string]) (string, error) {
							inherited.Store(stepContext.lease == goContext.lease)
							return "done", nil
						},
					)
				}).Wait()
			},
		}

		target.StartRun(context.Background(), struct{}{}, infrastructure)

		if !inherited.Load() {
			t.Fatal("step did not inherit go lease")
		}
		if infrastructure.claims.Load() != 1 || infrastructure.releases.Load() != 1 {
			t.Fatalf(
				"expected one claim and release, got claims=%d releases=%d",
				infrastructure.claims.Load(),
				infrastructure.releases.Load(),
			)
		}
	})

	tests := []struct {
		name    string
		handler func(StepContext[struct{}, string]) (string, error)
	}{
		{
			name: "error",
			handler: func(StepContext[struct{}, string]) (string, error) {
				return "", errors.New("failed")
			},
		},
		{
			name: "signal",
			handler: func(ctx StepContext[struct{}, string]) (string, error) {
				return ctx.WaitFor[string, string](
					"approval",
					WaitForOption[string]{Request: "review"},
				), nil
			},
		},
		{
			name: "unknown panic",
			handler: func(StepContext[struct{}, string]) (string, error) {
				panic("failed")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name+" releases lease", func(t *testing.T) {
			infrastructure := &trackingLeaseInfrastructure{
				InMemoryInfrastructure: NewInMemoryInfrastructure[struct{}, string](),
			}
			target := Flow[struct{}, string]{
				Handler: func(ctx *WorkflowContext[struct{}, string]) string {
					return ctx.Step("step", test.handler)
				},
			}

			target.StartRun(context.Background(), struct{}{}, infrastructure)

			if infrastructure.claims.Load() != 1 || infrastructure.releases.Load() != 1 {
				t.Fatalf(
					"expected one claim and release, got claims=%d releases=%d",
					infrastructure.claims.Load(),
					infrastructure.releases.Load(),
				)
			}
		})
	}
}

func TestRuntimeCancelsAndReleasesAfterLeaseRenewalFailure(t *testing.T) {
	tests := []struct {
		name            string
		renewError      error
		renewalConflict bool
		expectedStatus  FlowStatus
		expectedErrors  int
	}{
		{
			name:           "infrastructure error",
			renewError:     errors.New("renew failed"),
			expectedStatus: FlowStatusRunning,
			expectedErrors: 1,
		},
		{
			name:            "generation conflict",
			renewalConflict: true,
			expectedStatus:  FlowStatusRunning,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			infrastructure := &trackingLeaseInfrastructure{
				InMemoryInfrastructure: NewInMemoryInfrastructure[struct{}, string](),
				leaseDuration:          15 * time.Millisecond,
				renewError:             test.renewError,
				renewalConflict:        test.renewalConflict,
			}
			target := Flow[struct{}, string]{
				Handler: func(ctx *WorkflowContext[struct{}, string]) string {
					return ctx.Step(
						"step",
						func(stepContext StepContext[struct{}, string]) (string, error) {
							<-stepContext.Done()
							return "", stepContext.Err()
						},
					)
				},
			}

			target.StartRun(context.Background(), struct{}{}, infrastructure)

			if infrastructure.renewals.Load() != 1 {
				t.Fatalf("expected one renewal, got %d", infrastructure.renewals.Load())
			}
			if infrastructure.releases.Load() != 1 {
				t.Fatalf("expected one release, got %d", infrastructure.releases.Load())
			}
			state, err := infrastructure.GetState(context.Background())
			if err != nil {
				t.Fatalf("get state failed: %v", err)
			}
			if state.Status != test.expectedStatus {
				t.Fatalf("expected %s state, got %+v", test.expectedStatus, state)
			}
			if len(infrastructure.Errors()) != test.expectedErrors {
				t.Fatalf(
					"expected %d infrastructure errors, got %v",
					test.expectedErrors,
					infrastructure.Errors(),
				)
			}
		})
	}
}

func TestFlowStatusSuspendedValue_BitsUT(t *testing.T) {
	if FlowStatusSuspended != "SUSPENDED" {
		t.Fatalf("expected SUSPENDED status value, got %q", FlowStatusSuspended)
	}
}

func TestExecutionContextHierarchy(t *testing.T) {
	workflowType := reflect.TypeFor[WorkflowContext[struct{}, string]]()
	goField, exists := workflowType.FieldByName("GoContext")
	if !exists || !goField.Anonymous {
		t.Fatal("WorkflowContext must embed GoContext")
	}

	goType := reflect.TypeFor[GoContext[struct{}, string]]()
	stepField, exists := goType.FieldByName("StepContext")
	if !exists || !stepField.Anonymous {
		t.Fatal("GoContext must embed StepContext")
	}

	stepType := reflect.TypeFor[StepContext[struct{}, string]]()
	if _, exists := stepType.FieldByName("runtime"); exists {
		t.Fatal("StepContext must own atomic runtime capabilities directly")
	}
	if _, exists := stepType.MethodByName("Loop"); exists {
		t.Fatal("StepContext must not expose Loop")
	}
}

func TestFlowRunsStepAndReusesCompletedState_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[string, string]()
	var handlerCalls atomic.Int64
	var stepCalls atomic.Int64
	target := Flow[string, string]{
		Handler: func(runtime *WorkflowContext[string, string]) string {
			handlerCalls.Add(1)
			return runtime.Step(
				"echo",
				func(ctx StepContext[string, string]) (string, error) {
					stepCalls.Add(1)
					return ctx.GetKey() + "-output", nil
				},
			)
		},
	}

	target.StartRun(context.Background(), "input", infrastructure)
	target.StartRun(context.Background(), "ignored", infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted || state.Output != "echo-output" {
		t.Fatalf("unexpected final state: %+v", state)
	}
	if handlerCalls.Load() != 1 || stepCalls.Load() != 1 {
		t.Fatalf(
			"expected handler and step once, got handler=%d step=%d",
			handlerCalls.Load(),
			stepCalls.Load(),
		)
	}
	event, err := infrastructure.GetEvent(context.Background(), "echo")
	if err != nil {
		t.Fatalf("get step event failed: %v", err)
	}
	if event.GetStatus() != FlowStatusCompleted || event.GetVersion() != 2 {
		t.Fatalf("unexpected step event: %+v", event)
	}
}

func TestFlowResumesWaitForAndReusesCompletedStep_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var stepCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			prepared := runtime.Step(
				"prepare",
				func(StepContext[struct{}, string]) (string, error) {
					stepCalls.Add(1)
					return "prepared", nil
				},
			)
			approval := runtime.WaitFor[string, string](
				"approval",
				WaitForOption[string]{Request: prepared},
			)
			return prepared + ":" + approval
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)
	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get waiting state failed: %v", err)
	}
	if state.Status != FlowStatusRunning {
		t.Fatalf("expected running state while waiting, got %s", state.Status)
	}
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one signal, got %d", len(signals))
	}
	awaited := signals[0].(*WaitForSignalEvent)
	callback := *awaited
	callback.Response = json.RawMessage(`"approved"`)

	target.ResumeRun(context.Background(), infrastructure, &callback)

	state, err = infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get resumed state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted ||
		state.Output != "prepared:approved" {
		t.Fatalf("unexpected resumed state: %+v", state)
	}
	if stepCalls.Load() != 1 {
		t.Fatalf("expected completed step to be reused, got %d calls", stepCalls.Load())
	}
}

func TestFlowResumesWaitForInsideStepContext(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			return runtime.Step(
				"review-step",
				func(step StepContext[struct{}, string]) (string, error) {
					return step.WaitFor[string, string](
						"approval",
						WaitForOption[string]{Request: "review"},
					), nil
				},
			)
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one signal, got %d", len(signals))
	}
	callback := *(signals[0].(*WaitForSignalEvent))
	callback.Response = json.RawMessage(`"approved"`)
	target.ResumeRun(ctx, infrastructure, &callback)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted || state.Output != "approved" {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestFlowResumesSleepAfterDeadline_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var handlerCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			handlerCalls.Add(1)
			runtime.Sleep("short-sleep", 15*time.Millisecond)
			return "completed"
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one sleep signal, got %d", len(signals))
	}
	sleep := signals[0].(*SleepSignalEvent)
	time.Sleep(time.Until(sleep.Until) + 5*time.Millisecond)
	target.ResumeRun(context.Background(), infrastructure, sleep)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get resumed state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted || state.Output != "completed" {
		t.Fatalf("unexpected sleep state: %+v", state)
	}
	if handlerCalls.Load() != 2 {
		t.Fatalf("expected initial and resumed handler calls, got %d", handlerCalls.Load())
	}
}

func TestFlowIgnoresSleepSignalBeforeDeadline_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var handlerCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			handlerCalls.Add(1)
			runtime.Sleep("long-sleep", 25*time.Millisecond)
			return "completed"
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one sleep signal, got %d", len(signals))
	}
	sleep := signals[0].(*SleepSignalEvent)

	target.ResumeRun(ctx, infrastructure, sleep)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get early resumed state failed: %v", err)
	}
	if state.Status != FlowStatusRunning {
		t.Fatalf("expected running state before deadline, got %+v", state)
	}
	event, err := infrastructure.GetEvent(ctx, sleep.Key)
	if err != nil {
		t.Fatalf("get sleep event failed: %v", err)
	}
	if event.GetStatus() != FlowStatusSuspended || event.GetVersion() != sleep.Version {
		t.Fatalf("sleep event changed before deadline: %+v", event)
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("handler ran before sleep deadline, got %d calls", handlerCalls.Load())
	}

	time.Sleep(time.Until(sleep.Until) + 5*time.Millisecond)
	target.ResumeRun(ctx, infrastructure, sleep)

	state, err = infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get resumed state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted || state.Output != "completed" {
		t.Fatalf("unexpected resumed state: %+v", state)
	}
	if handlerCalls.Load() != 2 {
		t.Fatalf("expected handler to rerun after deadline only, got %d calls", handlerCalls.Load())
	}
}

func TestFlowLoopPollsUntilDone(t *testing.T) {
	infrastructure := &trackingLeaseInfrastructure{
		InMemoryInfrastructure: NewInMemoryInfrastructure[struct{}, string](),
	}
	var handlerCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			return runtime.Loop(
				"poll",
				LoopOption{
					Interval:    minLoopInterval,
					MaxAttempts: 3,
				},
				func(ctx *GoContext[struct{}, string], attempt int) (LoopResult[string], error) {
					call := handlerCalls.Add(1)
					if int(call) != attempt+1 {
						t.Fatalf("expected attempt %d for call %d", call-1, call)
					}
					if attempt < 2 {
						return LoopResult[string]{Result: "pending"}, nil
					}
					return LoopResult[string]{Result: "done", Done: true}, nil
				},
			)
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get polling state failed: %v", err)
	}
	if state.Status != FlowStatusRunning {
		t.Fatalf("expected running state after first pending poll, got %+v", state)
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("expected one poll attempt, got %d", handlerCalls.Load())
	}
	if infrastructure.claims.Load() != 1 || infrastructure.releases.Load() != 1 {
		t.Fatalf(
			"expected first poll to claim and release once, got claims=%d releases=%d",
			infrastructure.claims.Load(),
			infrastructure.releases.Load(),
		)
	}

	event, err := infrastructure.GetEvent(ctx, "poll")
	if err != nil {
		t.Fatalf("get loop event failed: %v", err)
	}
	loop := event.(*LoopEvent)
	if loop.Status != FlowStatusSuspended || loop.Attempt != 1 {
		t.Fatalf("unexpected loop event after first poll: %+v", loop)
	}

	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected first sleep signal, got %d", len(signals))
	}
	firstSleep := signals[0].(*SleepSignalEvent)
	firstSleep = expireSleepSignal(t, ctx, infrastructure, firstSleep)
	target.ResumeRun(ctx, infrastructure, firstSleep)

	if handlerCalls.Load() != 2 {
		t.Fatalf("expected second poll attempt, got %d", handlerCalls.Load())
	}
	if infrastructure.claims.Load() != 2 || infrastructure.releases.Load() != 2 {
		t.Fatalf(
			"expected second poll to run under a new released lease, got claims=%d releases=%d",
			infrastructure.claims.Load(),
			infrastructure.releases.Load(),
		)
	}
	event, err = infrastructure.GetEvent(ctx, "poll")
	if err != nil {
		t.Fatalf("get loop event after second poll failed: %v", err)
	}
	loop = event.(*LoopEvent)
	if loop.Status != FlowStatusSuspended || loop.Attempt != 2 {
		t.Fatalf("unexpected loop event after second poll: %+v", loop)
	}

	signals = infrastructure.Signals()
	if len(signals) != 2 {
		t.Fatalf("expected second sleep signal, got %d", len(signals))
	}
	secondSleep := signals[1].(*SleepSignalEvent)
	secondSleep = expireSleepSignal(t, ctx, infrastructure, secondSleep)
	target.ResumeRun(ctx, infrastructure, secondSleep)

	state, err = infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get completed loop state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted || state.Output != "done" {
		t.Fatalf("unexpected completed loop state: %+v", state)
	}
	if handlerCalls.Load() != 3 {
		t.Fatalf("expected three poll attempts, got %d", handlerCalls.Load())
	}
	if infrastructure.claims.Load() != 3 || infrastructure.releases.Load() != 3 {
		t.Fatalf(
			"expected every poll attempt to claim and release, got claims=%d releases=%d",
			infrastructure.claims.Load(),
			infrastructure.releases.Load(),
		)
	}
	event, err = infrastructure.GetEvent(ctx, "poll")
	if err != nil {
		t.Fatalf("get completed loop event failed: %v", err)
	}
	loop = event.(*LoopEvent)
	if loop.Status != FlowStatusCompleted || loop.Attempt != 2 {
		t.Fatalf("unexpected completed loop event: %+v", loop)
	}
}

func TestFlowLoopIgnoresSleepBeforeDeadline(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var handlerCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			return runtime.Loop(
				"poll",
				LoopOption{
					Interval:    minLoopInterval,
					MaxAttempts: 2,
				},
				func(*GoContext[struct{}, string], int) (LoopResult[string], error) {
					handlerCalls.Add(1)
					return LoopResult[string]{Result: "pending"}, nil
				},
			)
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one sleep signal, got %d", len(signals))
	}
	sleep := signals[0].(*SleepSignalEvent)

	target.ResumeRun(ctx, infrastructure, sleep)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get early resumed state failed: %v", err)
	}
	if state.Status != FlowStatusRunning {
		t.Fatalf("expected running state before deadline, got %+v", state)
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("loop ran before sleep deadline, got %d calls", handlerCalls.Load())
	}
	event, err := infrastructure.GetEvent(ctx, "poll")
	if err != nil {
		t.Fatalf("get loop event failed: %v", err)
	}
	loop := event.(*LoopEvent)
	if loop.Status != FlowStatusSuspended || loop.Attempt != 1 {
		t.Fatalf("loop event changed before deadline: %+v", loop)
	}
}

func TestFlowLoopFailsAfterMaxAttempts(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var handlerCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			return runtime.Loop(
				"poll",
				LoopOption{
					Interval:    minLoopInterval,
					MaxAttempts: 2,
				},
				func(*GoContext[struct{}, string], int) (LoopResult[string], error) {
					handlerCalls.Add(1)
					return LoopResult[string]{Result: "pending"}, nil
				},
			)
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one sleep signal, got %d", len(signals))
	}
	sleep := signals[0].(*SleepSignalEvent)
	sleep = expireSleepSignal(t, ctx, infrastructure, sleep)
	target.ResumeRun(ctx, infrastructure, sleep)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get failed loop state failed: %v", err)
	}
	if state.Status != FlowStatusFailed ||
		len(state.Errors) != 1 ||
		state.Errors[0] != "biz error: poll" {
		t.Fatalf("unexpected failed loop state: %+v", state)
	}
	if handlerCalls.Load() != 2 {
		t.Fatalf("expected exactly max attempts, got %d", handlerCalls.Load())
	}
	event, err := infrastructure.GetEvent(ctx, "poll")
	if err != nil {
		t.Fatalf("get failed loop event failed: %v", err)
	}
	loop := event.(*LoopEvent)
	if loop.Status != FlowStatusFailed ||
		loop.Error != "loop max attempts exceeded: 2" ||
		loop.Attempt != 1 {
		t.Fatalf("unexpected failed loop event: %+v", loop)
	}
	if len(infrastructure.Signals()) != 1 {
		t.Fatalf("loop scheduled a sleep after max attempts: %d", len(infrastructure.Signals()))
	}
}

func TestFlowLoopFailsWhenHandlerReturnsError(t *testing.T) {
	loopErr := errors.New("poll failed")
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			return runtime.Loop(
				"poll",
				LoopOption{
					Interval:    minLoopInterval,
					MaxAttempts: 3,
				},
				func(*GoContext[struct{}, string], int) (LoopResult[string], error) {
					return LoopResult[string]{}, loopErr
				},
			)
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get failed loop state failed: %v", err)
	}
	if state.Status != FlowStatusFailed ||
		len(state.Errors) != 1 ||
		state.Errors[0] != "biz error: poll" {
		t.Fatalf("unexpected failed loop state: %+v", state)
	}
	event, err := infrastructure.GetEvent(ctx, "poll")
	if err != nil {
		t.Fatalf("get failed loop event failed: %v", err)
	}
	if event.GetStatus() != FlowStatusFailed || event.GetError() != loopErr.Error() {
		t.Fatalf("unexpected failed loop event: %+v", event)
	}
}

func TestFlowReusesCompletedLoopResult(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var handlerCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			return runtime.Loop(
				"poll",
				LoopOption{
					Interval:    minLoopInterval,
					MaxAttempts: 2,
				},
				func(*GoContext[struct{}, string], int) (LoopResult[string], error) {
					handlerCalls.Add(1)
					return LoopResult[string]{Result: "done", Done: true}, nil
				},
			)
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)
	target.StartRun(ctx, struct{}{}, infrastructure)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get completed loop state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted || state.Output != "done" {
		t.Fatalf("unexpected completed loop state: %+v", state)
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("expected completed loop result to be reused, got %d calls", handlerCalls.Load())
	}
}

func TestFlowLoopRejectsIntervalBelowMinimum(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var handlerCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			return runtime.Loop(
				"poll",
				LoopOption{
					Interval:    minLoopInterval - time.Nanosecond,
					MaxAttempts: 2,
				},
				func(*GoContext[struct{}, string], int) (LoopResult[string], error) {
					handlerCalls.Add(1)
					return LoopResult[string]{Result: "done", Done: true}, nil
				},
			)
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get failed loop state failed: %v", err)
	}
	if state.Status != FlowStatusFailed ||
		len(state.Errors) != 1 ||
		state.Errors[0] != "loop interval must be at least 5s" {
		t.Fatalf("unexpected failed loop state: %+v", state)
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("loop handler ran despite invalid interval: %d", handlerCalls.Load())
	}
}

func expireSleepSignal(
	t *testing.T,
	ctx context.Context,
	infrastructure interface {
		GetEvent(context.Context, string) (Event, error)
		UpdateEvent(context.Context, Event) (Event, error)
	},
	signal *SleepSignalEvent,
) *SleepSignalEvent {
	t.Helper()
	event, err := infrastructure.GetEvent(ctx, signal.Key)
	if err != nil {
		t.Fatalf("get sleep event failed: %v", err)
	}
	sleep := event.(*SleepSignalEvent)
	sleep.Until = time.Now().Add(-time.Nanosecond)
	updated, err := infrastructure.UpdateEvent(ctx, sleep)
	if err != nil {
		t.Fatalf("update sleep event failed: %v", err)
	}
	if updated == nil {
		t.Fatal("update sleep event conflicted")
	}
	return updated.(*SleepSignalEvent)
}

func TestFlowWaitsForAsyncResult_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var asyncCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(flowRuntime *WorkflowContext[struct{}, string]) string {
			result := flowRuntime.Go("async", func(goContext *GoContext[struct{}, string]) string {
				asyncCalls.Add(1)
				return goContext.GetKey() + "-output"
			})
			return result.Wait()
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted || state.Output != "async-output" {
		t.Fatalf("unexpected async state: %+v", state)
	}
	if asyncCalls.Load() != 1 {
		t.Fatalf("expected async handler once, got %d", asyncCalls.Load())
	}
	event, err := infrastructure.GetEvent(context.Background(), "async")
	if err != nil {
		t.Fatalf("get async event failed: %v", err)
	}
	if event.GetStatus() != FlowStatusCompleted || event.GetVersion() != 2 {
		t.Fatalf("unexpected async event: %+v", event)
	}
	if _, ok := event.(*AsyncEvent); !ok || event.GetEventType() != "ASYNC" {
		t.Fatalf("expected ASYNC event, got %T (%s)", event, event.GetEventType())
	}
}

func TestFlowResumesSignalInsideAsyncResult_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var asyncCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(runtime *WorkflowContext[struct{}, string]) string {
			return runtime.Go("async", func(runtime *GoContext[struct{}, string]) string {
				asyncCalls.Add(1)
				response := runtime.WaitFor[string, string](
					"approval",
					WaitForOption[string]{Request: "review"},
				)
				return "async-" + response
			}).Wait()
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)

	event, err := infrastructure.GetEvent(ctx, "async")
	if err != nil {
		t.Fatalf("get suspended async event failed: %v", err)
	}
	if event.GetStatus() != FlowStatusSuspended || event.GetVersion() != 2 {
		t.Fatalf("unexpected suspended async event: %+v", event)
	}

	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one approval signal, got %d", len(signals))
	}
	callback := *(signals[0].(*WaitForSignalEvent))
	callback.Response = json.RawMessage(`"approved"`)
	target.ResumeRun(ctx, infrastructure, &callback)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get resumed state failed: %v", err)
	}
	if state.Status != FlowStatusCompleted || state.Output != "async-approved" {
		t.Fatalf("unexpected resumed state: %+v", state)
	}
	event, err = infrastructure.GetEvent(ctx, "async")
	if err != nil {
		t.Fatalf("get completed async event failed: %v", err)
	}
	if event.GetStatus() != FlowStatusCompleted || event.GetVersion() != 4 {
		t.Fatalf("unexpected completed async event: %+v", event)
	}
	if asyncCalls.Load() != 2 {
		t.Fatalf("expected async handler to be replayed once, got %d calls", asyncCalls.Load())
	}
}

func TestRuntimeGoLoadsCompletedAsyncEventAsynchronously_BitsUT(t *testing.T) {
	storage := NewInMemoryInfrastructure[struct{}, string]()
	_, err := storage.AddEvent(context.Background(), &AsyncEvent{
		BaseEvent: BaseEvent{
			Key:    "async",
			Status: FlowStatusCompleted,
		},
		Result: json.RawMessage(`"cached-output"`),
	})
	if err != nil {
		t.Fatalf("add async event failed: %v", err)
	}

	infrastructure := &blockingGetEventInfrastructure{
		InMemoryInfrastructure: storage,
		started:                make(chan struct{}),
		release:                make(chan struct{}),
	}
	defer func() {
		select {
		case <-infrastructure.release:
		default:
			close(infrastructure.release)
		}
	}()

	workflowContext := newWorkflowContext(
		context.Background(),
		infrastructure,
		struct{}{},
	)
	returned := make(chan IResult[string], 1)
	var handlerCalls atomic.Int64
	go func() {
		returned <- workflowContext.Go("async", func(*GoContext[struct{}, string]) string {
			handlerCalls.Add(1)
			return "new-output"
		})
	}()

	select {
	case <-infrastructure.started:
	case <-time.After(time.Second):
		t.Fatal("async event lookup did not start")
	}

	var result IResult[string]
	select {
	case result = <-returned:
	case <-time.After(time.Second):
		t.Fatal("Go blocked while loading checkpoint")
	}

	close(infrastructure.release)
	if value := result.Wait(); value != "cached-output" {
		t.Fatalf("unexpected async output: %q", value)
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("expected cached async event to skip handler, got %d calls", handlerCalls.Load())
	}
}

func TestFlowFailsWhenStepReturnsError_BitsUT(t *testing.T) {
	stepErr := errors.New("step failed")
	infrastructure := NewInMemoryInfrastructure[struct{}, struct{}]()
	target := Flow[struct{}, struct{}]{
		Handler: func(runtime *WorkflowContext[struct{}, struct{}]) struct{} {
			return runtime.Step(
				"failing-step",
				func(StepContext[struct{}, struct{}]) (struct{}, error) {
					return struct{}{}, stepErr
				},
			)
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get failed state failed: %v", err)
	}
	if state.Status != FlowStatusFailed ||
		len(state.Errors) != 1 ||
		state.Errors[0] != "biz error: failing-step" {
		t.Fatalf("unexpected failed state: %+v", state)
	}
	event, err := infrastructure.GetEvent(context.Background(), "failing-step")
	if err != nil {
		t.Fatalf("get failed step event failed: %v", err)
	}
	if event.GetStatus() != FlowStatusFailed || event.GetVersion() != 2 {
		t.Fatalf("unexpected failed step event: %+v", event)
	}
}

func TestFlowPublicAPIsObserveCanceledContext_BitsUT(t *testing.T) {
	t.Run("start run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		infrastructure := NewInMemoryInfrastructure[struct{}, struct{}]()
		target := Flow[struct{}, struct{}]{
			Handler: func(*WorkflowContext[struct{}, struct{}]) struct{} {
				t.Fatal("handler must not run with canceled context")
				return struct{}{}
			},
		}

		target.StartRun(ctx, struct{}{}, infrastructure)

		errs := infrastructure.Errors()
		if len(errs) != 1 {
			t.Fatalf("expected one context error, got %v", errs)
		}
		var contextErr *ContextError
		if !errors.As(errs[0], &contextErr) || !errors.Is(errs[0], context.Canceled) {
			t.Fatalf("expected canceled context error, got %T %v", errs[0], errs[0])
		}
	})

	t.Run("workflow api", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		infrastructure := NewInMemoryInfrastructure[struct{}, string]()
		workflowContext := newWorkflowContext(ctx, infrastructure, struct{}{})
		var stepCalls atomic.Int64

		expectContextCanceledPanic(t, func() {
			workflowContext.Step(
				"blocked",
				func(StepContext[struct{}, string]) (string, error) {
					stepCalls.Add(1)
					return "unexpected", nil
				},
			)
		})

		if stepCalls.Load() != 0 {
			t.Fatalf("step handler ran despite canceled context")
		}
	})
}

func expectContextCanceledPanic(t *testing.T, run func()) {
	t.Helper()
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected context error panic")
		}
		err, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T", recovered)
		}
		var contextErr *ContextError
		if !errors.As(err, &contextErr) || !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled context error, got %T %v", err, err)
		}
	}()
	run()
}

func TestFlowFailsWhenHandlerPanicsWithNonError_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	target := Flow[struct{}, string]{
		Handler: func(*WorkflowContext[struct{}, string]) string {
			panic("boom")
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if state.Status != FlowStatusFailed ||
		len(state.Errors) != 1 ||
		state.Errors[0] != "unknown error: boom" {
		t.Fatalf("unexpected panic state: %+v", state)
	}
}

func TestFlowExecutesParallelWorkflowAcrossHumanResumes_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure[struct{}, string]()
	var handlerCalls atomic.Int64
	var initialAutoCalls atomic.Int64
	var postFirstJoinCalls atomic.Int64
	var upperTaskCalls atomic.Int64
	var lowerEntryCalls atomic.Int64
	var nestedLeftCalls atomic.Int64
	var nestedRightCalls atomic.Int64

	target := Flow[struct{}, string]{
		Handler: func(flowRuntime *WorkflowContext[struct{}, string]) string {
			handlerCalls.Add(1)

			initialAuto := flowRuntime.Go(
				"initial-auto-branch",
				func(branchRuntime *GoContext[struct{}, string]) string {
					return branchRuntime.Step(
						"initial-auto-task",
						func(StepContext[struct{}, string]) (string, error) {
							initialAutoCalls.Add(1)
							return "initial-auto", nil
						},
					)
				},
			).Wait()
			firstReview := flowRuntime.WaitFor[string, string](
				"first-review",
				WaitForOption[string]{Request: initialAuto},
			)

			postFirstJoin := flowRuntime.Step(
				"post-first-join",
				func(StepContext[struct{}, string]) (string, error) {
					postFirstJoinCalls.Add(1)
					return firstReview + "-joined", nil
				},
			)

			lowerPath := flowRuntime.Go(
				"lower-branch",
				func(branchRuntime *GoContext[struct{}, string]) string {
					lowerEntry := branchRuntime.Step(
						"lower-entry-task",
						func(StepContext[struct{}, string]) (string, error) {
							lowerEntryCalls.Add(1)
							return "lower-entry", nil
						},
					)
					nestedLeft := branchRuntime.Step(
						"nested-left-task",
						func(StepContext[struct{}, string]) (string, error) {
							nestedLeftCalls.Add(1)
							return "nested-left", nil
						},
					)
					nestedRight := branchRuntime.Step(
						"nested-right-task",
						func(StepContext[struct{}, string]) (string, error) {
							nestedRightCalls.Add(1)
							return "nested-right", nil
						},
					)
					return lowerEntry + "+" + nestedLeft + "+" + nestedRight
				},
			)
			upperTask := flowRuntime.Step(
				"upper-task",
				func(StepContext[struct{}, string]) (string, error) {
					upperTaskCalls.Add(1)
					return postFirstJoin + "-upper", nil
				},
			)
			lowerResult := lowerPath.Wait()
			secondReview := flowRuntime.WaitFor[string, string](
				"second-review",
				WaitForOption[string]{Request: upperTask + "|" + lowerResult},
			)

			return postFirstJoin + "|" + upperTask + "|" + lowerResult + "|" + secondReview
		},
	}

	ctx := context.Background()
	target.StartRun(ctx, struct{}{}, infrastructure)

	state, err := infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get first waiting state failed: %v", err)
	}
	if state.Status != FlowStatusRunning {
		t.Fatalf("expected running state at first human task, got %+v", state)
	}
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected first human signal only, got %d signals", len(signals))
	}
	firstReview := signals[0].(*WaitForSignalEvent)
	if firstReview.Key != "first-review" || string(firstReview.Request) != `"initial-auto"` {
		t.Fatalf("unexpected first human signal: %+v", firstReview)
	}
	if initialAutoCalls.Load() != 1 ||
		postFirstJoinCalls.Load() != 0 ||
		upperTaskCalls.Load() != 0 ||
		lowerEntryCalls.Load() != 0 ||
		nestedLeftCalls.Load() != 0 ||
		nestedRightCalls.Load() != 0 {
		t.Fatal("nodes after the first human task ran before it was resumed")
	}

	firstCallback := *firstReview
	firstCallback.Response = json.RawMessage(`"review-one-approved"`)
	target.ResumeRun(ctx, infrastructure, &firstCallback)

	state, err = infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get second waiting state failed: %v", err)
	}
	if state.Status != FlowStatusRunning {
		t.Fatalf("expected running state at second human task, got %+v", state)
	}
	signals = infrastructure.Signals()
	if len(signals) != 2 {
		t.Fatalf("expected two human signals, got %d", len(signals))
	}
	secondReview := signals[1].(*WaitForSignalEvent)
	expectedSecondRequest := `"review-one-approved-joined-upper|lower-entry+nested-left+nested-right"`
	if secondReview.Key != "second-review" ||
		string(secondReview.Request) != expectedSecondRequest {
		t.Fatalf("unexpected second human signal: %+v", secondReview)
	}

	secondCallback := *secondReview
	secondCallback.Response = json.RawMessage(`"review-two-approved"`)
	target.ResumeRun(ctx, infrastructure, &secondCallback)

	state, err = infrastructure.GetState(ctx)
	if err != nil {
		t.Fatalf("get completed state failed: %v", err)
	}
	expectedOutput := "review-one-approved-joined|review-one-approved-joined-upper|lower-entry+nested-left+nested-right|review-two-approved"
	if state.Status != FlowStatusCompleted || state.Output != expectedOutput {
		t.Fatalf("unexpected completed state: %+v", state)
	}
	if handlerCalls.Load() != 3 ||
		initialAutoCalls.Load() != 1 ||
		postFirstJoinCalls.Load() != 1 ||
		upperTaskCalls.Load() != 1 ||
		lowerEntryCalls.Load() != 1 ||
		nestedLeftCalls.Load() != 1 ||
		nestedRightCalls.Load() != 1 {
		t.Fatalf(
			"unexpected execution counts: handler=%d initial=%d joined=%d upper=%d lower=%d left=%d right=%d",
			handlerCalls.Load(),
			initialAutoCalls.Load(),
			postFirstJoinCalls.Load(),
			upperTaskCalls.Load(),
			lowerEntryCalls.Load(),
			nestedLeftCalls.Load(),
			nestedRightCalls.Load(),
		)
	}
	if len(infrastructure.Signals()) != 2 {
		t.Fatalf("completed nodes raised duplicate signals: %d", len(infrastructure.Signals()))
	}

	completedEventKeys := []string{
		"initial-auto-branch",
		"initial-auto-task",
		"first-review",
		"post-first-join",
		"lower-branch",
		"lower-entry-task",
		"nested-left-task",
		"nested-right-task",
		"upper-task",
		"second-review",
	}
	for _, key := range completedEventKeys {
		event, getErr := infrastructure.GetEvent(ctx, key)
		if getErr != nil {
			t.Fatalf("get event %s failed: %v", key, getErr)
		}
		if event == nil ||
			event.GetStatus() != FlowStatusCompleted ||
			event.GetVersion() != 2 {
			t.Fatalf("unexpected completed event %s: %+v", key, event)
		}
	}
}
