package tests

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	. "github.com/k0923/go/cflow"
)

func TestForEachSlidingWindow(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		var mu sync.Mutex
		calls := map[string]int{}
		body := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			return f.Run("submit", AsAction(func(_ context.Context, id string) (string, error) {
				mu.Lock()
				calls[id]++
				mu.Unlock()
				return id + "!", nil
			}), id)
		})
		input := make([]string, 8)
		want := make([]string, 8)
		for i := range input {
			input[i] = fmt.Sprint(i)
			want[i] = input[i] + "!"
		}
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
			return f.ForEach("orders", body, input, identityKey, Concurrency(5))
		}))
		if r, err := testEngine(t, store, opts).Start(ctx, wf, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		// The first round admits exactly the window; the rest stay queued.
		loop, err := store.GetLoop(ctx, "orders")
		if err != nil || loop.AdmittedCount != 5 || loop.RunningCount != 5 || loop.Concurrency != 5 {
			t.Fatalf("initial window: %+v %v", loop, err)
		}
		for i, id := range input {
			item, err := store.GetFlow(ctx, "orders|item["+id+"]")
			wantStatus := Running
			if i >= 5 {
				wantStatus = Queued
			}
			if err != nil || item.Status != wantStatus {
				t.Fatalf("item %d: %+v %v", i, item, err)
			}
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil || len(messages) != 5 {
			t.Fatalf("initial wakes: %+v %v", messages, err)
		}
		// Finishing an item out of order admits the next queued one.
		if r, err := testEngine(t, store, opts).Resume(ctx, "orders|item[2]"); err != nil || r.Status != Completed {
			t.Fatalf("%+v %v", r, err)
		}
		loop, err = store.GetLoop(ctx, "orders")
		if err != nil || loop.AdmittedCount != 6 || loop.RunningCount != 5 || loop.CompletedCount != 1 {
			t.Fatalf("slide: %+v %v", loop, err)
		}
		if next, err := store.GetFlow(ctx, "orders|item[5]"); err != nil || next.Status != Running {
			t.Fatalf("next not admitted: %+v %v", next, err)
		}
		if r, err := testEngine(t, store, opts).Resume(ctx, "orders|item[7]"); !errors.Is(err, ErrConflict) || r.Status != Running {
			t.Fatalf("queued item ran: %+v %v", r, err)
		}
		result := drainLoop(t, store, opts, wf, 0)
		if !slices.Equal(result.Output, want) {
			t.Fatalf("outputs must follow input order: %+v", result)
		}
		for _, id := range input {
			if calls[id] != 1 {
				t.Fatalf("calls: %v", calls)
			}
		}
	})
}

func TestForEachFailureStopsAdmissionAndWaitsForRunning(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		var mu sync.Mutex
		calls := map[string]int{}
		body := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			if _, err := f.Run("submit", AsAction(func(_ context.Context, id string) (string, error) {
				mu.Lock()
				calls[id]++
				mu.Unlock()
				if id == "b" {
					return "", Permanent(errors.New("rejected"))
				}
				return id, nil
			}), id); err != nil {
				return "", err
			}
			return f.WaitFor[string]("approval", id), nil
		})
		var caught error
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
			out, err := f.ForEach("orders", body, []string{"a", "b", "c", "d"}, identityKey, Concurrency(2))
			caught = err
			return out, nil
		}))
		if r, err := testEngine(t, store, opts).Start(ctx, wf, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		if r, err := testEngine(t, store, opts).Resume(ctx, "orders|item[b]"); r.Status != Failed || err == nil {
			t.Fatalf("%+v %v", r, err)
		}
		// b failed while a is still running: admission stops, the parent waits.
		loop, err := store.GetLoop(ctx, "orders")
		if err != nil || loop.Status != Suspended || loop.Failure == nil || loop.AdmittedCount != 2 || loop.RunningCount != 1 {
			t.Fatalf("after failure: %+v %v", loop, err)
		}
		if item, err := store.GetFlow(ctx, "orders|item[c]"); err != nil || item.Status != Queued {
			t.Fatalf("c admitted after failure: %+v %v", item, err)
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range messages {
			if msg.Key == "orders" {
				t.Fatalf("parent woken before running items finished: %+v", msg)
			}
		}
		result := drainLoop(t, store, opts, wf, 0)
		var itemErr *ForEachError
		if result.Output != nil || !errors.As(caught, &itemErr) || itemErr.Index != 1 || itemErr.Key != "orders|item[b]" {
			t.Fatalf("%+v caught=%v", result, caught)
		}
		loop, err = store.GetLoop(ctx, "orders")
		if err != nil || loop.Status != Failed || loop.RunningCount != 0 || loop.CompletedCount != 1 {
			t.Fatalf("final: %+v %v", loop, err)
		}
		if calls["a"] != 1 || calls["c"] != 0 || calls["d"] != 0 {
			t.Fatalf("calls: %v", calls)
		}
	})
}

