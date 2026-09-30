package workflow

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func createTestRun[Input any, Output any](
	t *testing.T,
	store *InMemoryWorkflowStore[Input, Output],
	runID string,
	input Input,
) ExecutionLease {
	t.Helper()
	_, lease, err := store.CreateRun(
		runID,
		input,
		"owner",
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}
	return lease
}

func TestInMemoryWorkflowStoreCreatesRunAtomically(t *testing.T) {
	store := NewInMemoryWorkflowStore[string, string]()
	state, lease, err := store.CreateRun(
		"run",
		"input",
		"owner",
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}
	if state.Status != RunStatusRunning || state.Input != "input" {
		t.Fatalf("unexpected created state: %+v", state)
	}
	if lease.Key.RunID != "run" || len(lease.Key.BranchIDs) != 0 ||
		lease.OwnerID != "owner" || lease.Generation != 1 {
		t.Fatalf("unexpected root lease: %+v", lease)
	}

	if _, _, err := store.CreateRun(
		"run",
		"replacement",
		"other-owner",
		defaultLeaseDuration,
	); !errors.Is(err, ErrRunStateExists) {
		t.Fatalf("expected duplicate run to fail, got %v", err)
	}
	stored, err := store.GetRunState("run")
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
	if stored.Input != "input" {
		t.Fatalf("expected original input to remain, got %q", stored.Input)
	}
}

func TestInMemoryWorkflowStoreAppendsEventsWithLeaseFencing(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	lease := createTestRun(t, store, "run", struct{}{})
	started := &StepStartEvent{
		EventBase: EventBase{Name: "step"},
	}
	if err := store.AppendEvent(lease, started); err != nil {
		t.Fatalf("failed to append event: %v", err)
	}

	if err := store.ReleaseContinuation(lease); err != nil {
		t.Fatalf("failed to release root: %v", err)
	}
	reclaimed, err := store.ClaimContinuation(
		lease.Key,
		"new-owner",
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to reclaim root: %v", err)
	}
	if reclaimed.Generation <= lease.Generation {
		t.Fatalf("expected generation to increase: old=%d new=%d", lease.Generation, reclaimed.Generation)
	}
	if err := store.AppendEvent(lease, &StepFinishEvent{
		EventBase: EventBase{Name: "step"},
	}); !errors.Is(err, ErrContinuationLeaseInvalid) {
		t.Fatalf("expected stale lease write to fail, got %v", err)
	}
}

func TestInMemoryWorkflowStoreUpsertsEventsByCompositeIdentity(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	rootLease := createTestRun(t, store, "run", struct{}{})
	forked, err := store.ForkParallel(
		rootLease,
		"group",
		newParallelStartedEvent("parallel", "first", "second"),
		[]string{"first", "second"},
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to fork branches: %v", err)
	}
	first := &StepFailedEvent{
		EventBase: EventBase{
			Name:      "shared-step",
			BranchIds: []string{"first"},
		},
		Attempts: []StepAttempt{{}},
	}
	updated := &StepFailedEvent{
		EventBase: EventBase{
			Name:      "shared-step",
			BranchIds: []string{"first"},
		},
		Attempts: []StepAttempt{{}, {}},
	}
	otherBranch := &StepFailedEvent{
		EventBase: EventBase{
			Name:      "shared-step",
			BranchIds: []string{"second"},
		},
		Attempts: []StepAttempt{{}},
	}
	for _, item := range []struct {
		lease ExecutionLease
		event Event
	}{
		{forked.BranchLeases[0], first},
		{forked.BranchLeases[0], updated},
		{forked.BranchLeases[1], otherBranch},
	} {
		if err := store.AppendEvent(item.lease, item.event); err != nil {
			t.Fatalf("failed to append event: %v", err)
		}
	}

	events, err := store.GetEvents("run")
	if err != nil {
		t.Fatalf("failed to read events: %v", err)
	}
	var failedEvents []*StepFailedEvent
	for _, event := range events {
		if failed, ok := event.(*StepFailedEvent); ok {
			failedEvents = append(failedEvents, failed)
		}
	}
	if len(failedEvents) != 2 || len(failedEvents[0].Attempts) != 2 {
		t.Fatalf("unexpected failed events: %#v", failedEvents)
	}
}

