package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

type measuredLoopStore struct {
	*memory.Infrastructure
	mu             sync.Mutex
	gets           []string
	lists, results int
	loseCommit     bool
}

func (s *measuredLoopStore) Get(ctx context.Context, key string) (*Record, error) {
	s.mu.Lock()
	s.gets = append(s.gets, key)
	s.mu.Unlock()
	return s.Infrastructure.Get(ctx, key)
}

func (s *measuredLoopStore) Records(ctx context.Context) ([]*Record, error) {
	s.lists++
	return s.Infrastructure.Records(ctx)
}

func (s *measuredLoopStore) LoopResults(ctx context.Context, key string) ([]json.RawMessage, error) {
	s.results++
	return s.Infrastructure.LoopResults(ctx, key)
}

func (s *measuredLoopStore) CommitFlow(ctx context.Context, lease Lease, item *FlowExecution, version int64) (*FlowExecution, error) {
	out, err := s.Infrastructure.CommitFlow(ctx, lease, item, version)
	if err == nil && s.loseCommit {
		s.loseCommit = false
		return nil, errors.New("response lost after commit")
	}
	return out, err
}

func TestForEachThousandWaitsReadOnlyTheirOwnSteps(t *testing.T) {
	ctx := context.Background()
	store := &measuredLoopStore{Infrastructure: memory.New()}
	input := make([]string, 1000)
	for i := range input {
		input[i] = fmt.Sprint(i)
	}
	rootCalls, actionCalls, bodyCalls := 0, 0, 0
	body := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
		bodyCalls++
		if _, err := f.Run("submit", AsAction(func(_ context.Context, id string) (string, error) {
			actionCalls++
			return id, nil
		}), id); err != nil {
			return "", err
		}
		return f.WaitFor[string]("approval", id), nil
	})
	opts := Options{Flows: map[string]FlowDefinition{"item": body}}
	wf := namedDefinition("batch", FlowFunc[[]string, []string](func(f *FlowContext, in []string) ([]string, error) {
		rootCalls++
		return f.ForEach("orders", body, in, identityKey)
	}))
	engine := testEngine(t, store, opts)
	if r, err := engine.Start(ctx, wf, input); err != nil || r.Status != Suspended {
		t.Fatalf("%+v %v", r, err)
	}
	for i, id := range input {
		key := "orders|item[" + id + "]"
		before := len(store.gets)
		r, err := engine.Resume(ctx, key)
		if err != nil || r.Status != Suspended {
			t.Fatalf("item %d: %+v %v", i, r, err)
		}
		if ok, err := engine.Signal(ctx, key+"|approval", id); !ok || err != nil {
			t.Fatalf("%t %v", ok, err)
		}
		if i == 500 {
			store.loseCommit = true
		}
		r, err = testEngine(t, store, opts).Resume(ctx, key)
		if i == 500 {
			if err == nil || r.Status != Running {
				t.Fatalf("lost response: %+v %v", r, err)
			}
			calls, gets := bodyCalls, len(store.gets)
			r, err = testEngine(t, store, opts).Resume(ctx, key)
			if bodyCalls != calls || len(store.gets) != gets {
				t.Fatal("committed child reran after lost response")
			}
		}
		if err != nil || r.Status != Completed {
			t.Fatalf("item %d: %+v %v", i, r, err)
		}
		for _, read := range store.gets[before:] {
			if !strings.HasPrefix(read, key+"|") {
				t.Fatalf("%s read unrelated %s", key, read)
			}
		}
		state, err := store.GetLoop(ctx, "orders")
		if err != nil || state.CompletedCount != i+1 {
			t.Fatalf("progress: %+v %v", state, err)
		}
	}
	// Outputs arrive with the root preload, so LoopResults is never called.
	result, err := engine.Start(ctx, wf, input)
	if err != nil || result.Status != Completed || !slices.Equal(result.Output, input) ||
		rootCalls != 2 || actionCalls != 1000 || bodyCalls != 2000 || store.lists != 0 || store.results != 0 || len(store.gets) > 10000 {
		t.Fatalf("result=%s err=%v root=%d action=%d body=%d lists=%d results=%d gets=%d",
			result.Status, err, rootCalls, actionCalls, bodyCalls, store.lists, store.results, len(store.gets))
	}
	t.Logf("1000 waits: root=%d, body=%d, action=%d, step reads=%d, full scans=%d, result batches=%d",
		rootCalls, bodyCalls, actionCalls, len(store.gets), store.lists, store.results)
}

