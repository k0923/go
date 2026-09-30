package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

type checkoutInput struct {
	CustomerID string
	Items      []string
}

type checkoutOutput struct {
	ReservationID  string
	ConfirmationID string
}

type reservationResult struct {
	ID       string
	Quantity int
}

type confirmationResult struct {
	ID            string
	ReservationID string
}

func readRunState[Input any, Output any](
	t *testing.T,
	store *InMemoryWorkflowStore[Input, Output],
	runID string,
) *RunState[Input, Output] {
	t.Helper()
	state, err := store.GetRunState(runID)
	if err != nil {
		t.Fatalf("failed to read run state: %v", err)
	}
	return state
}

func readEvents[Input any, Output any](
	t *testing.T,
	store *InMemoryWorkflowStore[Input, Output],
	runID string,
) []Event {
	t.Helper()
	events, err := store.GetEvents(runID)
	if err != nil {
		t.Fatalf("failed to read events: %v", err)
	}
	return events
}

func seedRunState[Input any, Output any](
	store *InMemoryWorkflowStore[Input, Output],
	runID string,
	state *RunState[Input, Output],
) {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()
	store.states[runID] = cloneRunState(state)
}

func appendReplayEvent[Input any, Output any](
	store *InMemoryWorkflowStore[Input, Output],
	runID string,
	event Event,
) {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()
	store.saveEventLocked(runID, event)
}

func branchCount[Input any, Output any](
	store *InMemoryWorkflowStore[Input, Output],
	runID string,
	branchGroupID string,
) int {
	store.lock.RLock()
	defer store.lock.RUnlock()
	group := store.branchGroups[inMemoryBranchGroupKey{
		runID:         runID,
		branchGroupID: branchGroupID,
	}]
	if group == nil {
		return 0
	}
	return len(group.remaining)
}

func invalidateLease[Input any, Output any](
	store *InMemoryWorkflowStore[Input, Output],
	lease ExecutionLease,
) {
	store.lock.Lock()
	defer store.lock.Unlock()
	continuation := store.continuations[newInMemoryContinuationKey(lease.Key)]
	if continuation != nil {
		continuation.generation++
	}
}

func newParallelTestContext() (*WorkflowContext[struct{}, struct{}], *InMemoryWorkflowStore[struct{}, struct{}]) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	state, lease, err := store.CreateRun(
		"parallel-test",
		struct{}{},
		newRuntimeID(),
		defaultLeaseDuration,
	)
	if err != nil {
		panic(err)
	}
	runtime := &workflowRuntime[struct{}, struct{}]{
		runID:         "parallel-test",
		ownerID:       lease.OwnerID,
		initialLease:  lease,
		state:         state,
		store:         store,
		consumed:      make(map[int]bool),
		activeLeases:  make(map[string]ExecutionLease),
		leaseDuration: defaultLeaseDuration,
	}
	runtime.trackLease(lease)
	leaseCopy := lease
	return &WorkflowContext[struct{}, struct{}]{
		Context: context.Background(),
		runtime: runtime,
		lease:   &leaseCopy,
	}, store
}

func TestWorkflowContextBranchGroupID(t *testing.T) {
	ctx := &WorkflowContext[struct{}, struct{}]{}
	if id := ctx.branchGroupID("parallel"); id != "parallel" {
		t.Fatalf("expected root branch group id to be parallel, got %q", id)
	}

	branchCtx := &WorkflowContext[struct{}, struct{}]{
		branchIds: []string{"outer", "inner"},
	}
	if id := branchCtx.branchGroupID("parallel"); id != "parallel:outer.inner" {
		t.Fatalf("expected nested branch group id to include branch path, got %q", id)
	}
}

func newParallelStartedEvent(name string, branchIDs ...string) *ParallelStartedEvent {
	return &ParallelStartedEvent{
		EventBase: EventBase{Name: name},
		Branches:  branchIDs,
	}
}

func newBranchCompletedEvent(name string, branchID string) *BranchCompletedEvent {
	return &BranchCompletedEvent{
		EventBase: EventBase{
			Name:      name,
			BranchIds: []string{branchID},
		},
		BranchID: branchID,
	}
}

func newParallelCompletedEvent(name string) *ParallelCompletedEvent {
	return &ParallelCompletedEvent{
		EventBase: EventBase{Name: name},
	}
}

func assertEventPrevious(t *testing.T, event Event, expected EventIdentity) {
	t.Helper()
	previous := event.GetBase().Previous
	if previous == nil {
		t.Fatalf("expected %s event to have a previous identity", event.GetType())
	}
	if previous.Name != expected.Name ||
		previous.Type != expected.Type ||
		!slices.Equal(previous.BranchIds, expected.BranchIds) {
		t.Fatalf("unexpected previous identity: got %+v, expected %+v", *previous, expected)
	}
}