func TestForItemsDiscardsOutputs(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		// Any registered Flow can be reused; its output is dropped.
		body := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			return f.Run("notify", Pure(func(id string) string { return id + "!" }), id)
		})
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		wf := namedDefinition("batch", FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			if err := f.ForItems("notify", body, []string{"a", "b", "c"}, identityKey, Concurrency(3)); err != nil {
				return "", err
			}
			return "done", nil
		}))
		if r, err := testEngine(t, store, opts).Start(ctx, wf, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		if result := drainLoop(t, store, opts, wf, 0); result.Output != "done" {
			t.Fatalf("%+v", result)
		}
		item, err := store.GetFlow(ctx, "notify|item[a]")
		if err != nil || item.Status != Completed || (len(item.Output) != 0 && string(item.Output) != "null") {
			t.Fatalf("output kept: %+v %v", item, err)
		}
		root, err := store.(ExecutionLoader).LoadExecution(ctx, "")
		if err != nil || len(root.Loops) != 1 || len(root.Loops[0].Outputs) != 0 {
			t.Fatalf("root preload: %+v %v", root, err)
		}
		measured := &loopCounter{loopBackend: store}
		if r, err := testEngine(t, measured, opts).Start(ctx, wf, 0); err != nil || r.Output != "done" {
			t.Fatalf("%+v %v", r, err)
		}
		if measured.loops.Load() != 0 || measured.results.Load() != 0 {
			t.Fatalf("replay read loop=%d results=%d", measured.loops.Load(), measured.results.Load())
		}
	})
}

func TestForEachConcurrencyChange(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		body := FlowFunc[string, string](func(_ *FlowContext, id string) (string, error) { return id, nil })
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		n := 2
		wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
			return f.ForEach("orders", body, []string{"a", "b", "c"}, identityKey, Concurrency(n))
		}))
		if r, err := testEngine(t, store, opts).Start(ctx, wf, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		// A changed window is not drift; the registered one keeps applying.
		n = 3
		if r, err := testEngine(t, store, opts).Start(ctx, wf, 0); err != nil || r.Status != Suspended {
			t.Fatalf("changed concurrency: %+v %v", r, err)
		}
		if loop, err := store.GetLoop(ctx, "orders"); err != nil || loop.AdmittedCount != 2 {
			t.Fatalf("%+v %v", loop, err)
		}
		if result := drainLoop(t, store, opts, wf, 0); !slices.Equal(result.Output, []string{"a", "b", "c"}) {
			t.Fatalf("%+v", result)
		}
	})
}

func TestForEachNegativeConcurrency(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		body := FlowFunc[string, string](func(_ *FlowContext, id string) (string, error) { return id, nil })
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
			return f.ForEach("orders", body, []string{"a"}, identityKey, Concurrency(-1))
		}))
		if r, err := testEngine(t, store, opts).Start(context.Background(), wf, 0); r.Status != Failed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("negative concurrency: %+v %v", r, err)
		}
		if loop, err := store.GetLoop(context.Background(), "orders"); err != nil || loop != nil {
			t.Fatalf("partial registration: %+v %v", loop, err)
		}
	})
}

func TestForEachConcurrentDispatcher(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		body := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			return f.Run("submit", AsAction(func(_ context.Context, id string) (string, error) { return id, nil }), id)
		})
		input := make([]string, 20)
		for i := range input {
			input[i] = fmt.Sprint(i)
		}
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
			return f.ForEach("orders", body, input, identityKey, Concurrency(4))
		}))
		if r, err := testEngine(t, store, opts).Start(ctx, wf, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		// A dispatcher that resumes every pending item wake in parallel.
		for range 100 {
			messages, err := store.Pending(ctx, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for _, msg := range messages {
				if msg.ExecutionKey == "" {
					continue
				}
				wg.Go(func() {
					r, err := testEngine(t, store, opts).Resume(ctx, msg.ExecutionKey)
					if err != nil || r.Status != Completed {
						t.Errorf("%s: %+v %v", msg.ExecutionKey, r, err)
					}
					if err := store.Ack(ctx, msg.ID); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			loop, err := store.GetLoop(ctx, "orders")
			if err != nil {
				t.Fatal(err)
			}
			if loop.RunningCount > 4 || loop.AdmittedCount-loop.CompletedCount != loop.RunningCount {
				t.Fatalf("window broken: %+v", loop)
			}
			if loop.Status == Completed {
				break
			}
		}
		result := drainLoop(t, store, opts, wf, 0)
		if !slices.Equal(result.Output, input) {
			t.Fatalf("%+v", result)
		}
	})
}

func TestApplyFlowCompletionKeepsFatalFailure(t *testing.T) {
	loop := NewLoopState(LoopSpec{Key: "l", Total: 3, Concurrency: 3})
	item := func(i int) *FlowExecution {
		return &FlowExecution{Key: fmt.Sprintf("l|%d", i), LoopKey: "l", Index: i, Status: Running, Version: 1}
	}
	items := []*FlowExecution{item(0), item(1), item(2)}
	results := []*FlowExecution{
		{Key: "l|0", Status: Failed, Failure: &FlowFailure{Message: "business"}},
		{Key: "l|1", Status: Failed, Failure: &FlowFailure{Message: "panic", Fatal: true}},
		{Key: "l|2", Status: Completed, Output: []byte(`"ok"`)},
	}
	for i := range items {
		next, err := ApplyFlowCompletion(loop, items[i], results[i], 1)
		if err != nil || next != -1 {
			t.Fatalf("item %d: next=%d %v", i, next, err)
		}
	}
	if loop.Status != Failed || loop.FailedKey != "l|1" || !loop.Failure.Fatal || loop.RunningCount != 0 || loop.CompletedCount != 1 {
		t.Fatalf("%+v", loop)
	}
	if _, err := ApplyFlowCompletion(loop, items[2], results[2], 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal recommit: %v", err)
	}
}