func TestForEachCheckpointAndBoundaryPreload(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		body := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
			return f.Run("submit", Pure(func(id string) string { return id + "!" }), id)
		})
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
			f.Now("started")
			return f.ForEach("orders", body, []string{"a", "b"}, identityKey)
		}))
		if r, err := testEngine(t, store, opts).Start(ctx, wf, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		result := drainLoop(t, store, opts, wf, 0)
		if !slices.Equal(result.Output, []string{"a!", "b!"}) {
			t.Fatalf("%+v", result)
		}
		loader := store.(ExecutionLoader)
		root, err := loader.LoadExecution(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		// The root owns only its own Now and the loop with its outputs; items'
		// steps belong to the items, and the framework adds no records.
		if len(root.Records) != 1 || root.Records[0].Key != "started" || len(root.Flows) != 0 ||
			len(root.Loops) != 1 || root.Loops[0].Key != "orders" || len(root.Loops[0].Outputs) != 2 {
			t.Fatalf("root preload: %+v", root)
		}
		item, err := loader.LoadExecution(ctx, "orders|item[a]")
		if err != nil || len(item.Records) != 1 || item.Records[0].Key != "orders|item[a]|submit" || len(item.Loops) != 0 {
			t.Fatalf("item preload: %+v %v", item, err)
		}
		// Replays use the preloaded loop, not GetLoop or LoopResults.
		measured := &loopCounter{loopBackend: store}
		for range 2 {
			r, err := testEngine(t, measured, opts).Start(ctx, wf, 0)
			if err != nil || !slices.Equal(r.Output, []string{"a!", "b!"}) {
				t.Fatalf("%+v %v", r, err)
			}
		}
		if measured.loops.Load() != 0 || measured.results.Load() != 0 || measured.gets.Load() != 0 {
			t.Fatalf("replay read loop=%d results=%d gets=%d", measured.loops.Load(), measured.results.Load(), measured.gets.Load())
		}
	})
}

// Standalone children appear only in their parent's boundary, nested ones too.
func TestLoadExecutionStandaloneChildBoundaries(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		leaf := FlowFunc[string, string](func(f *FlowContext, in string) (string, error) {
			return f.Run("step", Pure(func(s string) string { return s }), in)
		})
		middle := FlowFunc[string, string](func(f *FlowContext, in string) (string, error) {
			return f.RunFlow("leaf", leaf, in)
		})
		root := FlowFunc[int, string](func(f *FlowContext, _ int) (string, error) {
			return f.RunFlow("middle", middle, "a")
		})
		engine := testEngine(t, store, Options{Flows: map[string]FlowDefinition{"root": root, "middle": middle, "leaf": leaf}})
		if r, err := engine.Start(ctx, root, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		if r, err := engine.Resume(ctx, "middle"); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		if r, err := engine.Resume(ctx, "middle|leaf"); err != nil || r.Status != Completed {
			t.Fatalf("%+v %v", r, err)
		}
		loader := store.(ExecutionLoader)
		want := map[string][2]string{"": {"middle", ""}, "middle": {"middle|leaf", ""}, "middle|leaf": {"", "middle|leaf|step"}}
		for owner, keys := range want {
			snapshot, err := loader.LoadExecution(ctx, owner)
			if err != nil {
				t.Fatal(err)
			}
			var flows, records []string
			for _, item := range snapshot.Flows {
				flows = append(flows, item.Key)
			}
			for _, r := range snapshot.Records {
				records = append(records, r.Key)
			}
			if strings.Join(flows, ",") != keys[0] || strings.Join(records, ",") != keys[1] || len(snapshot.Loops) != 0 {
				t.Fatalf("%q: flows=%v records=%v loops=%d", owner, flows, records, len(snapshot.Loops))
			}
		}
	})
}