func runCheckoutWorkflow(store WorkflowStore[checkoutInput, checkoutOutput]) (checkoutOutput, int, error) {
	input := checkoutInput{
		CustomerID: "customer-001",
		Items:      []string{"book", "pen"},
	}
	stepCalls := 0
	var output checkoutOutput

	err := RunWorkflow(WorkflowOption[checkoutInput, checkoutOutput]{
		RunID: "checkout-001",
		Store: store,
		Input: input,
		Workflow: Workflow[checkoutInput, checkoutOutput]{
			Handler: func(ctx *WorkflowContext[checkoutInput, checkoutOutput]) (checkoutOutput, error) {
				reservation, err := ctx.Step("reserve_inventory", StepOption[reservationResult]{
					Handler: func(stepCtx *StepContext) (reservationResult, error) {
						stepCalls++
						return reservationResult{
							ID:       stepCtx.GetName(),
							Quantity: len(ctx.input.Items),
						}, nil
					},
				})
				if err != nil {
					return checkoutOutput{}, err
				}

				confirmation, err := ctx.Step("confirm_order", StepOption[confirmationResult]{
					Handler: func(stepCtx *StepContext) (confirmationResult, error) {
						stepCalls++
						return confirmationResult{
							ID:            stepCtx.GetName(),
							ReservationID: reservation.ID,
						}, nil
					},
				})
				if err != nil {
					return checkoutOutput{}, err
				}

				output = checkoutOutput{
					ReservationID:  reservation.ID,
					ConfirmationID: confirmation.ID,
				}
				return output, nil
			},
		},
	})

	return output, stepCalls, err
}

func runRetryWorkflow(store WorkflowStore[struct{}, string], fail bool) (string, int, error) {
	stepCalls := 0
	var output string

	workflow := Workflow[struct{}, string]{
		Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
			result, err := ctx.Step("fragile_step", StepOption[string]{
				Handler: func(stepCtx *StepContext) (string, error) {
					stepCalls++
					if fail {
						return "", errors.New("temporary failure")
					}
					return stepCtx.GetName(), nil
				},
			})
			if err != nil {
				return "", err
			}

			output = result
			return output, nil
		},
	}
	state, err := store.GetRunState("retry-001")
	if err != nil {
		return "", 0, err
	}
	if state == nil {
		err = RunWorkflow(WorkflowOption[struct{}, string]{
			RunID:    "retry-001",
			Store:    store,
			Workflow: workflow,
		})
	} else {
		err = ResumeRun(ResumeRunOption[struct{}, string]{
			RunID:    "retry-001",
			Store:    store,
			Workflow: workflow,
		})
	}

	return output, stepCalls, err
}

func TestStepRetriesFailedStep(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, string]()

	_, stepCalls, err := runRetryWorkflow(store, true)
	if err == nil {
		t.Fatal("expected first run to fail")
	}
	if stepCalls != 1 {
		t.Fatalf("expected first run to call step once, got %d", stepCalls)
	}
	events := readEvents(t, store, "retry-001")
	if len(events) != 2 {
		t.Fatalf("expected start and failed events, got %d events", len(events))
	}

	_, stepCalls, err = runRetryWorkflow(store, true)
	if err == nil {
		t.Fatal("expected retry run to fail")
	}
	if stepCalls != 1 {
		t.Fatalf("expected retry run to call step once, got %d", stepCalls)
	}
	events = readEvents(t, store, "retry-001")
	if len(events) != 2 {
		t.Fatalf("expected retry to update the failed event, got %d events", len(events))
	}
	failedEvent, ok := events[1].(*StepFailedEvent)
	if !ok {
		t.Fatalf("expected latest event to be StepFailedEvent, got %T", events[1])
	}
	if len(failedEvent.Attempts) != 2 {
		t.Fatalf("expected failed event to include 2 attempts, got %d", len(failedEvent.Attempts))
	}

	output, stepCalls, err := runRetryWorkflow(store, false)
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if output != "fragile_step" {
		t.Fatalf("unexpected output: %s", output)
	}
	if stepCalls != 1 {
		t.Fatalf("expected successful retry to call step once, got %d", stepCalls)
	}
	events = readEvents(t, store, "retry-001")
	if len(events) != 3 {
		t.Fatalf("expected successful retry to append one finish event, got %d events", len(events))
	}
	if _, ok := events[2].(*StepFinishEvent); !ok {
		t.Fatalf("expected latest event to be StepFinishEvent, got %T", events[2])
	}

}

func TestStepRejectsDuplicateName(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, string]()

	err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID: "duplicate-step-name",
		Store: store,
		Workflow: Workflow[struct{}, string]{
			Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
				if _, err := ctx.Step("duplicate", StepOption[string]{
					Handler: func(ctx *StepContext) (string, error) {
						return "first", nil
					},
				}); err != nil {
					return "", err
				}
				_, err := ctx.Step("duplicate", StepOption[string]{
					Handler: func(ctx *StepContext) (string, error) {
						return "second", nil
					},
				})
				return "", err
			},
		},
	})
	if !errors.Is(err, ErrDuplicateStepName) {
		t.Fatalf("expected duplicate step name error, got %v", err)
	}
}

