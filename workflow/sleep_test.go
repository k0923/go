package workflow

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

func expireScheduledSleep[Input any, Output any](
	t *testing.T,
	store *InMemoryWorkflowStore[Input, Output],
	runID string,
	name string,
) {
	t.Helper()
	events, err := store.GetEvents(runID)
	if err != nil {
		t.Fatalf("failed to read events: %v", err)
	}
	for _, event := range events {
		scheduled, ok := event.(*SleepScheduledEvent)
		if ok && scheduled.Name == name {
			scheduled.Until = time.Now().Add(-time.Second)
			return
		}
	}
	t.Fatalf("sleep event %q not found", name)
}

func TestSleepWaitsInProcess(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, string]()
	startedAt := time.Now()

	err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID: "sleep-in-process",
		Store: store,
		Workflow: Workflow[struct{}, string]{
			Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
				if err := ctx.Sleep(15*time.Millisecond, SleepOption{
					Name:               "short-sleep",
					InProcessThreshold: 100 * time.Millisecond,
				}); err != nil {
					return "", err
				}
				return "finished", nil
			},
		},
	})
	if err != nil {
		t.Fatalf("expected in-process sleep to finish, got %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed < 10*time.Millisecond {
		t.Fatalf("expected workflow to wait in process, elapsed %s", elapsed)
	}

	state, err := store.GetRunState("sleep-in-process")
	if err != nil {
		t.Fatalf("failed to read run state: %v", err)
	}
	if state.Status != RunStatusFinished || state.Output != "finished" {
		t.Fatalf("unexpected final state: %+v", state)
	}

	events, err := store.GetEvents("sleep-in-process")
	if err != nil {
		t.Fatalf("failed to read events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one sleep event, got %d", len(events))
	}
	scheduled, ok := events[0].(*SleepScheduledEvent)
	if !ok {
		t.Fatalf("expected SleepScheduledEvent, got %T", events[0])
	}
	if scheduled.Suspended {
		t.Fatal("expected short sleep not to be marked as suspended")
	}
}

func TestSleepPausesAndResumesAfterDeadline(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, string]()
	handlerCalls := 0
	continued := 0
	workflow := Workflow[struct{}, string]{
		Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
			handlerCalls++
			if err := ctx.Sleep(40*time.Millisecond, SleepOption{
				Name:               "deferred-sleep",
				InProcessThreshold: 5 * time.Millisecond,
			}); err != nil {
				return "", err
			}
			continued++
			return "resumed", nil
		},
	}

	startedAt := time.Now()
	err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID:    "sleep-deferred",
		Store:    store,
		Workflow: workflow,
	})
	if err != nil {
		t.Fatalf("expected long sleep to pause without an error, got %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 30*time.Millisecond {
		t.Fatalf("expected long sleep to pause immediately, elapsed %s", elapsed)
	}
	state, err := store.GetRunState("sleep-deferred")
	if err != nil {
		t.Fatalf("failed to read paused state: %v", err)
	}
	if state.Status != RunStatusPaused {
		t.Fatalf("expected paused state, got %s", state.Status)
	}
	if continued != 0 {
		t.Fatalf("expected workflow not to continue before resume, got %d continuations", continued)
	}

	events, err := store.GetEvents("sleep-deferred")
	if err != nil {
		t.Fatalf("failed to read events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one sleep event, got %d", len(events))
	}
	scheduled, ok := events[0].(*SleepScheduledEvent)
	if !ok {
		t.Fatalf("expected SleepScheduledEvent, got %T", events[0])
	}
	if !scheduled.Suspended {
		t.Fatal("expected long sleep to be marked as suspended")
	}

	err = ResumeRun(ResumeRunOption[struct{}, string]{
		RunID:    "sleep-deferred",
		Store:    store,
		Workflow: workflow,
	})
	if err != nil {
		t.Fatalf("expected early resume to pause again, got %v", err)
	}
	state, err = store.GetRunState("sleep-deferred")
	if err != nil {
		t.Fatalf("failed to read state after early resume: %v", err)
	}
	if state.Status != RunStatusPaused {
		t.Fatalf("expected early resume to remain paused, got %s", state.Status)
	}
	if continued != 0 {
		t.Fatalf("expected early resume not to continue workflow, got %d continuations", continued)
	}

	time.Sleep(time.Until(scheduled.Until) + 5*time.Millisecond)
	err = ResumeRun(ResumeRunOption[struct{}, string]{
		RunID:    "sleep-deferred",
		Store:    store,
		Workflow: workflow,
	})
	if err != nil {
		t.Fatalf("expected sleep workflow to resume, got %v", err)
	}
	if handlerCalls != 3 {
		t.Fatalf("expected initial, early, and due handler calls, got %d", handlerCalls)
	}
	if continued != 1 {
		t.Fatalf("expected workflow to continue once, got %d", continued)
	}
	events, err = store.GetEvents("sleep-deferred")
	if err != nil {
		t.Fatalf("failed to read resumed events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected sleep event to be replayed instead of duplicated, got %d events", len(events))
	}
	state, err = store.GetRunState("sleep-deferred")
	if err != nil {
		t.Fatalf("failed to read finished state: %v", err)
	}
	if state.Status != RunStatusFinished || state.Output != "resumed" {
		t.Fatalf("unexpected resumed state: %+v", state)
	}
}