func TestInMemoryWorkflowStoreResolvesSignalAndClaimsContinuation(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	lease := createTestRun(t, store, "run", struct{}{})
	awaited := &SignalAwaitedEvent{
		EventBase: EventBase{Name: "approval"},
	}
	awaitedIdentity := IdentityOf(awaited)
	if err := store.SuspendContinuation(lease, awaited, Suspension{
		Kind:  SuspensionKindSignal,
		Event: awaitedIdentity,
	}); err != nil {
		t.Fatalf("failed to suspend continuation: %v", err)
	}
	resolved := &SignalResolvedEvent{
		EventBase: EventBase{
			Name:     awaited.Name,
			Previous: &awaitedIdentity,
		},
	}
	resumed, err := store.ResolveSignal(
		"run",
		awaitedIdentity,
		resolved,
		"resume-owner",
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to resolve signal: %v", err)
	}
	if resumed.OwnerID != "resume-owner" || resumed.Generation <= lease.Generation {
		t.Fatalf("unexpected resumed lease: %+v", resumed)
	}
	if _, err := store.ResolveSignal(
		"run",
		awaitedIdentity,
		resolved,
		"duplicate-owner",
		defaultLeaseDuration,
	); !errors.Is(err, ErrAwaitedSignalNotFound) {
		t.Fatalf("expected duplicate resolution to fail, got %v", err)
	}
}

func TestInMemoryWorkflowStoreDoesNotClaimSignalSuspensionWithoutSignal(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	lease := createTestRun(t, store, "run", struct{}{})
	awaited := &SignalAwaitedEvent{
		EventBase: EventBase{Name: "approval"},
	}
	if err := store.SuspendContinuation(lease, awaited, Suspension{
		Kind:  SuspensionKindSignal,
		Event: IdentityOf(awaited),
	}); err != nil {
		t.Fatalf("failed to suspend continuation: %v", err)
	}

	if _, err := store.ClaimContinuation(
		lease.Key,
		"manual-owner",
		defaultLeaseDuration,
	); !errors.Is(err, ErrContinuationNotClaimable) {
		t.Fatalf("expected signal suspension to require ResolveSignal, got %v", err)
	}
}

func TestInMemoryWorkflowStoreClaimsSleepingContinuation(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	lease := createTestRun(t, store, "run", struct{}{})
	sleep := &SleepScheduledEvent{
		EventBase: EventBase{Name: "sleep"},
		Until:     time.Now().Add(time.Hour),
		Suspended: true,
	}
	if err := store.SuspendContinuation(lease, sleep, Suspension{
		Kind:   SuspensionKindSleep,
		Event:  IdentityOf(sleep),
		WakeAt: sleep.Until,
	}); err != nil {
		t.Fatalf("failed to suspend continuation: %v", err)
	}
	resumed, err := store.ClaimNextContinuation(
		"run",
		"resume-owner",
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to claim sleeping continuation: %v", err)
	}
	if resumed.OwnerID != "resume-owner" || !continuationKeysEqual(resumed.Key, lease.Key) {
		t.Fatalf("unexpected resumed lease: %+v", resumed)
	}
}

func TestInMemoryWorkflowStoreForksAndJoinsParallel(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	rootLease := createTestRun(t, store, "run", struct{}{})
	started := newParallelStartedEvent("parallel", "first", "second", "third")
	completed := newParallelCompletedEvent("parallel")
	forked, err := store.ForkParallel(
		rootLease,
		"group",
		started,
		started.Branches,
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to fork branches: %v", err)
	}
	if len(forked.BranchLeases) != 3 {
		t.Fatalf("expected three branch leases, got %+v", forked.BranchLeases)
	}

	for index, branchLease := range forked.BranchLeases {
		branchID := started.Branches[index]
		result, err := store.CompleteContinuation(
			branchLease,
			"group",
			newBranchCompletedEvent("parallel", branchID),
			completed,
			defaultLeaseDuration,
		)
		if err != nil {
			t.Fatalf("failed to complete %s: %v", branchID, err)
		}
		if index < len(forked.BranchLeases)-1 && result.ParentLease != nil {
			t.Fatalf("expected parent to remain waiting after %s", branchID)
		}
		if index == len(forked.BranchLeases)-1 && result.ParentLease == nil {
			t.Fatal("expected final branch to reactivate parent")
		}
	}
}