func TestStepReusesExistingStartEvent(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, string]()
	appendReplayEvent(store, "orphan-start-001", &StepStartEvent{
		EventBase: EventBase{
			Name: "resume_step",
		},
	})

	stepCalls := 0
	var output string
	err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID: "orphan-start-001",
		Store: store,
		Workflow: Workflow[struct{}, string]{
			Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
				result, err := ctx.Step("resume_step", StepOption[string]{
					Handler: func(stepCtx *StepContext) (string, error) {
						stepCalls++
						return "resumed", nil
					},
				})
				if err != nil {
					return "", err
				}

				output = result
				return output, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("expected workflow to succeed, got %v", err)
	}
	if output != "resumed" {
		t.Fatalf("unexpected output: %s", output)
	}
	if stepCalls != 1 {
		t.Fatalf("expected resumed step to call handler once, got %d", stepCalls)
	}
	events := readEvents(t, store, "orphan-start-001")
	if len(events) != 2 {
		t.Fatalf("expected existing start and appended finish events, got %d events", len(events))
	}
	if _, ok := events[0].(*StepStartEvent); !ok {
		t.Fatalf("expected first event to remain StepStartEvent, got %T", events[0])
	}
	if _, ok := events[1].(*StepFinishEvent); !ok {
		t.Fatalf("expected second event to be StepFinishEvent, got %T", events[1])
	}
}

func TestRuntimeRenewsLeaseDuringLongStep(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, string]()
	err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID:         "long-step-lease",
		Store:         store,
		LeaseDuration: 15 * time.Millisecond,
		Workflow: Workflow[struct{}, string]{
			Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
				_, err := ctx.Step("long_step", StepOption[struct{}]{
					Handler: func(ctx *StepContext) (struct{}, error) {
						time.Sleep(50 * time.Millisecond)
						return struct{}{}, nil
					},
				})
				if err != nil {
					return "", err
				}
				return "finished", nil
			},
		},
	})
	if err != nil {
		t.Fatalf("expected heartbeat to keep lease valid, got %v", err)
	}
	state, err := store.GetRunState("long-step-lease")
	if err != nil {
		t.Fatalf("failed to read final state: %v", err)
	}
	if state.Status != RunStatusFinished || state.Output != "finished" {
		t.Fatalf("unexpected final state: %+v", state)
	}
}

func TestWorkflowStoreContinuationLifecycleIsAtomic(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	state, rootLease, err := store.CreateRun(
		"run",
		struct{}{},
		"owner",
		defaultLeaseDuration,
	)
	if err != nil || state.Status != RunStatusRunning {
		t.Fatalf("failed to create run: state=%+v err=%v", state, err)
	}
	started := newParallelStartedEvent("parallel", "first", "second")
	completed := newParallelCompletedEvent("parallel")
	forked, err := store.ForkParallel(
		rootLease,
		"group",
		started,
		started.Branches,
		defaultLeaseDuration,
	)
	if err != nil || len(forked.BranchLeases) != 2 {
		t.Fatalf("expected two branch leases, result=%+v err=%v", forked, err)
	}

	firstResult, err := store.CompleteContinuation(
		forked.BranchLeases[0],
		"group",
		newBranchCompletedEvent("parallel", "first"),
		completed,
		defaultLeaseDuration,
	)
	if err != nil || firstResult.ParentLease != nil ||
		!slices.Equal(firstResult.RemainingBranchIDs, []string{"second"}) {
		t.Fatalf("expected second branch to remain, result=%+v err=%v", firstResult, err)
	}
	if _, err := store.CompleteContinuation(
		forked.BranchLeases[0],
		"group",
		newBranchCompletedEvent("parallel", "first"),
		completed,
		defaultLeaseDuration,
	); !errors.Is(err, ErrContinuationLeaseInvalid) {
		t.Fatalf("expected stale completion to fail, got %v", err)
	}

	lastResult, err := store.CompleteContinuation(
		forked.BranchLeases[1],
		"group",
		newBranchCompletedEvent("parallel", "second"),
		completed,
		defaultLeaseDuration,
	)
	if err != nil || lastResult.ParentLease == nil ||
		len(lastResult.RemainingBranchIDs) != 0 {
		t.Fatalf("expected parent continuation to resume, result=%+v err=%v", lastResult, err)
	}
	if err := store.CompleteRun(*lastResult.ParentLease, struct{}{}); err != nil {
		t.Fatalf("failed to complete run: %v", err)
	}
}

func TestWorkflowReusesBranchPathAcrossSequentialParallels(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, string]()
	branchCalls := 0
	workflow := Workflow[struct{}, string]{
		Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
			for _, name := range []string{"first-parallel", "second-parallel"} {
				if err := ctx.Parallel(
					ParallelOption{Name: name},
					Branch[struct{}, string]{
						BranchID: "shared-branch",
						Handler: func(ctx *WorkflowContext[struct{}, string]) error {
							branchCalls++
							return nil
						},
					},
				); err != nil {
					return "", err
				}
			}
			return "finished", nil
		},
	}

	if err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID:    "sequential-parallels",
		Store:    store,
		Workflow: workflow,
	}); err != nil {
		t.Fatalf("failed to run sequential parallels: %v", err)
	}
	if branchCalls != 2 {
		t.Fatalf("expected shared branch path to execute twice, got %d", branchCalls)
	}
	state, err := store.GetRunState("sequential-parallels")
	if err != nil {
		t.Fatalf("failed to read final state: %v", err)
	}
	if state.Status != RunStatusFinished || state.Output != "finished" {
		t.Fatalf("unexpected final state: %+v", state)
	}
}

