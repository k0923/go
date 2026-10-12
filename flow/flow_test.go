package flow

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlowRunsStepAndReusesFinishedState_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	var handlerCalls atomic.Int64
	var stepCalls atomic.Int64
	target := Flow[string, string]{
		Handler: func(ctx *FlowContext[string, string]) string {
			handlerCalls.Add(1)
			return ctx.Step("echo", StepOption[string]{
				Handler: func(StepContext) (string, error) {
					stepCalls.Add(1)
					return ctx.GetInput() + "-output", nil
				},
			})
		},
	}

	target.StartRun(context.Background(), "input", infrastructure)
	target.StartRun(context.Background(), "ignored", infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if state.Status != FlowStatusFinished || string(state.Output) != `"input-output"` {
		t.Fatalf("unexpected final state: %+v", state)
	}
	if handlerCalls.Load() != 1 || stepCalls.Load() != 1 {
		t.Fatalf(
			"expected handler and step once, got handler=%d step=%d",
			handlerCalls.Load(),
			stepCalls.Load(),
		)
	}
	event, err := infrastructure.GetEvent(context.Background(), nil, "echo")
	if err != nil {
		t.Fatalf("get step event failed: %v", err)
	}
	if event.GetStatus() != FlowStatusFinished || event.GetVersion() != 2 {
		t.Fatalf("unexpected step event: %+v", event)
	}
}

func TestFlowResumesWaitForUsingStoredInput_BitsUT(t *testing.T) {
	type input struct {
		Customer string `json:"customer"`
	}
	infrastructure := NewInMemoryInfrastructure()
	var stepCalls atomic.Int64
	target := Flow[input, string]{
		Handler: func(ctx *FlowContext[input, string]) string {
			prepared := ctx.Step("prepare", StepOption[string]{
				Handler: func(StepContext) (string, error) {
					stepCalls.Add(1)
					return ctx.GetInput().Customer + "-prepared", nil
				},
			})
			approval := ctx.WaitFor[string, string](
				"approval",
				WaitForOption[string]{Request: prepared},
			)
			return prepared + ":" + approval
		},
	}

	target.StartRun(
		context.Background(),
		input{Customer: "stored"},
		infrastructure,
	)
	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get paused state failed: %v", err)
	}
	if state.Status != FlowStatusRunning {
		t.Fatalf("expected running state while waiting, got %s", state.Status)
	}
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one signal, got %d", len(signals))
	}
	awaited := signals[0].(*WaitForSignal)
	callback := *awaited
	callback.Response = json.RawMessage(`"approved"`)

	target.ResumeRun(context.Background(), infrastructure, &callback)

	state, err = infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get resumed state failed: %v", err)
	}
	if state.Status != FlowStatusFinished ||
		string(state.Output) != `"stored-prepared:approved"` {
		t.Fatalf("unexpected resumed state: %+v", state)
	}
	if stepCalls.Load() != 1 {
		t.Fatalf("expected completed step to be reused, got %d calls", stepCalls.Load())
	}
}

func TestFlowResumesSleepAfterDeadline_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	var handlerCalls atomic.Int64
	target := Flow[struct{}, string]{
		Handler: func(ctx *FlowContext[struct{}, string]) string {
			handlerCalls.Add(1)
			ctx.Sleep("short-sleep", 15*time.Millisecond)
			return "finished"
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)
	signals := infrastructure.Signals()
	if len(signals) != 1 {
		t.Fatalf("expected one sleep signal, got %d", len(signals))
	}
	sleep := signals[0].(*SleepSignal)
	time.Sleep(time.Until(sleep.Until) + 5*time.Millisecond)
	target.ResumeRun(context.Background(), infrastructure, sleep)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get resumed state failed: %v", err)
	}
	if state.Status != FlowStatusFinished || string(state.Output) != `"finished"` {
		t.Fatalf("unexpected sleep state: %+v", state)
	}
	if handlerCalls.Load() != 2 {
		t.Fatalf("expected initial and resumed handler calls, got %d", handlerCalls.Load())
	}
}

func TestFlowParallelIsolatesBranches_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	target := Flow[struct{}, string]{
		Handler: func(ctx *FlowContext[struct{}, string]) string {
			ctx.Parallel(
				"group",
				Branch[struct{}, string]{
					Key: "first",
					Handler: func(ctx *FlowContext[struct{}, string]) {
						ctx.Step("shared", StepOption[string]{
							Handler: func(StepContext) (string, error) {
								return "first", nil
							},
						})
					},
				},
				Branch[struct{}, string]{
					Key: "second",
					Handler: func(ctx *FlowContext[struct{}, string]) {
						ctx.Step("shared", StepOption[string]{
							Handler: func(StepContext) (string, error) {
								return "second", nil
							},
						})
					},
				},
			)
			return "finished"
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)

	for _, branch := range []string{"first", "second"} {
		event, err := infrastructure.GetEvent(
			context.Background(),
			[]string{"group", branch},
			"shared",
		)
		if err != nil {
			t.Fatalf("get %s branch event failed: %v", branch, err)
		}
		if event == nil || event.GetStatus() != FlowStatusFinished {
			t.Fatalf("unexpected %s branch event: %+v", branch, event)
		}
	}
	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if state.Status != FlowStatusFinished {
		t.Fatalf("expected finished state, got %s", state.Status)
	}
}

func TestFlowParallelCollectsBranchErrors_BitsUT(t *testing.T) {
	firstErr := errors.New("first failed")
	secondErr := errors.New("second failed")
	infrastructure := NewInMemoryInfrastructure()
	target := Flow[struct{}, struct{}]{
		Handler: func(ctx *FlowContext[struct{}, struct{}]) struct{} {
			ctx.Parallel(
				"group",
				Branch[struct{}, struct{}]{
					Key: "first",
					Handler: func(ctx *FlowContext[struct{}, struct{}]) {
						ctx.Step("step", StepOption[struct{}]{
							Handler: func(StepContext) (struct{}, error) {
								return struct{}{}, firstErr
							},
						})
					},
				},
				Branch[struct{}, struct{}]{
					Key: "second",
					Handler: func(ctx *FlowContext[struct{}, struct{}]) {
						ctx.Step("step", StepOption[struct{}]{
							Handler: func(StepContext) (struct{}, error) {
								return struct{}{}, secondErr
							},
						})
					},
				},
			)
			return struct{}{}
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get failed state failed: %v", err)
	}
	if state.Status != FlowStatusFailed || len(state.Errors) != 2 {
		t.Fatalf("unexpected failed state: %+v", state)
	}
}

func TestFlowFailsWhenParallelBranchPanicsWithNonError_BitsUT(t *testing.T) {
	infrastructure := NewInMemoryInfrastructure()
	target := Flow[struct{}, string]{
		Handler: func(ctx *FlowContext[struct{}, string]) string {
			ctx.Parallel(
				"group",
				Branch[struct{}, string]{
					Key: "panic",
					Handler: func(*FlowContext[struct{}, string]) {
						panic("boom")
					},
				},
			)
			return "should-not-finish"
		},
	}

	target.StartRun(context.Background(), struct{}{}, infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if state.Status != FlowStatusFailed {
		t.Fatalf("expected failed state after branch panic, got %+v", state)
	}
}
