package tflow_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/k0923/go/tflow"
)

type executor struct {
	start  func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error)
	resume func(context.Context, tflow.SubTask, tflow.Execution, tflow.Event) (tflow.DispatchResult, error)
	cancel func(context.Context, tflow.SubTask, tflow.Execution) error
}

func (x executor) Start(ctx context.Context, task tflow.SubTask, execution tflow.Execution) (tflow.DispatchResult, error) {
	return x.start(ctx, task, execution)
}
func (x executor) Resume(ctx context.Context, task tflow.SubTask, execution tflow.Execution, event tflow.Event) (tflow.DispatchResult, error) {
	if x.resume == nil {
		return tflow.DispatchResult{}, tflow.ErrInvalid
	}
	return x.resume(ctx, task, execution, event)
}
func (x executor) Cancel(ctx context.Context, task tflow.SubTask, execution tflow.Execution) error {
	if x.cancel != nil {
		return x.cancel(ctx, task, execution)
	}
	return nil
}

func TestAsyncExecutorWaitingAndResume(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	var resumes atomic.Int64
	d, err := tflow.NewExecutorDispatcher(e.Engine, executor{
		start: func(_ context.Context, task tflow.SubTask, execution tflow.Execution) (tflow.DispatchResult, error) {
			if task.(*rowTask).row != 0 || execution.Attempt != 1 {
				t.Fatal("lost business object or execution identity")
			}
			return tflow.DispatchResult{Status: tflow.Waiting, ExternalRef: "opaque-run-id"}, nil
		},
		resume: func(_ context.Context, task tflow.SubTask, execution tflow.Execution, input tflow.Event) (tflow.DispatchResult, error) {
			resumes.Add(1)
			if execution.ExternalRef != "opaque-run-id" || input.(event).kind != "ready" {
				return tflow.DispatchResult{}, fmt.Errorf("missing opaque run or event")
			}
			return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DispatchPending(ctx, 100, d); err != nil {
		t.Fatal(err)
	}
	sub := list(t, s, tflow.SubRunning)[0]
	m, _ := s.GetMain(ctx)
	if sub.ExternalRef != "opaque-run-id" || m.Subs.Running != 1 || m.Subs.TerminalCount() != 0 {
		t.Fatalf("waiting freed slot: %+v %+v", m, sub)
	}
	input := event{id: "callback-1", kind: "ready"}
	if err := e.Resume(ctx, sub.Execution("job"), input); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(ctx, sub.Execution("job"), input); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DispatchPending(ctx, 100, d); err != nil {
		t.Fatal(err)
	}
	m, _ = s.GetMain(ctx)
	if m.Status != tflow.MainSucceeded || resumes.Load() != 1 {
		t.Fatalf("resume: %+v count=%d", m, resumes.Load())
	}
}

func TestSynchronousExecutorDoesNotRecursivelyDrain(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(200), tflow.Options{})
	if _, err := e.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	d, err := tflow.NewExecutorDispatcher(e.Engine, executor{
		start: func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error) {
			return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.DispatchPending(ctx, 1000, d); err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetMain(ctx)
	if m.Status != tflow.MainSucceeded || m.Subs.Succeeded != 200 {
		t.Fatalf("synchronous execution: %+v", m)
	}
}

func TestLateAcceptanceAfterCancelAndRerun(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	canceled := make(chan tflow.Execution, 10)
	d, err := tflow.NewExecutorDispatcher(e.Engine, executor{
		start: func(_ context.Context, task tflow.SubTask, execution tflow.Execution) (tflow.DispatchResult, error) {
			if execution.Attempt == 1 {
				close(entered)
				<-release
				return tflow.DispatchResult{Status: tflow.Accepted, ExternalRef: "old-run"}, nil
			}
			return tflow.DispatchResult{Status: tflow.Accepted, ExternalRef: "new-run"}, nil
		},
		cancel: func(_ context.Context, task tflow.SubTask, execution tflow.Execution) error {
			canceled <- execution
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := e.DispatchPending(ctx, 1, d); done <- err }()
	<-entered
	if _, err := e.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Rerun(ctx, tflow.RerunCanceled); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sub := list(t, s, tflow.SubRunning)[0]
	if sub.Attempt != 2 || sub.ExternalRef != "" {
		t.Fatalf("old acceptance contaminated rerun: %+v", sub)
	}
	if _, err := e.DispatchPending(ctx, 100, d); err != nil {
		t.Fatal(err)
	}
	close(canceled)
	found := false
	for sub := range canceled {
		if sub.Attempt != 1 {
			t.Fatalf("canceled new attempt: %+v", sub)
		}
		if sub.ExternalRef == "old-run" {
			found = true
		}
	}
	if !found {
		t.Fatal("late old external run was not canceled")
	}
}
