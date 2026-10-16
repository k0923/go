package workflow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlowContextWaitFor(t *testing.T) {
	store := NewMemoryInfrastructure()
	e := testEngine(t, store, Options{})
	var calls int
	flow := definition(FlowFunc[int, string](func(f *FlowContext, in int) (string, error) {
		_, err := f.Run("submit", func(context.Context, int) (int, error) {
			calls++
			return 42, nil
		}, in)
		if err != nil { return "", err }
		return f.WaitFor[string]("callback", in)
	}))
	for i := 0; i < 3; i++ {
		result, err := e.Drive(context.Background(), flow, 2)
		if err != nil || result.Status != Suspended || len(result.Pending) != 1 {
			t.Fatalf("%+v %v", result, err)
		}
	}
	if calls != 1 { t.Fatalf("submit repeated: %d", calls) }
	requests, _ := store.Pending(context.Background(), time.Now())
	if len(requests) != 1 || requests[0].Purpose != RequestNotification { t.Fatal(requests) }
	request, _ := store.Get(context.Background(), "callback")
	if request == nil || request.Status != Suspended { t.Fatal(request) }
	if ok, err := e.ResolveSignal(context.Background(), "callback", request.Version+1, "wrong"); ok || !errors.Is(err, ErrConflict) {
		t.Fatalf("stale callback: %t %v", ok, err)
	}
	if ok, err := e.ResolveSignal(context.Background(), "callback", request.Version, "approved"); !ok || err != nil {
		t.Fatalf("%t %v", ok, err)
	}
	// Delayed acknowledgement of the original request must not consume its wake.
	if err := store.Ack(context.Background(), requests[0].ID); err != nil { t.Fatal(err) }
	messages, _ := store.Pending(context.Background(), time.Now())
	if len(messages) != 1 || messages[0].Purpose != WakeNotification { t.Fatal(messages) }
	if ok, err := e.ResolveSignal(context.Background(), "callback", request.Version, "replacement"); ok || err != nil {
		t.Fatalf("duplicate: %t %v", ok, err)
	}
	for i := 0; i < 2; i++ {
		result, err := e.Drive(context.Background(), flow, 2)
		if err != nil || result.Status != Completed || result.Output != "approved" {
			t.Fatalf("%+v %v", result, err)
		}
	}
	if calls != 1 { t.Fatal(calls) }
	if _, err := e.ResolveSignal(context.Background(), "missing", 1, "early"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestFlowContextSleep(t *testing.T) {
	t.Run("durable deadline", func(t *testing.T) {
		now := time.Now()
		store := NewMemoryInfrastructure()
		store.now = func() time.Time { return now }
		e := testEngine(t, store, Options{Now: func() time.Time { return now }})
		flow := definition(FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			if err := f.Sleep("delay", time.Hour); err != nil { return "", err }
			return "awake", nil
		}))
		first, err := e.Drive(context.Background(), flow, 0)
		if err != nil || first.Status != Suspended { t.Fatalf("%+v %v", first, err) }
		original := first.Pending[0].Until
		now = now.Add(30 * time.Minute)
		second, err := e.Drive(context.Background(), flow, 0)
		if err != nil || second.Status != Suspended || second.Pending[0].Until != original { t.Fatalf("%+v %v", second, err) }
		messages, _ := store.Pending(context.Background(), now)
		if len(messages) != 0 { t.Fatal(messages) }
		now = now.Add(30 * time.Minute)
		for i := 0; i < 2; i++ {
			result, err := e.Drive(context.Background(), flow, 0)
			if err != nil || result.Status != Completed || result.Output != "awake" { t.Fatalf("%+v %v", result, err) }
		}
	})
	t.Run("immediate and sleep until", func(t *testing.T) {
		e := testEngine(t, NewMemoryInfrastructure(), Options{})
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			if err := f.Sleep("zero", 0); err != nil { return 0, err }
			if err := f.SleepUntil("past", time.Now().Add(-time.Hour)); err != nil { return 0, err }
			return in, nil
		}))
		result, err := e.Drive(context.Background(), flow, 8)
		if err != nil || result.Status != Completed || result.Output != 8 { t.Fatalf("%+v %v", result, err) }
	})
}

func TestFlowContextCheckpoint(t *testing.T) {
	now := time.Now()
	store := NewMemoryInfrastructure()
	e := testEngine(t, store, Options{Now: func() time.Time { return now }})
	type output struct { ID string; At time.Time }
	flow := definition(FlowFunc[int, output](func(f *FlowContext, _ int) (output, error) {
		id, err := f.UUID("id")
		if err != nil { return output{}, err }
		at, err := f.Now("at")
		return output{ID: id, At: at}, err
	}))
	first, err := e.Drive(context.Background(), flow, 0)
	if err != nil || first.Output.ID == "" || !first.Output.At.Equal(now) { t.Fatalf("%+v %v", first, err) }
	now = now.Add(time.Hour)
	second, err := e.Drive(context.Background(), flow, 0)
	if err != nil || second.Output != first.Output { t.Fatalf("%+v %v", second, err) }
}

