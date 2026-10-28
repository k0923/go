package tflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type syncExecutor func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error)

func (f syncExecutor) Start(ctx context.Context, task tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
	if f == nil {
		return tflow.DispatchResult{}, tflow.ErrExecutorUnavailable
	}
	result, err := f(ctx, task, ref)
	if err == nil && (result.Status == tflow.Accepted || result.Status == tflow.Waiting) {
		return tflow.DispatchResult{}, tflow.ErrInvalid
	}
	return result, err
}

func (syncExecutor) Resume(context.Context, tflow.SubTask, tflow.Execution, tflow.Event) (tflow.DispatchResult, error) {
	return tflow.DispatchResult{}, tflow.ErrResumeUnsupported
}

func (syncExecutor) Cancel(context.Context, tflow.SubTask, tflow.Execution) error { return nil }

func TestConstructorsRejectTypedNilDependencies(t *testing.T) {
	var storage *memory.Store
	if _, err := tflow.New(storage, tflow.Options{}); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("typed nil store: %v", err)
	}
	var exec *executor
	var dispatch *tflow.ExecutorDispatcher
	var clock tflow.ClockFunc
	var handler tflow.TerminationHandlerFunc
	for _, opts := range []tflow.Options{
		{Executor: exec}, {Dispatcher: dispatch}, {Clock: clock}, {TerminationHandler: handler},
		{Executor: execution{}, Dispatcher: &memory.FakeDispatcher{}},
	} {
		if _, err := tflow.New(memory.New().ForMain("job"), opts); !errors.Is(err, tflow.ErrInvalid) {
			t.Fatalf("invalid dependency accepted: %+v %v", opts, err)
		}
	}
	e, _ := tflow.New(memory.New().ForMain("job"), tflow.Options{})
	if _, err := tflow.NewExecutorDispatcher(e, exec); !errors.Is(err, tflow.ErrInvalid) {
		t.Fatalf("typed nil executor: %v", err)
	}
}

func TestDirectExecutorRespectsDeliveryBudgetAndRecovers(t *testing.T) {
	s := newStore()
	starts := 0
	keyCounts := make(map[string]int)
	x := syncExecutor(func(_ context.Context, _ tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
		starts++
		keyCounts[ref.Key()]++
		return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
	})
	e := newEngine(t, s, newSource(5), tflow.Options{Executor: x, DispatchLimit: 1})
	m, err := e.submit(ctx, spec(2))
	if err != nil || m.Status != tflow.MainRunning || starts != 1 {
		t.Fatalf("unbounded automatic delivery: %+v starts=%d err=%v", m, starts, err)
	}
	recovered := newEngine(t, s, nil, tflow.Options{Executor: x})
	if _, err := recovered.Drain(ctx, 100); err != nil {
		t.Fatal(err)
	}
	m, _ = recovered.GetMain(ctx)
	if m.Status != tflow.MainSucceeded || starts != 5 {
		t.Fatalf("recovery: %+v starts=%d", m, starts)
	}
	for key, count := range keyCounts {
		if count != 1 {
			t.Fatalf("recursive or duplicate execution: %s count=%d", key, count)
		}
	}
}

func TestExecutorAdaptersKeepFailureSemantics(t *testing.T) {
	failure := errors.New("database down")
	retry := syncExecutor(func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error) {
		return tflow.DispatchResult{}, failure
	})
	if _, err := retry.Start(ctx, unit("u"), tflow.Execution{}); !errors.Is(err, failure) {
		t.Fatal("infrastructure error lost")
	}
	for _, status := range []tflow.DispatchStatus{tflow.Accepted, tflow.Waiting} {
		x := syncExecutor(func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error) {
			return tflow.DispatchResult{Status: status}, nil
		})
		if _, err := x.Start(ctx, unit("u"), tflow.Execution{}); !errors.Is(err, tflow.ErrInvalid) {
			t.Fatalf("async acceptance in sync adapter: %v", err)
		}
	}
	f := tflow.ExecutorFuncs{}
	if _, err := f.Start(ctx, nil, tflow.Execution{}); !errors.Is(err, tflow.ErrExecutorUnavailable) {
		t.Fatal(err)
	}
	if _, err := f.Resume(ctx, nil, tflow.Execution{}, nil); !errors.Is(err, tflow.ErrResumeUnsupported) {
		t.Fatal(err)
	}
	if err := f.Cancel(ctx, nil, tflow.Execution{}); !errors.Is(err, tflow.ErrCancelUnsupported) {
		t.Fatal(err)
	}
}