func TestInMemoryWorkflowStoreReusesCompletedBranchPath(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	rootLease := createTestRun(t, store, "run", struct{}{})

	firstStarted := newParallelStartedEvent("first-parallel", "branch")
	firstFork, err := store.ForkParallel(
		rootLease,
		"first-group",
		firstStarted,
		firstStarted.Branches,
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed first fork: %v", err)
	}
	firstJoin, err := store.CompleteContinuation(
		firstFork.BranchLeases[0],
		"first-group",
		newBranchCompletedEvent("first-parallel", "branch"),
		newParallelCompletedEvent("first-parallel"),
		defaultLeaseDuration,
	)
	if err != nil || firstJoin.ParentLease == nil {
		t.Fatalf("failed first join: result=%+v err=%v", firstJoin, err)
	}

	secondStarted := newParallelStartedEvent("second-parallel", "branch")
	secondFork, err := store.ForkParallel(
		*firstJoin.ParentLease,
		"second-group",
		secondStarted,
		secondStarted.Branches,
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("expected completed branch path to be reusable, got %v", err)
	}
	if len(secondFork.BranchLeases) != 1 ||
		secondFork.BranchLeases[0].Generation <= firstFork.BranchLeases[0].Generation {
		t.Fatalf("expected a new fenced lease, got %+v", secondFork.BranchLeases)
	}
}

func TestInMemoryWorkflowStoreCompletesBranchesConcurrently(t *testing.T) {
	store := NewInMemoryWorkflowStore[struct{}, struct{}]()
	rootLease := createTestRun(t, store, "run", struct{}{})
	const branchCount = 100
	branchIDs := make([]string, branchCount)
	for index := range branchIDs {
		branchIDs[index] = fmt.Sprintf("branch-%d", index)
	}
	started := newParallelStartedEvent("parallel", branchIDs...)
	completed := newParallelCompletedEvent("parallel")
	forked, err := store.ForkParallel(
		rootLease,
		"group",
		started,
		branchIDs,
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to fork branches: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan CompleteContinuationResult, branchCount)
	errs := make(chan error, branchCount)
	for index, lease := range forked.BranchLeases {
		wg.Add(1)
		go func(index int, lease ExecutionLease) {
			defer wg.Done()
			result, err := store.CompleteContinuation(
				lease,
				"group",
				newBranchCompletedEvent("parallel", branchIDs[index]),
				completed,
				defaultLeaseDuration,
			)
			results <- result
			errs <- err
		}(index, lease)
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("failed to complete branch concurrently: %v", err)
		}
	}
	parentLeases := 0
	for result := range results {
		if result.ParentLease != nil {
			parentLeases++
		}
	}
	if parentLeases != 1 {
		t.Fatalf("expected exactly one parent lease, got %d", parentLeases)
	}
}

func TestInMemoryWorkflowStoreZeroValue(t *testing.T) {
	var store InMemoryWorkflowStore[struct{}, struct{}]
	_, lease, err := store.CreateRun(
		"run",
		struct{}{},
		"owner",
		defaultLeaseDuration,
	)
	if err != nil {
		t.Fatalf("failed to create run on zero-value store: %v", err)
	}
	if lease.Key.RunID != "run" {
		t.Fatalf("unexpected lease: %+v", lease)
	}
}

func TestInMemoryWorkflowStoreRunsWorkflow(t *testing.T) {
	store := NewInMemoryWorkflowStore[string, string]()
	err := RunWorkflow(WorkflowOption[string, string]{
		RunID: "memory-store-run",
		Input: "input",
		Store: store,
		Workflow: Workflow[string, string]{
			Handler: func(ctx *WorkflowContext[string, string]) (string, error) {
				return ctx.Step("echo", StepOption[string]{
					Handler: func(ctx *StepContext) (string, error) {
						return "output", nil
					},
				})
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to run workflow: %v", err)
	}
	state, err := store.GetRunState("memory-store-run")
	if err != nil {
		t.Fatalf("failed to read run state: %v", err)
	}
	if state.Status != RunStatusFinished || state.Output != "output" {
		t.Fatalf("unexpected run state: %+v", state)
	}
}

func assertBranchIDs(t *testing.T, actual []string, expected ...string) {
	t.Helper()
	if !slices.Equal(actual, expected) {
		t.Fatalf("expected branches %v, got %v", expected, actual)
	}
}
