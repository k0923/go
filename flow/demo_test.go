package flow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type testInfrastructure struct {
	lock          sync.Mutex
	state         State
	events        []Event
	processSignal func(signal Signal)
}

func (infrastructure *testInfrastructure) AppendEvent(_ context.Context, event Event, expectedIndex int) (bool, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	eventCount := 0
	for _, storedEvent := range infrastructure.events {
		if slices.Equal(storedEvent.GetBranches(), event.GetBranches()) {
			eventCount++
		}
	}
	if eventCount != expectedIndex {
		return false, nil
	}
	infrastructure.events = append(infrastructure.events, event)
	return true, nil
}

func (infrastructure *testInfrastructure) GetState(context.Context) (State, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	return infrastructure.state, nil
}

func (infrastructure *testInfrastructure) SetState(_ context.Context, state State) error {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.state = state
	return nil
}

func (infrastructure *testInfrastructure) GetEvents(context.Context) ([]Event, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	return append([]Event(nil), infrastructure.events...), nil
}

func (infrastructure *testInfrastructure) ProcessSignal(_ context.Context, signal Signal) {
	if infrastructure.processSignal != nil {
		infrastructure.processSignal(signal)
	}
}

func TestResumeRunRejectsUnknownWaitForEvent(t *testing.T) {
	infrastructure := &testInfrastructure{
		state: State{
			Input:  json.RawMessage(`{}`),
			Status: FlowStatusRunning,
		},
	}
	flow := Flow[struct{}, struct{}]{
		Handler: func(*FlowContext[struct{}, struct{}]) struct{} {
			return struct{}{}
		},
	}

	err := flow.ResumeRun(context.Background(), infrastructure, WaitForSignal{Name: "missing"})

	if !errors.Is(err, ErrWaitForEventNotFound) {
		t.Fatalf("expected ErrWaitForEventNotFound, got %v", err)
	}
}

func TestResumeRunMatchesWaitForEvent(t *testing.T) {
	waitForEvent := new(WaitForEvent)
	waitForEvent.Name = "approval"
	waitForEvent.Request = json.RawMessage(`"request"`)
	infrastructure := &testInfrastructure{
		state: State{
			Input:  json.RawMessage(`{}`),
			Status: FlowStatusRunning,
		},
		events: []Event{waitForEvent},
	}
	flow := Flow[struct{}, string]{
		Handler: func(ctx *FlowContext[struct{}, string]) string {
			return ctx.WaitFor[string, string](
				"approval",
				WaitForOption[string]{Request: "request"},
			)
		},
	}

	err := flow.ResumeRun(
		context.Background(),
		infrastructure,
		WaitForSignal{Name: "approval", Response: json.RawMessage(`"approved"`)},
	)

	if err != nil {
		t.Fatalf("resume run failed: %v", err)
	}
	state, _ := infrastructure.GetState(context.Background())
	if state.Status != FlowStatusFinished {
		t.Fatalf("expected flow to finish, got %s", state.Status)
	}
	if string(state.Output) != `"approved"` {
		t.Fatalf("expected approved output, got %s", state.Output)
	}
}

func TestRunWaitsForSignalProcessing(t *testing.T) {
	signalStarted := make(chan struct{})
	releaseSignal := make(chan struct{})
	infrastructure := &testInfrastructure{
		processSignal: func(Signal) {
			close(signalStarted)
			<-releaseSignal
		},
	}
	flow := Flow[struct{}, struct{}]{
		Handler: func(ctx *FlowContext[struct{}, struct{}]) struct{} {
			ctx.WaitFor[string, string](
				"approval",
				WaitForOption[string]{Request: "request"},
			)
			return struct{}{}
		},
	}
	runResult := make(chan error, 1)
	go func() {
		runResult <- flow.StartRun(context.Background(), struct{}{}, infrastructure)
	}()

	select {
	case <-signalStarted:
	case <-time.After(time.Second):
		t.Fatal("signal processing did not start")
	}
	select {
	case err := <-runResult:
		close(releaseSignal)
		t.Fatalf("run returned before signal processing completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseSignal)
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("run failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not return after signal processing completed")
	}
}

func TestParallelRejectsDuplicateBranchKeys(t *testing.T) {
	infrastructure := &testInfrastructure{}
	flow := Flow[struct{}, struct{}]{
		Handler: func(ctx *FlowContext[struct{}, struct{}]) struct{} {
			ctx.Parallel(
				"group",
				Branch[struct{}, struct{}]{Key: "duplicate", Handler: func(*FlowContext[struct{}, struct{}]) {}},
				Branch[struct{}, struct{}]{Key: "duplicate", Handler: func(*FlowContext[struct{}, struct{}]) {}},
			)
			return struct{}{}
		},
	}

	err := flow.StartRun(context.Background(), struct{}{}, infrastructure)

	if !errors.Is(err, ErrDuplicateBranchKey) {
		t.Fatalf("expected ErrDuplicateBranchKey, got %v", err)
	}
	state, _ := infrastructure.GetState(context.Background())
	if state.Status != FlowStatusFailed {
		t.Fatalf("expected failed state, got %s", state.Status)
	}
}

func TestParallelReturnsWhenBranchRaisesSignal(t *testing.T) {
	stepStarted := make(chan struct{})
	releaseStep := make(chan struct{})
	signalProcessed := make(chan struct{})
	var closeSignalProcessed sync.Once
	infrastructure := &testInfrastructure{
		processSignal: func(Signal) {
			closeSignalProcessed.Do(func() {
				close(signalProcessed)
			})
		},
	}
	flow := Flow[struct{}, struct{}]{
		Handler: func(ctx *FlowContext[struct{}, struct{}]) struct{} {
			ctx.Parallel(
				"group",
				Branch[struct{}, struct{}]{
					Key: "step",
					Handler: func(ctx *FlowContext[struct{}, struct{}]) {
						ctx.Step(
							"slow",
							StepOption[string]{
								Handler: func(StepContext) (string, error) {
									close(stepStarted)
									<-releaseStep
									return "done", nil
								},
							},
						)
					},
				},
				Branch[struct{}, struct{}]{
					Key: "wait",
					Handler: func(ctx *FlowContext[struct{}, struct{}]) {
						<-stepStarted
						ctx.WaitFor[string, string](
							"approval",
							WaitForOption[string]{Request: "request"},
						)
					},
				},
			)
			return struct{}{}
		},
	}

	runResult := make(chan error, 1)
	go func() {
		runResult <- flow.StartRun(context.Background(), struct{}{}, infrastructure)
	}()

	select {
	case <-signalProcessed:
	case <-time.After(time.Second):
		close(releaseStep)
		t.Fatal("signal processing did not start")
	}
	select {
	case err := <-runResult:
		if err != nil {
			close(releaseStep)
			t.Fatalf("run failed: %v", err)
		}
	case <-time.After(20 * time.Millisecond):
		close(releaseStep)
		t.Fatal("parallel waited for the running step after a branch raised signal")
	}
	close(releaseStep)
}