func TestParallelJoinsBranchErrors(t *testing.T) {
	firstErr := errors.New("first branch failed")
	secondErr := errors.New("second branch failed")
	ctx, store := newParallelTestContext()

	option := ParallelOption{Name: "join-errors"}
	err := ctx.Parallel(
		option,
		Branch[struct{}, struct{}]{
			BranchID: "first",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				return firstErr
			},
		},
		Branch[struct{}, struct{}]{
			BranchID: "second",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				return secondErr
			},
		},
	)

	if !errors.Is(err, firstErr) {
		t.Fatalf("expected joined error to include first branch error, got %v", err)
	}
	if !errors.Is(err, secondErr) {
		t.Fatalf("expected joined error to include second branch error, got %v", err)
	}
	if err.Error() != firstErr.Error()+"\n"+secondErr.Error() {
		t.Fatalf("expected errors in branch declaration order, got %v", err)
	}
	if remaining := branchCount(store, "parallel-test", ctx.branchGroupID(option.Name)); remaining != 2 {
		t.Fatalf("expected failed branches to remain, got %d", remaining)
	}
}

func TestParallelRepanicsNonAwaitedPanic(t *testing.T) {
	panicErr := errors.New("branch panicked")
	ctx, _ := newParallelTestContext()

	defer func() {
		if recovered := recover(); recovered != panicErr {
			t.Fatalf("expected original panic, got %v", recovered)
		}
	}()

	err := ctx.Parallel(ParallelOption{Name: "panic"}, Branch[struct{}, struct{}]{
		BranchID: "panic",
		Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
			panic(panicErr)
		},
	})
	t.Fatalf("expected panic, got error %v", err)
}

func TestParallelReturnsBranchErrorBeforeAwaitedPanic(t *testing.T) {
	branchErr := errors.New("branch failed")
	ctx, store := newParallelTestContext()

	option := ParallelOption{Name: "error-before-await"}
	err := ctx.Parallel(
		option,
		Branch[struct{}, struct{}]{
			BranchID: "failed",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				return branchErr
			},
		},
		Branch[struct{}, struct{}]{
			BranchID: "await",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				panic(&awaitSignal{})
			},
		},
	)
	if !errors.Is(err, branchErr) {
		t.Fatalf("expected branch error, got %v", err)
	}
	if remaining := branchCount(store, "parallel-test", ctx.branchGroupID(option.Name)); remaining != 2 {
		t.Fatalf("expected awaiting branch to remain, got %d branches", remaining)
	}
}

func TestParallelValidatesNameAndBranchIDsBeforeStarting(t *testing.T) {
	ctx := &WorkflowContext[struct{}, struct{}]{}
	handlerCalled := false
	handler := func(ctx *WorkflowContext[struct{}, struct{}]) error {
		handlerCalled = true
		return nil
	}

	err := ctx.Parallel(ParallelOption{}, Branch[struct{}, struct{}]{
		BranchID: "valid",
		Handler:  handler,
	})
	if !errors.Is(err, ErrParallelNameRequired) {
		t.Fatalf("expected required name error, got %v", err)
	}

	err = ctx.Parallel(ParallelOption{Name: "missing-branch-id"}, Branch[struct{}, struct{}]{
		Handler: handler,
	})
	if !errors.Is(err, ErrBranchIDRequired) {
		t.Fatalf("expected required branch ID error, got %v", err)
	}

	err = ctx.Parallel(
		ParallelOption{Name: "duplicate-branch-id"},
		Branch[struct{}, struct{}]{BranchID: "duplicate", Handler: handler},
		Branch[struct{}, struct{}]{BranchID: "duplicate", Handler: handler},
	)
	if !errors.Is(err, ErrDuplicateBranchID) {
		t.Fatalf("expected duplicate branch ID error, got %v", err)
	}
	if handlerCalled {
		t.Fatal("expected validation to finish before starting branches")
	}
}

func TestParallelRepanicsBranchSignalAfterBranchesComplete(t *testing.T) {
	ctx, store := newParallelTestContext()
	completed := make(chan struct{})
	option := ParallelOption{Name: "await-branch"}

	defer func() {
		recovered := recover()
		if _, ok := recovered.(*branchSignal); !ok {
			t.Fatalf("expected branchSignal panic, got %v", recovered)
		}

		select {
		case <-completed:
		default:
			t.Fatal("expected parallel branch to complete before awaited panic")
		}
		if remaining := branchCount(store, "parallel-test", ctx.branchGroupID(option.Name)); remaining != 1 {
			t.Fatalf("expected awaiting branch to remain, got %d branches", remaining)
		}
	}()

	err := ctx.Parallel(
		option,
		Branch[struct{}, struct{}]{
			BranchID: "await",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				panic(&awaitSignal{})
			},
		},
		Branch[struct{}, struct{}]{
			BranchID: "complete",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				close(completed)
				return nil
			},
		},
	)
	t.Fatalf("expected awaited panic, got error %v", err)
}

