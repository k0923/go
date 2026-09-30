package workflow

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type concurrentResumeStore struct {
	*InMemoryWorkflowStore[struct{}, string]
	synchronizeSnapshots atomic.Bool
	snapshotsRemaining   atomic.Int32
	snapshotsReady       sync.WaitGroup
}

func newConcurrentResumeStore(resumeCount int) *concurrentResumeStore {
	store := &concurrentResumeStore{
		InMemoryWorkflowStore: NewInMemoryWorkflowStore[struct{}, string](),
	}
	store.snapshotsRemaining.Store(int32(resumeCount))
	store.snapshotsReady.Add(resumeCount)
	return store
}

func (store *concurrentResumeStore) GetEvents(runID string) ([]Event, error) {
	events, err := store.InMemoryWorkflowStore.GetEvents(runID)
	if err == nil && store.synchronizeSnapshots.Load() {
		for {
			remaining := store.snapshotsRemaining.Load()
			if remaining <= 0 {
				break
			}
			if store.snapshotsRemaining.CompareAndSwap(remaining, remaining-1) {
				store.snapshotsReady.Done()
				store.snapshotsReady.Wait()
				break
			}
		}
	}
	return events, err
}

func TestResumeRunConcurrentBranchesDoesNotRegressFinishedState(t *testing.T) {
	const (
		runID       = "concurrent-resume"
		branchCount = 3
	)

	store := newConcurrentResumeStore(branchCount)
	resumedBranches := make([]atomic.Int32, branchCount)
	resumeBarrier := &sync.WaitGroup{}
	resumeBarrier.Add(branchCount)

	branches := make([]Branch[struct{}, string], branchCount)
	for index := range branches {
		branchID := fmt.Sprintf("branch-%d", index)
		branches[index] = Branch[struct{}, string]{
			BranchID: branchID,
			Handler: func(ctx *WorkflowContext[struct{}, string]) error {
				value, err := ctx.WaitForEvent[string](branchID, SignalOption{})
				if err != nil {
					return err
				}
				if value != branchID {
					return fmt.Errorf("unexpected signal payload %q for %s", value, branchID)
				}

				resumeBarrier.Done()
				resumeBarrier.Wait()
				_, err = ctx.Step("after_"+branchID, StepOption[struct{}]{
					Handler: func(ctx *StepContext) (struct{}, error) {
						resumedBranches[index].Add(1)
						return struct{}{}, nil
					},
				})
				return err
			},
		}
	}

	workflow := Workflow[struct{}, string]{
		Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
			if err := ctx.Parallel(
				ParallelOption{Name: "parallel"},
				branches...,
			); err != nil {
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

	store.synchronizeSnapshots.Store(true)
	results := make(chan error, branchCount)
	for index := range branches {
		go func() {
			results <- ResumeRun(ResumeRunOption[struct{}, string]{
				RunID: runID,
				Store: store,
				Callback: &SignalCallback{
					Name:   fmt.Sprintf("branch-%d", index),
					Result: json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("branch-%d", index))),
				},
				Workflow: workflow,
			})
		}()
	}

	allBranchesResumed := make(chan struct{})
	go func() {
		resumeBarrier.Wait()
		close(allBranchesResumed)
	}()
	select {
	case <-allBranchesResumed:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent resumes did not execute every resolved branch")
	}

	for range branchCount {
		if err := <-results; err != nil {
			t.Fatalf("concurrent ResumeRun failed: %v", err)
		}
	}
	for index := range resumedBranches {
		if calls := resumedBranches[index].Load(); calls != 1 {
			t.Errorf("expected branch-%d continuation to execute once, got %d", index, calls)
		}
	}

	state, err := store.GetRunState(runID)
	if err != nil {
		t.Fatalf("failed to read final state: %v", err)
	}
	if state.Status != RunStatusFinished {
		t.Fatalf("expected concurrent resumes to preserve finished state, got %s", state.Status)
	}
	if state.Output != "finished" {
		t.Fatalf("expected finished output, got %q", state.Output)
	}
}

func TestResumeRunConcurrentNestedBranchesShareAncestorClaim(t *testing.T) {
	const runID = "concurrent-nested-resume"

	store := newConcurrentResumeStore(2)
	resumed := [2]atomic.Int32{}
	barrier := &sync.WaitGroup{}
	barrier.Add(2)

	nestedBranch := func(index int) Branch[struct{}, string] {
		branchID := fmt.Sprintf("nested-%d", index)
		return Branch[struct{}, string]{
			BranchID: branchID,
			Handler: func(ctx *WorkflowContext[struct{}, string]) error {
				if _, err := ctx.WaitForEvent[string](branchID, SignalOption{}); err != nil {
					return err
				}
				resumed[index].Add(1)
				barrier.Done()
				barrier.Wait()
				return nil
			},
		}
	}

	workflow := Workflow[struct{}, string]{
		Handler: func(ctx *WorkflowContext[struct{}, string]) (string, error) {
			err := ctx.Parallel(
				ParallelOption{Name: "outer"},
				Branch[struct{}, string]{
					BranchID: "nested",
					Handler: func(ctx *WorkflowContext[struct{}, string]) error {
						return ctx.Parallel(
							ParallelOption{Name: "inner"},
							nestedBranch(0),
							nestedBranch(1),
						)
					},
				},
			)
			if err != nil {
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

	store.synchronizeSnapshots.Store(true)
	results := make(chan error, 2)
	for index := range 2 {
		go func() {
			branchID := fmt.Sprintf("nested-%d", index)
			results <- ResumeRun(ResumeRunOption[struct{}, string]{
				RunID: runID,
				Store: store,
				Callback: &SignalCallback{
					Name:      branchID,
					BranchIDs: []string{"nested", branchID},
					Result:    json.RawMessage(`"resolved"`),
				},
				Workflow: workflow,
			})
		}()
	}

	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent nested ResumeRun failed: %v", err)
		}
	}
	for index := range resumed {
		if calls := resumed[index].Load(); calls != 1 {
			t.Errorf("expected nested-%d to resume once, got %d", index, calls)
		}
	}
	state, err := store.GetRunState(runID)
	if err != nil {
		t.Fatalf("failed to read final state: %v", err)
	}
	if state.Status != RunStatusFinished || state.Output != "finished" {
		t.Fatalf("unexpected final state: %+v", state)
	}
}