func TestDirectExecutorRetryDoesNotBecomeBusinessFailure(t *testing.T) {
	s := newStore()
	transient := errors.New("temporary execution IO failure")
	var keys []string
	x := syncExecutor(func(_ context.Context, _ tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
		keys = append(keys, ref.Key())
		if len(keys) == 1 {
			return tflow.DispatchResult{}, transient
		}
		return tflow.DispatchResult{Status: tflow.ExecutionFailed, Error: "business rejected"}, nil
	})
	e := newEngine(t, s, newSource(1), tflow.Options{Executor: x})
	m, err := e.submit(ctx, spec(1))
	var committed *tflow.CommittedError
	if !errors.Is(err, transient) || !errors.As(err, &committed) || m.Status != tflow.MainRunning || m.Subs.Failed != 0 {
		t.Fatalf("retry was counted as business failure: %+v %v", m, err)
	}
	deliveryClock.advance()
	if _, err := e.Drain(ctx, 100); err != nil {
		t.Fatal(err)
	}
	m, _ = e.GetMain(ctx)
	if m.Status != tflow.MainFailed || m.Subs.Failed != 1 || len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("execution failure or idempotency lost: %+v keys=%v", m, keys)
	}
}

type transactionForbiddenStore struct{ *memory.Store }

func (*transactionForbiddenStore) WithTx(context.Context, func(tflow.Tx) error) error {
	return errors.New("read must not open a write transaction")
}

func TestListSubTasksUsesReadPort(t *testing.T) {
	repository := newRepository()
	s := repository.ForMain("job")
	e := newEngine(t, s, newSource(4), tflow.Options{PlanningPolicy: tflow.BufferPolicy{TargetPending: 2}})
	if _, err := e.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	reader, _ := tflow.New(&transactionForbiddenStore{Store: s}, tflow.Options{})
	subs, err := reader.ListSubTasks(ctx, tflow.SubQuery{Statuses: []tflow.SubStatus{tflow.SubPending}, Limit: 1})
	if err != nil || len(subs) != 1 || subs[0].Cursor != 2 {
		t.Fatalf("read: %+v %v", subs, err)
	}
	subs[0].Error = "local change"
	sub, _ := reader.GetSub(ctx, subs[0].Key)
	if sub.Error != "" {
		t.Fatal("query leaked mutable snapshot")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.ListSubTasks(canceled, tflow.SubQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	missing, _ := tflow.New(repository.ForMain("missing"), tflow.Options{})
	if _, err := missing.ListSubTasks(ctx, tflow.SubQuery{}); !errors.Is(err, tflow.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestPlanErrorPreservesIdentityAndField(t *testing.T) {
	m, _ := tflow.NewMain(&mainTask{}, tflow.SubmitOptions{ConcurrencyLimit: 2}, time.Time{})
	_, _, err := tflow.ApplyPlan(m, tflow.Page{Items: []tflow.SubTask{unit("a"), unit("a")}}, 2, time.Time{})
	var detail *tflow.PlanError
	if !errors.Is(err, tflow.ErrInvalidPlan) || !errors.As(err, &detail) ||
		detail.MainID != "" || detail.Field != "Key" || detail.Index != 1 || !strings.Contains(detail.Reason, `"a"`) {
		t.Fatalf("missing context: %#v %v", detail, err)
	}
}