func TestParallelOnlyRunsRemainingBranches(t *testing.T) {
	ctx, store := newParallelTestContext()
	option := ParallelOption{Name: "resume-branches"}
	branchGroupID := ctx.branchGroupID(option.Name)
	started := newParallelStartedEvent(option.Name, "pending", "completed")
	completed := newParallelCompletedEvent(option.Name)
	forked, err := store.ForkParallel(
		*ctx.lease,
		branchGroupID,
		started,
		started.Branches,
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to start branches: %v", err)
	}
	ctx.lease = nil
	if _, err := store.CompleteContinuation(
		forked.BranchLeases[1],
		branchGroupID,
		newBranchCompletedEvent(option.Name, "completed"),
		completed,
		defaultLeaseDuration,
	); err != nil {
		t.Fatalf("failed to complete branch: %v", err)
	}
	events, err := store.GetEvents("parallel-test")
	if err != nil {
		t.Fatalf("failed to load events: %v", err)
	}
	ctx.runtime.events = events
	ctx.runtime.initialLease = forked.BranchLeases[0]
	ctx.runtime.targeted = true
	ctx.runtime.targetBranchIDs = []string{"pending"}
	completedBranchCalled := false

	defer func() {
		recovered := recover()
		if _, ok := recovered.(*branchSignal); !ok {
			t.Fatalf("expected branchSignal panic, got %v", recovered)
		}
		if completedBranchCalled {
			t.Fatal("expected completed branch to be skipped")
		}
		if remaining := branchCount(store, "parallel-test", branchGroupID); remaining != 1 {
			t.Fatalf("expected pending branch to remain, got %d branches", remaining)
		}
	}()

	err = ctx.Parallel(
		option,
		Branch[struct{}, struct{}]{
			BranchID: "pending",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				panic(&awaitSignal{})
			},
		},
		Branch[struct{}, struct{}]{
			BranchID: "completed",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				completedBranchCalled = true
				return nil
			},
		},
	)
	t.Fatalf("expected branch signal, got error %v", err)
}

func TestParallelIsolatesNestedBranchGroups(t *testing.T) {
	ctx, store := newParallelTestContext()
	outerOption := ParallelOption{Name: "outer-parallel"}
	innerOption := ParallelOption{Name: "inner-parallel"}

	defer func() {
		recovered := recover()
		if _, ok := recovered.(*branchSignal); !ok {
			t.Fatalf("expected branchSignal panic, got %v", recovered)
		}
		if remaining := branchCount(store, "parallel-test", ctx.branchGroupID(outerOption.Name)); remaining != 1 {
			t.Fatalf("expected one outer branch to remain, got %d", remaining)
		}
		innerGroupID := ctx.branch("outer", nil).branchGroupID(innerOption.Name)
		if remaining := branchCount(store, "parallel-test", innerGroupID); remaining != 1 {
			t.Fatalf("expected one nested branch to remain, got %d", remaining)
		}
	}()

	err := ctx.Parallel(
		outerOption,
		Branch[struct{}, struct{}]{
			BranchID: "outer",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				return ctx.Parallel(
					innerOption,
					Branch[struct{}, struct{}]{
						BranchID: "await",
						Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
							panic(&awaitSignal{})
						},
					},
					Branch[struct{}, struct{}]{
						BranchID: "complete",
						Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
							return nil
						},
					},
				)
			},
		},
		Branch[struct{}, struct{}]{
			BranchID: "complete",
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) error {
				return nil
			},
		},
	)
	t.Fatalf("expected branch signal, got error %v", err)
}