func TestSleepUntilPastDeadlineReturnsImmediately(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, string]()

	err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID: "sleep-until-past",
		Store: store,
		Workflow: Workflow[struct{}, string]{
			Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
				if err := ctx.SleepUntil(time.Now().Add(-time.Second), SleepOption{
					Name:               "past-deadline",
					InProcessThreshold: time.Millisecond,
				}); err != nil {
					return "", err
				}
				return "finished", nil
			},
		},
	})
	if err != nil {
		t.Fatalf("expected past deadline to return immediately, got %v", err)
	}
	state, err := store.GetRunState("sleep-until-past")
	if err != nil {
		t.Fatalf("failed to read final state: %v", err)
	}
	if state.Status != RunStatusFinished {
		t.Fatalf("expected finished state, got %s", state.Status)
	}
}

func TestSleepRemainsPausedWhenHandlerRecoversSignal(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()

	err := RunWorkflow(WorkflowOption[struct{}, struct{}]{
		RunID: "sleep-recovered-signal",
		Store: store,
		Workflow: Workflow[struct{}, struct{}]{
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) (struct{}, error) {
				defer func() {
					_ = recover()
				}()
				_ = ctx.Sleep(time.Hour, SleepOption{
					Name:               "recovered-sleep",
					InProcessThreshold: time.Second,
				})
				return struct{}{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("expected recovered sleep signal not to escape, got %v", err)
	}
	state, err := store.GetRunState("sleep-recovered-signal")
	if err != nil {
		t.Fatalf("failed to read paused state: %v", err)
	}
	if state.Status != RunStatusPaused {
		t.Fatalf("expected paused state, got %s", state.Status)
	}
}

func TestSleepValidatesOptions(t *testing.T) {
	ctx := &WorkflowContext[struct{}, struct{}]{}

	if err := ctx.Sleep(0, SleepOption{}); !errors.Is(err, ErrSleepNameRequired) {
		t.Fatalf("expected missing sleep name error, got %v", err)
	}
	if err := ctx.Sleep(0, SleepOption{
		Name:               "invalid-threshold",
		InProcessThreshold: -time.Nanosecond,
	}); !errors.Is(err, ErrSleepThresholdNegative) {
		t.Fatalf("expected negative threshold error, got %v", err)
	}
}

func TestStepParallelSleepCombinationResumesBranchesInDeadlineOrder(t *testing.T) {
	const runID = "step-parallel-sleep-combination"

	store := NewInMemoryWorkflowStore[struct{}, string]()
	var callsLock sync.Mutex
	stepCalls := make(map[string]int)
	runStep := func(ctx *WorkflowContext[struct{}, string], name string) error {
		_, err := ctx.Step(name, StepOption[struct{}]{
			Handler: func(ctx *StepContext) (struct{}, error) {
				callsLock.Lock()
				stepCalls[name]++
				callsLock.Unlock()
				return struct{}{}, nil
			},
		})
		return err
	}

	sleepBranch := func(branchID string) Branch[struct{}, string] {
		return Branch[struct{}, string]{
			BranchID: branchID,
			Handler: func(ctx *WorkflowContext[struct{}, string]) error {
				if err := runStep(ctx, branchID+"_before_sleep"); err != nil {
					return err
				}
				if err := ctx.Sleep(time.Hour, SleepOption{
					Name:               branchID + "-sleep",
					InProcessThreshold: time.Second,
				}); err != nil {
					return err
				}
				return runStep(ctx, branchID+"_after_sleep")
			},
		}
	}

	workflow := Workflow[struct{}, string]{
		Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
			if err := runStep(ctx, "before_parallel"); err != nil {
				return "", err
			}
			if err := ctx.Parallel(
				ParallelOption{Name: "sleep-parallel"},
				sleepBranch("first"),
				Branch[struct{}, string]{
					BranchID: "immediate",
					Handler: func(ctx *WorkflowContext[struct{}, string]) error {
						return runStep(ctx, "immediate")
					},
				},
				sleepBranch("second"),
			); err != nil {
				return "", err
			}
			if err := runStep(ctx, "after_parallel"); err != nil {
				return "", err
			}
			return "finished", nil
		},
	}

	if err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID:    runID,
		Store:    store,
		Workflow: workflow,
	}); err != nil {
		t.Fatalf("failed to start workflow: %v", err)
	}
	state, err := store.GetRunState(runID)
	if err != nil {
		t.Fatalf("failed to read initial state: %v", err)
	}
	if state.Status != RunStatusPaused {
		t.Fatalf("expected initial run to pause, got %s", state.Status)
	}
	initialExpected := map[string]int{
		"before_parallel":     1,
		"first_before_sleep":  1,
		"immediate":           1,
		"second_before_sleep": 1,
	}
	for name, expected := range initialExpected {
		if stepCalls[name] != expected {
			t.Fatalf("expected %s to run %d time(s), got %d", name, expected, stepCalls[name])
		}
	}

	expireScheduledSleep(t, store, runID, "first-sleep")
	resumeErr := ResumeRun(ResumeRunOption[struct{}, string]{
		RunID:    runID,
		Store:    store,
		Workflow: workflow,
	})
	if resumeErr != nil {
		t.Fatalf("failed first resume: %v", resumeErr)
	}
	state, err = store.GetRunState(runID)
	if err != nil {
		t.Fatalf("failed to read state after first resume: %v", err)
	}
	if state.Status != RunStatusPaused {
		t.Fatalf("expected second sleep to keep workflow paused, got %s", state.Status)
	}
	if stepCalls["first_after_sleep"] != 1 {
		t.Fatalf("expected first branch to continue once, got %d", stepCalls["first_after_sleep"])
	}
	if stepCalls["second_after_sleep"] != 0 || stepCalls["after_parallel"] != 0 {
		t.Fatalf(
			"expected second branch and join continuation to remain pending, second=%d after_parallel=%d",
			stepCalls["second_after_sleep"],
			stepCalls["after_parallel"],
		)
	}

	expireScheduledSleep(t, store, runID, "second-sleep")
	resumeErr = ResumeRun(ResumeRunOption[struct{}, string]{
		RunID:    runID,
		Store:    store,
		Workflow: workflow,
	})
	if resumeErr != nil {
		t.Fatalf("failed second resume: %v", resumeErr)
	}
	state, err = store.GetRunState(runID)
	if err != nil {
		t.Fatalf("failed to read final state: %v", err)
	}
	if state.Status != RunStatusFinished || state.Output != "finished" {
		t.Fatalf("unexpected final state: %+v", state)
	}

	expectedCalls := map[string]int{
		"before_parallel":     1,
		"first_before_sleep":  1,
		"first_after_sleep":   1,
		"immediate":           1,
		"second_before_sleep": 1,
		"second_after_sleep":  1,
		"after_parallel":      1,
	}
	for name, expected := range expectedCalls {
		if stepCalls[name] != expected {
			t.Errorf("expected %s to run %d time(s), got %d", name, expected, stepCalls[name])
		}
	}

	events, err := store.GetEvents(runID)
	if err != nil {
		t.Fatalf("failed to read final events: %v", err)
	}
	eventCounts := make(map[string]int)
	var firstSleep *SleepScheduledEvent
	var firstAfterSleep *StepStartEvent
	var firstCompleted *BranchCompletedEvent
	var parallelCompleted *ParallelCompletedEvent
	var afterParallel *StepStartEvent
	for _, event := range events {
		eventCounts[event.GetType()]++
		switch event := event.(type) {
		case *SleepScheduledEvent:
			if event.Name == "first-sleep" {
				firstSleep = event
			}
		case *StepStartEvent:
			if event.Name == "first_after_sleep" {
				firstAfterSleep = event
			}
			if event.Name == "after_parallel" {
				afterParallel = event
			}
		case *BranchCompletedEvent:
			if event.BranchID == "first" {
				firstCompleted = event
			}
		case *ParallelCompletedEvent:
			if event.Name == "sleep-parallel" && len(event.BranchIds) == 0 {
				parallelCompleted = event
			}
		}
	}
	if eventCounts[EventTypeStepStart] != len(expectedCalls) ||
		eventCounts[EventTypeStepFinish] != len(expectedCalls) ||
		eventCounts[EventTypeSleepScheduled] != 2 ||
		eventCounts[EventTypeParallelStarted] != 1 ||
		eventCounts[EventTypeBranchCompleted] != 3 ||
		eventCounts[EventTypeParallelCompleted] != 1 {
		t.Fatalf("unexpected event counts: %v", eventCounts)
	}
	if firstSleep == nil || firstAfterSleep == nil || firstCompleted == nil ||
		parallelCompleted == nil || afterParallel == nil {
		t.Fatal("expected combination lifecycle events to be present")
	}
	assertEventPrevious(t, firstAfterSleep, IdentityOf(firstSleep))
	assertEventPrevious(t, firstCompleted, EventIdentity{
		Name:      "first_after_sleep",
		BranchIds: []string{"first"},
		Type:      EventTypeStepFinish,
	})
	assertEventPrevious(t, afterParallel, IdentityOf(parallelCompleted))
	if !slices.Equal(firstSleep.BranchIds, []string{"first"}) {
		t.Fatalf("unexpected first sleep branch path: %v", firstSleep.BranchIds)
	}
}