type loopCounter struct {
	loopBackend
	loops, results, gets atomic.Int32
}

func (s *loopCounter) GetLoop(ctx context.Context, key string) (*LoopState, error) {
	s.loops.Add(1)
	return s.loopBackend.GetLoop(ctx, key)
}

func (s *loopCounter) LoopResults(ctx context.Context, key string) ([]json.RawMessage, error) {
	s.results.Add(1)
	return s.loopBackend.LoopResults(ctx, key)
}

func (s *loopCounter) Get(ctx context.Context, key string) (*Record, error) {
	s.gets.Add(1)
	return s.loopBackend.Get(ctx, key)
}

func (s *loopCounter) LoadExecution(ctx context.Context, owner string) (*ExecutionSnapshot, error) {
	return s.loopBackend.(ExecutionLoader).LoadExecution(ctx, owner)
}

func TestForEachCompletionFencingAndIdempotence(t *testing.T) {
	eachLoopStore(t, func(t *testing.T, store loopBackend, _ func() loopBackend) {
		ctx := context.Background()
		body := FlowFunc[string, string](func(_ *FlowContext, id string) (string, error) { return id, nil })
		opts := Options{Flows: map[string]FlowDefinition{"item": body}}
		wf := namedDefinition("batch", FlowFunc[int, []string](func(f *FlowContext, _ int) ([]string, error) {
			return f.ForEach("orders", body, []string{"a", "b"}, identityKey)
		}))
		if r, err := testEngine(t, store, opts).Start(ctx, wf, 0); err != nil || r.Status != Suspended {
			t.Fatalf("%+v %v", r, err)
		}
		key := "orders|item[a]"
		item, err := store.GetFlow(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		old, err := store.Claim(ctx, key, time.Minute)
		if err != nil || old == nil {
			t.Fatalf("%+v %v", old, err)
		}
		if err := store.Release(ctx, *old); err != nil {
			t.Fatal(err)
		}
		current, err := store.Claim(ctx, key, time.Minute)
		if err != nil || current == nil {
			t.Fatalf("%+v %v", current, err)
		}
		defer store.Release(ctx, *current)
		item.Status, item.Output = Completed, json.RawMessage(`"a"`)
		if _, err := store.CommitFlow(ctx, *old, item, item.Version); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale lease: %v", err)
		}
		if _, err := store.CommitFlow(ctx, *current, item, item.Version+1); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale version: %v", err)
		}
		loop, err := store.GetLoop(ctx, "orders")
		if err != nil || loop.CompletedCount != 0 {
			t.Fatalf("rejected commit changed count: %+v %v", loop, err)
		}
		for range 3 {
			saved, err := store.CommitFlow(ctx, *current, item, item.Version)
			if err != nil || saved.Status != Completed {
				t.Fatalf("%+v %v", saved, err)
			}
		}
		// Mutating a returned snapshot cannot change durable data.
		item.Output[1] = 'x'
		loop, err = store.GetLoop(ctx, "orders")
		if err != nil || loop.CompletedCount != 1 {
			t.Fatalf("%+v %v", loop, err)
		}
		saved, err := store.GetFlow(ctx, key)
		if err != nil || string(saved.Output) != `"a"` {
			t.Fatalf("aliased output: %+v %v", saved, err)
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil || len(messages) != 2 {
			t.Fatalf("duplicate/missing wakes: %+v %v", messages, err)
		}
		next, err := store.GetFlow(ctx, "orders|item[b]")
		if err != nil || next.Status != Running {
			t.Fatalf("next not activated: %+v %v", next, err)
		}
	})
}