func TestNestedParallelWorkflowResumesAfterUserExplanation(t *testing.T) {
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

	var explanation string
	workflow := Workflow[struct{}, string]{
		Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
			if err := ctx.Parallel(
				ParallelOption{Name: "initial-parallel"},
				Branch[struct{}, string]{
					BranchID: "initial-upper",
					Handler: func(ctx *WorkflowContext[struct{}, string]) error {
						return runStep(ctx, "initial_upper")
					},
				},
				Branch[struct{}, string]{
					BranchID: "initial-lower",
					Handler: func(ctx *WorkflowContext[struct{}, string]) error {
						return runStep(ctx, "initial_lower")
					},
				},
			); err != nil {
				return "", err
			}

			if err := runStep(ctx, "after_initial_join"); err != nil {
				return "", err
			}

			if err := ctx.Parallel(
				ParallelOption{Name: "final-parallel"},
				Branch[struct{}, string]{
					BranchID: "user-explanation",
					Handler: func(ctx *WorkflowContext[struct{}, string]) error {
						if err := runStep(ctx, "before_user_explanation"); err != nil {
							return err
						}
						value, err := ctx.WaitForEvent[string]("user-explanation", SignalOption{})
						if err == nil {
							explanation = value
						}
						return err
					},
				},
				Branch[struct{}, string]{
					BranchID: "nested-automation",
					Handler: func(ctx *WorkflowContext[struct{}, string]) error {
						if err := runStep(ctx, "before_nested_parallel"); err != nil {
							return err
						}
						return ctx.Parallel(
							ParallelOption{Name: "nested-parallel"},
							Branch[struct{}, string]{
								BranchID: "nested-upper",
								Handler: func(ctx *WorkflowContext[struct{}, string]) error {
									return runStep(ctx, "nested_upper")
								},
							},
							Branch[struct{}, string]{
								BranchID: "nested-lower",
								Handler: func(ctx *WorkflowContext[struct{}, string]) error {
									return runStep(ctx, "nested_lower")
								},
							},
						)
					},
				},
			); err != nil {
				return "", err
			}

			return explanation, nil
		},
	}

	err := RunWorkflow(WorkflowOption[struct{}, string]{
		RunID:    "nested-parallel-user-explanation",
		Store:    store,
		Workflow: workflow,
	})
	if err != nil {
		t.Fatalf("expected workflow to pause without error, got %v", err)
	}
	state, err := store.GetRunState("nested-parallel-user-explanation")
	if err != nil {
		t.Fatalf("failed to read paused state: %v", err)
	}
	if state.Status != RunStatusPaused {
		t.Fatalf("expected paused state, got %s", state.Status)
	}

	expectedSteps := []string{
		"initial_upper",
		"initial_lower",
		"after_initial_join",
		"before_user_explanation",
		"before_nested_parallel",
		"nested_upper",
		"nested_lower",
	}
	for _, name := range expectedSteps {
		if stepCalls[name] != 1 {
			t.Fatalf("expected step %s to execute once before pause, got %d", name, stepCalls[name])
		}
	}

	err = ResumeRun(ResumeRunOption[struct{}, string]{
		RunID: "nested-parallel-user-explanation",
		Store: store,
		Callback: &SignalCallback{
			Name:   "user-explanation",
			Result: json.RawMessage(`"clarified"`),
		},
		Workflow: workflow,
	})
	if err != nil {
		t.Fatalf("expected workflow to finish after explanation, got %v", err)
	}
	state, err = store.GetRunState("nested-parallel-user-explanation")
	if err != nil {
		t.Fatalf("failed to read finished state: %v", err)
	}
	if state.Status != RunStatusFinished {
		t.Fatalf("expected finished state, got %s", state.Status)
	}
	if state.Output != "clarified" {
		t.Fatalf("expected explanation output, got %q", state.Output)
	}
	for _, name := range expectedSteps {
		if stepCalls[name] != 1 {
			t.Fatalf("expected replay to skip step %s, got %d executions", name, stepCalls[name])
		}
	}

	events, err := store.GetEvents("nested-parallel-user-explanation")
	if err != nil {
		t.Fatalf("failed to read events: %v", err)
	}
	eventCounts := make(map[string]int)
	var initialCompleted *ParallelCompletedEvent
	var afterInitialStarted *StepStartEvent
	var finalStarted *ParallelStartedEvent
	var finalCompleted *ParallelCompletedEvent
	var awaited *SignalAwaitedEvent
	var resolved *SignalResolvedEvent
	var userBranchCompleted *BranchCompletedEvent
	var nestedBranchCompleted *BranchCompletedEvent
	for _, event := range events {
		eventCounts[event.GetType()]++
		switch event := event.(type) {
		case *ParallelStartedEvent:
			if event.Name == "final-parallel" && len(event.BranchIds) == 0 {
				finalStarted = event
			}
		case *ParallelCompletedEvent:
			if event.Name == "initial-parallel" {
				initialCompleted = event
			}
			if event.Name == "final-parallel" && len(event.BranchIds) == 0 {
				finalCompleted = event
			}
		case *StepStartEvent:
			if event.Name == "after_initial_join" {
				afterInitialStarted = event
			}
		case *SignalAwaitedEvent:
			awaited = event
		case *SignalResolvedEvent:
			resolved = event
		case *BranchCompletedEvent:
			if event.Name == "final-parallel" && slices.Equal(event.BranchIds, []string{"user-explanation"}) {
				userBranchCompleted = event
			}
			if event.Name == "final-parallel" && slices.Equal(event.BranchIds, []string{"nested-automation"}) {
				nestedBranchCompleted = event
			}
		}
	}
	if eventCounts[EventTypeStepStart] != len(expectedSteps) ||
		eventCounts[EventTypeStepFinish] != len(expectedSteps) ||
		eventCounts[EventTypeSignalAwaited] != 1 ||
		eventCounts[EventTypeSignalResolved] != 1 ||
		eventCounts[EventTypeParallelStarted] != 3 ||
		eventCounts[EventTypeBranchCompleted] != 6 ||
		eventCounts[EventTypeParallelCompleted] != 3 {
		t.Fatalf("unexpected event counts: %v", eventCounts)
	}
	if initialCompleted == nil || afterInitialStarted == nil || finalStarted == nil ||
		finalCompleted == nil || awaited == nil || resolved == nil ||
		userBranchCompleted == nil || nestedBranchCompleted == nil {
		t.Fatal("expected all workflow lifecycle events to be present")
	}
	assertEventPrevious(t, afterInitialStarted, IdentityOf(initialCompleted))
	assertEventPrevious(t, resolved, IdentityOf(awaited))
	assertEventPrevious(t, userBranchCompleted, IdentityOf(resolved))
	assertEventPrevious(t, finalCompleted, IdentityOf(finalStarted))
	assertEventPrevious(t, nestedBranchCompleted, EventIdentity{
		Name:      "nested-parallel",
		BranchIds: []string{"nested-automation"},
		Type:      EventTypeParallelCompleted,
	})
}