type pollOutput struct { Count int; Finished bool }
func (o pollOutput) Done() bool { return o.Finished }

func TestFlowContextLoop(t *testing.T) {
	for _,finish := range []bool{true, false} {
		t.Run(map[bool]string{true: "completed", false: "exhausted"}[finish], func(t *testing.T) {
			now := time.Now()
			store := NewMemoryInfrastructure()
			store.now = func() time.Time { return now }
			e := testEngine(t, store, Options{Now: func() time.Time { return now }})
			calls := 0
			flow := definition(FlowFunc[int, pollOutput](func(f *FlowContext, _ int) (pollOutput, error) {
				return f.Loop("poll", func(context.Context, int) (pollOutput, error) {
					calls++
					return pollOutput{Count: calls, Finished: finish && calls == 2}, nil
				}, 0, LoopOptions{Interval: time.Hour, MaxAttempts: 2})
			}))
			for i := 0; i < 3; i++ {
				result, err := e.Drive(context.Background(), flow, 0)
				if err != nil || result.Status != Suspended || calls != 1 { t.Fatalf("%+v %v calls=%d", result, err, calls) }
			}
			now = now.Add(time.Hour)
			for i := 0; i < 2; i++ {
				result, err := e.Drive(context.Background(), flow, 0)
				if finish && (err != nil || result.Output.Count != 2 || result.Status != Completed) { t.Fatalf("%+v %v", result, err) }
				var permanent *PermanentError
				if !finish && (!errors.As(err, &permanent) || result.Status != Failed) { t.Fatalf("%+v %v", result, err) }
			}
			if calls != 2 { t.Fatal(calls) }
		})
	}
}

func TestFutureWait(t *testing.T) {
	store := NewMemoryInfrastructure()
	e := testEngine(t, store, Options{})
	var calls atomic.Int32
	flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
		future := f.Go("async", func(context.Context, int) (int, error) { calls.Add(1); return 21, nil }, in)
		var wg sync.WaitGroup
		errorsOut := make(chan error, 16)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				value, err := future.Wait()
				if err == nil && value != 21 { err = errors.New("wrong future value") }
				errorsOut <- err
			}()
		}
		wg.Wait()
		close(errorsOut)
		for err := range errorsOut { if err != nil { return 0, err } }
		return future.Wait()
	}))
	result, err := e.Drive(context.Background(), flow, 0)
	if err != nil || result.Output != 21 || calls.Load() != 1 { t.Fatalf("%+v %v", result, err) }
	unfinished := &Future[int]{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background()); cancel()
	if _, err := unfinished.WaitContext(ctx); !errors.Is(err, context.Canceled) { t.Fatal(err) }
}

func TestFlowContextGoFlow(t *testing.T) {
	t.Run("all suspensions collected", func(t *testing.T) {
		e := testEngine(t, NewMemoryInfrastructure(), Options{})
		child := FlowFunc[int, string](func(f *FlowContext, in int) (string, error) { return f.WaitFor[string]("callback", in) })
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			f.GoFlow("left", child, in)
			f.GoFlow("right", child, in)
			return 99, nil
		}))
		result, err := e.Drive(context.Background(), flow, 0)
		if err != nil || result.Status != Suspended || len(result.Pending) != 2 { t.Fatalf("%+v %v", result, err) }
		if result.Pending[0].Key != "left|callback" || result.Pending[1].Key != "right|callback" { t.Fatal(result.Pending) }
	})
	t.Run("unwaited errors propagated and siblings joined", func(t *testing.T) {
		e := testEngine(t, NewMemoryInfrastructure(), Options{})
		started := make(chan struct{})
		finished := make(chan struct{})
		cause := errors.New("notify failed")
		flow := definition(FlowFunc[int, int](func(f *FlowContext, in int) (int, error) {
			f.Go("slow", func(ctx context.Context, _ int) (int, error) {
				close(started)
				<-ctx.Done()
				close(finished)
				return 0, ctx.Err()
			}, in)
			<-started
			f.Go("notify", func(context.Context, int) (int, error) { return 0, cause }, in)
			return 999, nil
		}))
		result, err := e.Drive(context.Background(), flow, 0)
		if !errors.Is(err, cause) || result.Status == Completed { t.Fatalf("%+v %v", result, err) }
		select { case <-finished: default: t.Fatal("driver returned before sibling exited") }
	})
}