func TestParallelCollectsSleepSignalsBeforePausing(t *testing.T) {
	ctx, store := newParallelTestContext()
	ctx.Context = context.Background()
	ctx.runtime.state = &RunState[struct{}, struct{}]{
		Status: RunStatusRunning,
	}
	option := ParallelOption{Name: "collect-sleep-signals"}
	completedBranchRan := false

	defer func() {
		recovered := recover()
		if _, ok := recovered.(*branchSignal); !ok {
			t.Fatalf("expected branchSignal, got %v", recovered)
		}
		if !completedBranchRan {
			t.Fatal("expected Parallel to collect all branch results before pausing")
		}
		if remaining := branchCount(
			store,
			"parallel-test",
			ctx.branchGroupID(option.Name),
		); remaining != 2 {
			t.Fatalf("expected two sleeping branches to remain, got %d", remaining)
		}

		events, err := store.GetEvents("parallel-test")
		if err != nil {
			t.Fatalf("failed to read events: %v", err)
		}
		eventCounts := make(map[string]int)
		for _, event := range events {
			eventCounts[event.GetType()]++
		}
		if eventCounts[EventTypeSleepScheduled] != 2 ||
			eventCounts[EventTypeBranchCompleted] != 1 ||
			eventCounts[EventTypeParallelCompleted] != 0 {
			t.Fatalf("unexpected event counts: %v", eventCounts)
		}
	}()

	err := ctx.Parallel(
		option,
		Branch[struct{}, struct{}]{
			BranchID: "first-sleep",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				return ctx.Sleep(time.Hour, SleepOption{
					Name:               "first-sleep-operation",
					InProcessThreshold: time.Second,
				})
			},
		},
		Branch[struct{}, struct{}]{
			BranchID: "complete",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				completedBranchRan = true
				return nil
			},
		},
		Branch[struct{}, struct{}]{
			BranchID: "second-sleep",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				return ctx.Sleep(time.Hour, SleepOption{
					Name:               "second-sleep-operation",
					InProcessThreshold: time.Second,
				})
			},
		},
	)
	t.Fatalf("expected Parallel to propagate branchSignal, got error %v", err)
}