func TestRunWorkflowPausesWhenAwaitingSignal(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()

	err := RunWorkflow(WorkflowOption[struct{}, struct{}]{
		RunID: "await-signal-001",
		Store: store,
		Workflow: Workflow[struct{}, struct{}]{
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) (struct{}, error) {
				_, err := ctx.WaitForEvent[string]("approval", SignalOption{})
				return struct{}{}, err
			},
		},
	})
	if err != nil {
		t.Fatalf("expected awaiting workflow to pause without returning an error, got %v", err)
	}
	state := readRunState(t, store, "await-signal-001")
	if state.Status != RunStatusPaused {
		t.Fatalf("expected paused state, got %s", state.Status)
	}
	events := readEvents(t, store, "await-signal-001")
	if len(events) != 1 {
		t.Fatalf("expected one awaited event, got %d", len(events))
	}
	if _, ok := events[0].(*SignalAwaitedEvent); !ok {
		t.Fatalf("expected SignalAwaitedEvent, got %T", events[0])
	}
}

func TestRunWorkflowRemainsPausedWhenHandlerRecoversAwaitSignal(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()

	err := RunWorkflow(WorkflowOption[struct{}, struct{}]{
		RunID: "recovered-await-signal-001",
		Store: store,
		Workflow: Workflow[struct{}, struct{}]{
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) (struct{}, error) {
				defer func() {
					_ = recover()
				}()
				_, err := ctx.WaitForEvent[string]("approval", SignalOption{})
				return struct{}{}, err
			},
		},
	})
	if err != nil {
		t.Fatalf("expected recovered awaiting workflow to return nil, got %v", err)
	}
	state := readRunState(t, store, "recovered-await-signal-001")
	if state.Status != RunStatusPaused {
		t.Fatalf("expected paused state, got %s", state.Status)
	}
}

func TestRunWorkflowReturnsSuspendContinuationError(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()

	err := RunWorkflow(WorkflowOption[struct{}, struct{}]{
		RunID: "failed-pause-001",
		Store: store,
		Workflow: Workflow[struct{}, struct{}]{
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) (struct{}, error) {
				invalidateLease(store, *ctx.lease)
				_, err := ctx.WaitForEvent[string]("approval", SignalOption{})
				return struct{}{}, err
			},
		},
	})
	if !errors.Is(err, ErrContinuationLeaseInvalid) {
		t.Fatalf("expected suspend continuation error, got %v", err)
	}
}

func TestRunWorkflowReturnsCompleteRunError(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()

	err := RunWorkflow(WorkflowOption[struct{}, struct{}]{
		RunID: "failed-finish-001",
		Store: store,
		Workflow: Workflow[struct{}, struct{}]{
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) (struct{}, error) {
				invalidateLease(store, *ctx.lease)
				return struct{}{}, nil
			},
		},
	})
	if !errors.Is(err, ErrContinuationLeaseInvalid) {
		t.Fatalf("expected complete run error, got %v", err)
	}
}

func TestRunWorkflowReturnsErrorWhenRunStateExists(t *testing.T) {
	store := NewInMemoryWorkflowStore[checkoutInput, checkoutOutput]()
	seedRunState(store, "stored-state-001", &RunState[checkoutInput, checkoutOutput]{
		Status: RunStatusPaused,
		Input: checkoutInput{
			CustomerID: "stored-customer",
			Items:      []string{"stored-item"},
		},
	})

	handlerCalled := false
	err := RunWorkflow(WorkflowOption[checkoutInput, checkoutOutput]{
		RunID: "stored-state-001",
		Store: store,
		Input: checkoutInput{
			CustomerID: "option-customer",
			Items:      []string{"option-item"},
		},
		Workflow: Workflow[checkoutInput, checkoutOutput]{
			Handler: func(ctx *WorkflowContext[checkoutInput, checkoutOutput]) (checkoutOutput, error) {
				handlerCalled = true
				return checkoutOutput{}, nil
			},
		},
	})
	if !errors.Is(err, ErrRunStateExists) {
		t.Fatalf("expected run state exists error, got %v", err)
	}
	if handlerCalled {
		t.Fatal("expected handler not to run")
	}
	state := readRunState(t, store, "stored-state-001")
	if state.Status != RunStatusPaused {
		t.Fatalf("expected existing state to remain paused, got %s", state.Status)
	}
	if state.Input.CustomerID != "stored-customer" {
		t.Fatalf("expected existing run state to remain unchanged, got %s", state.Input.CustomerID)
	}
}

func TestResumeRunReturnsErrorWhenRunStateMissing(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()

	handlerCalled := false
	err := ResumeRun(ResumeRunOption[struct{}, struct{}]{
		RunID: "missing-state-001",
		Store: store,
		Workflow: Workflow[struct{}, struct{}]{
			Handler: func(ctx *WorkflowContext[struct{}, struct{}]) (struct{}, error) {
				handlerCalled = true
				return struct{}{}, nil
			},
		},
	})
	if !errors.Is(err, ErrRunStateNotFound) {
		t.Fatalf("expected run state not found error, got %v", err)
	}
	if handlerCalled {
		t.Fatal("expected handler not to run")
	}
}

