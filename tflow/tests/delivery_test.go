package tflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
)

func TestFailedCommandDoesNotStarveFollowingWork(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(2), tflow.Options{})
	if _, err := e.submit(ctx, spec(2)); err != nil {
		t.Fatal(err)
	}
	commands := commandsOf(t, s, tflow.StartSub)
	bad := commands[0].ID
	failure := errors.New("bad command")
	var good int
	dispatcher := tflow.DispatcherFunc(func(_ context.Context, c tflow.Command) error {
		if c.ID == bad {
			return failure
		}
		good++
		return nil
	})
	r, err := e.DispatchBatch(ctx, 10, dispatcher)
	if !errors.Is(err, failure) || r.Failed != 1 || good != 1 || r.Claimed != r.Failed+r.Succeeded {
		t.Fatalf("%+v good=%d %v", r, good, err)
	}
	if _, err := e.DispatchBatch(ctx, 10, dispatcher); err != nil {
		t.Fatal("retried before store clock advanced", err)
	}
	deliveryClock.advance()
	if _, err := e.DispatchBatch(ctx, 10, dispatcher); !errors.Is(err, failure) {
		t.Fatal("failed intent lost")
	}
}

func TestImmediateRetryStillHasClaimBudget(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{RetryPolicy: tflow.RetryPolicyFunc(func(tflow.Delivery, error) time.Duration { return 0 })})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("unavailable")
	r, err := e.DispatchBatch(ctx, 5, tflow.DispatcherFunc(func(context.Context, tflow.Command) error { return failure }))
	if !errors.Is(err, failure) || r.Claimed != 5 || r.Failed == 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRetryBoundsAndTimeoutValidation(t *testing.T) {
	p := tflow.ExponentialRetry{Base: time.Second, Max: 8 * time.Second}
	for _, test := range []struct {
		generation int64
		max        time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {4, 8 * time.Second}, {10000, 8 * time.Second}} {
		delay := p.Delay(tflow.Delivery{Generation: test.generation}, errors.New("retry"))
		if delay < test.max/2 || delay > test.max {
			t.Fatalf("generation %d delay %v", test.generation, delay)
		}
	}
	for _, opts := range []tflow.Options{
		{DeliveryLease: time.Second, DeliveryTimeout: time.Second},
		{DeliveryLease: time.Second, DeliveryTimeout: 2 * time.Second},
	} {
		if _, err := tflow.New(newStore(), opts); !errors.Is(err, tflow.ErrInvalid) {
			t.Fatal(err)
		}
	}
}

func TestObserverCannotVetoDurableWork(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(1), tflow.Options{Observer: tflow.ObserverFunc(func(context.Context, tflow.Observation) { panic("diagnostics failed") })})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	finishAll(t, e, s)
}

func TestTaskUpdateCASAndWakeRollback(t *testing.T) {
	s := newStore()
	e := newEngine(t, s, newSource(2), tflow.Options{})
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updated := *m.Task.(*source)
	if _, err := e.UpdateTask(ctx, m.Version+1, &updated); !errors.Is(err, tflow.ErrConflict) {
		t.Fatal(err)
	}
	changed, err := e.UpdateTask(ctx, m.Version, &updated)
	if err != nil || changed.Version != m.Version+1 || changed.DispatchCursor != m.DispatchCursor {
		t.Fatalf("%+v %v", changed, err)
	}
}

type retryingCallbackStore struct {
	tflow.Store
	retry atomic.Bool
}

func (s *retryingCallbackStore) WithTx(ctx context.Context, fn func(tflow.Tx) error) error {
	replay := errors.New("adapter replay")
	err := s.Store.WithTx(ctx, func(tx tflow.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if s.retry.CompareAndSwap(true, false) {
			return replay
		}
		return nil
	})
	if errors.Is(err, replay) {
		return s.Store.WithTx(ctx, fn)
	}
	return err
}

func TestAdapterCallbackReplayDoesNotDuplicateStateOrReceipts(t *testing.T) {
	s := &retryingCallbackStore{Store: newStore()}
	planner := newSource(2)
	e := newEngine(t, s, planner, tflow.Options{})
	s.retry.Store(true)
	if _, err := e.submit(ctx, spec(1)); err != nil {
		t.Fatal(err)
	}
	if len(planner.state.cursors) != 1 {
		t.Fatal("adapter replay called business planner")
	}
	sub, err := s.GetSub(ctx, "0")
	if err != nil {
		t.Fatal(err)
	}
	s.retry.Store(true)
	receipt, err := e.Report(ctx, tflow.Report{Execution: sub.Execution(e.MainID()), Status: tflow.SubSucceeded})
	if err != nil || !receipt.Changed || receipt.Main.Subs.Succeeded != 1 {
		t.Fatalf("%+v %v", receipt, err)
	}
	m, err := s.GetMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := *m.Task.(*source)
	s.retry.Store(true)
	updated, err := e.UpdateTask(ctx, m.Version, &next)
	if err != nil || updated.Version != m.Version+1 {
		t.Fatalf("%+v %v", updated, err)
	}
}