func TestResumeRunUsesStoredInputAndResolvedSignal(t *testing.T) {
	store := NewInMemoryWorkflowStore[checkoutInput, string]()
	input := checkoutInput{
		CustomerID: "stored-customer",
		Items:      []string{"book", "pen"},
	}

	handlerCalls := 0
	stepCalls := 0
	var resumedInput checkoutInput
	var approval string
	var result string
	workflow := Workflow[checkoutInput, string]{
		Handler: func(ctx *WorkflowContext[checkoutInput, string]) (string, error) {
			handlerCalls++
			if handlerCalls == 2 {
				resumedInput = ctx.input
			}

			var err error
			result, err = ctx.Step("prepare_order", StepOption[string]{
				Handler: func(stepCtx *StepContext) (string, error) {
					stepCalls++
					return "prepared", nil
				},
			})
			if err != nil {
				return "", err
			}

			approval, err = ctx.WaitForEvent[string]("approval", SignalOption{})
			return approval, err
		},
	}

	err := RunWorkflow(WorkflowOption[checkoutInput, string]{
		RunID:    "resume-run-001",
		Store:    store,
		Input:    input,
		Workflow: workflow,
	})
	if err != nil {
		t.Fatalf("expected first run to pause without error, got %v", err)
	}
	if handlerCalls != 1 {
		t.Fatalf("expected first run to call workflow handler once, got %d", handlerCalls)
	}
	if stepCalls != 1 {
		t.Fatalf("expected first run to execute step once, got %d", stepCalls)
	}
	state := readRunState(t, store, "resume-run-001")
	if state.Status != RunStatusPaused {
		t.Fatalf("expected paused state, got %s", state.Status)
	}

	err = ResumeRun(ResumeRunOption[checkoutInput, string]{
		RunID: "resume-run-001",
		Store: store,
		Callback: &SignalCallback{
			Name:   "approval",
			Result: json.RawMessage(`"approved"`),
		},
		Workflow: workflow,
	})
	if err != nil {
		t.Fatalf("expected resumed run to succeed, got %v", err)
	}
	if handlerCalls != 2 {
		t.Fatalf("expected resume to call the same workflow handler again, got %d calls", handlerCalls)
	}
	if resumedInput.CustomerID != input.CustomerID {
		t.Fatalf("expected stored input to be used, got %s", resumedInput.CustomerID)
	}
	if result != "prepared" {
		t.Fatalf("expected step result to replay, got %s", result)
	}
	if stepCalls != 1 {
		t.Fatalf("expected resume to skip finished step handler, got %d calls", stepCalls)
	}
	if approval != "approved" {
		t.Fatalf("unexpected approval payload: %s", approval)
	}
	state = readRunState(t, store, "resume-run-001")
	if state.Status != RunStatusFinished {
		t.Fatalf("expected finished state, got %s", state.Status)
	}
}

func TestRunCheckoutWorkflow(t *testing.T) {
	ExampleRunWorkflow()
}

func TestStepEventsUseNameAndName(t *testing.T) {
	store := NewInMemoryWorkflowStore[checkoutInput, checkoutOutput]()

	output, _, err := runCheckoutWorkflow(store)
	if err != nil {
		t.Fatalf("expected workflow to succeed, got %v", err)
	}
	state := readRunState(t, store, "checkout-001")
	if state.Output != output {
		t.Fatalf("expected stored output to match handler output, got %+v", state.Output)
	}

	events := readEvents(t, store, "checkout-001")
	startEvent, ok := events[0].(*StepStartEvent)
	if !ok {
		t.Fatalf("expected first event to be StepStartEvent, got %T", events[0])
	}
	if startEvent.Name != "reserve_inventory" {
		t.Fatalf("unexpected step name: %s", startEvent.Name)
	}

	eventBytes, err := json.Marshal(startEvent)
	if err != nil {
		t.Fatalf("expected event to marshal, got %v", err)
	}
	eventJSON := string(eventBytes)
	var eventFields map[string]any
	if err := json.Unmarshal(eventBytes, &eventFields); err != nil {
		t.Fatalf("expected event JSON object, got %v", err)
	}
	if _, ok := eventFields["name"]; !ok {
		t.Fatalf("expected name in event JSON: %s", eventJSON)
	}
	if _, ok := eventFields["stepId"]; ok {
		t.Fatalf("expected stepId to be omitted from event JSON: %s", eventJSON)
	}
	if _, ok := eventFields["runId"]; ok {
		t.Fatalf("expected runId to be omitted from event JSON: %s", eventJSON)
	}
}

func ExampleRunWorkflow() {
	store := NewInMemoryWorkflowStore[checkoutInput, checkoutOutput]()

	output, stepCalls, err := runCheckoutWorkflow(store)
	fmt.Println(err == nil)
	fmt.Println(output.ReservationID)
	fmt.Println(output.ConfirmationID)
	fmt.Println(stepCalls)

	_, stepCalls, err = runCheckoutWorkflow(store)
	fmt.Println(errors.Is(err, ErrRunStateExists))
	fmt.Println(stepCalls)

	// Output:
	// true
	// reserve_inventory
	// confirm_order
	// 2
	// true
	// 0
}
