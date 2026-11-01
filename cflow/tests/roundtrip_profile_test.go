package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

// profiledStore counts every storage round trip. Put and Get are split by
// outcome so replays of existing records are told apart from new writes.
type profiledStore struct {
	*memory.Infrastructure
	mu     sync.Mutex
	counts map[string]int
}

func (s *profiledStore) add(op string) {
	s.mu.Lock()
	s.counts[op]++
	s.mu.Unlock()
}

func (s *profiledStore) take() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.counts
	s.counts = map[string]int{}
	return out
}

func (s *profiledStore) Get(ctx context.Context, key string) (*Record, error) {
	r, err := s.Infrastructure.Get(ctx, key)
	switch {
	case r == nil:
		s.add("Get(miss)")
	case r.Status == Completed || r.Status == Failed:
		s.add("Get(terminal)")
	default:
		s.add("Get(" + strings.ToLower(string(r.Status)) + ")")
	}
	return r, err
}

func (s *profiledStore) Put(ctx context.Context, r *Record) (*Record, error) {
	before, _ := s.Infrastructure.Get(ctx, r.Key)
	out, err := s.Infrastructure.Put(ctx, r)
	switch {
	case r.Kind == DefinitionKind:
		s.add("Put($definition)")
	case before == nil:
		s.add("Put(new " + strings.ToLower(string(r.Kind)) + ")")
	case before.Status == Suspended:
		s.add("Put(existing waiting)")
	default:
		s.add("Put(existing terminal)")
	}
	return out, err
}

func (s *profiledStore) Commit(ctx context.Context, l Lease, r *Record, v int64) (*Record, error) {
	s.add("Commit")
	return s.Infrastructure.Commit(ctx, l, r, v)
}

func (s *profiledStore) Resolve(ctx context.Context, key string, v int64, raw []byte) (bool, error) {
	s.add("Resolve")
	return s.Infrastructure.Resolve(ctx, key, v, raw)
}

func (s *profiledStore) Claim(ctx context.Context, key string, ttl time.Duration) (*Lease, error) {
	s.add("Claim")
	return s.Infrastructure.Claim(ctx, key, ttl)
}

func (s *profiledStore) Release(ctx context.Context, l Lease) error {
	s.add("Release")
	return s.Infrastructure.Release(ctx, l)
}

func (s *profiledStore) LoadExecution(ctx context.Context, owner string) (*ExecutionSnapshot, error) {
	s.add("LoadExecution")
	return s.Infrastructure.LoadExecution(ctx, owner)
}

func (s *profiledStore) GetFlow(ctx context.Context, key string) (*FlowExecution, error) {
	s.add("GetFlow")
	return s.Infrastructure.GetFlow(ctx, key)
}

func (s *profiledStore) StartFlow(ctx context.Context, item FlowExecution) (*FlowExecution, error) {
	s.add("StartFlow")
	return s.Infrastructure.StartFlow(ctx, item)
}

func (s *profiledStore) CommitFlow(ctx context.Context, l Lease, item *FlowExecution, v int64) (*FlowExecution, error) {
	s.add("CommitFlow")
	return s.Infrastructure.CommitFlow(ctx, l, item, v)
}

func (s *profiledStore) GetLoop(ctx context.Context, key string) (*LoopState, error) {
	s.add("GetLoop")
	return s.Infrastructure.GetLoop(ctx, key)
}

func (s *profiledStore) StartLoop(ctx context.Context, spec LoopSpec, items []FlowExecution) (*LoopState, error) {
	s.add("StartLoop")
	return s.Infrastructure.StartLoop(ctx, spec, items)
}

func (s *profiledStore) LoopResults(ctx context.Context, key string) ([]json.RawMessage, error) {
	s.add("LoopResults")
	return s.Infrastructure.LoopResults(ctx, key)
}

// TestStoreRoundTripProfile logs the storage round trips of every scheduling
// step in a representative run. Run with -v to read the table.
//
//	root:  Now, Run(load), RunFlow(outer), ForEach(items: a, b), Run(done)
//	outer: Run(check), RunFlow(leaf), Run(after)
//	leaf:  Run(prepare), WaitFor(approval), Run(finish)
//	item:  Run(x), WaitFor(ok)
func TestStoreRoundTripProfile(t *testing.T) {
	ctx := context.Background()
	store := &profiledStore{Infrastructure: memory.New(), counts: map[string]int{}}
	step := func(name string) Action[string, string] {
		return Pure(func(in string) string { return name + ":" + in })
	}
	leaf := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
		f.Run("prepare", step("prepare"), id)
		f.WaitFor[string]("approval", id)
		return f.Run("finish", step("finish"), id)
	})
	outer := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
		f.Run("check", step("check"), id)
		out, err := f.RunFlow("leaf", leaf, id)
		if err != nil {
			return "", err
		}
		return f.Run("after", step("after"), out)
	})
	item := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
		f.Run("x", step("x"), id)
		return f.WaitFor[string]("ok", id), nil
	})
	root := FlowFunc[string, string](func(f *FlowContext, id string) (string, error) {
		f.Now("started")
		f.Run("load", step("load"), id)
		out, err := f.RunFlow("outer", outer, id)
		if err != nil {
			return "", err
		}
		if _, err := f.ForEach("items", item, []string{"a", "b"}, identityKey); err != nil {
			return "", err
		}
		return f.Run("done", step("done"), out)
	})
	engine, err := NewEngine(store, Options{Flows: map[string]FlowDefinition{
		"root": root, "outer": outer, "leaf": leaf, "item": item,
	}})
	if err != nil {
		t.Fatal(err)
	}

	type row struct {
		label  string
		status Status
		counts map[string]int
	}
	var rows []row
	record := func(label string, status Status) {
		rows = append(rows, row{label, status, store.take()})
	}
	r, err := engine.Start(ctx, root, "o1")
	if err != nil {
		t.Fatal(err)
	}
	record("Drive root", r.Status)
	done := false
	for range 100 {
		messages, err := store.Pending(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 0 {
			break
		}
		store.take() // Pending/Ack are dispatcher overhead, not engine round trips.
		for _, msg := range messages {
			switch {
			case msg.Purpose == memory.RequestNotification:
				if ok, err := engine.Signal(ctx, msg.Key, "yes"); !ok || err != nil {
					t.Fatalf("signal %s: %t %v", msg.Key, ok, err)
				}
				record("Signal "+msg.Key, Completed)
			case msg.ExecutionKey == "":
				r, err := engine.Start(ctx, root, "o1")
				if err != nil {
					t.Fatal(err)
				}
				record("Drive root", r.Status)
				done = done || r.Status == Completed
			default:
				r, err := engine.Resume(ctx, msg.ExecutionKey)
				if err != nil {
					t.Fatal(err)
				}
				record("Resume "+msg.ExecutionKey, r.Status)
			}
			if err := store.Ack(ctx, msg.ID); err != nil {
				t.Fatal(err)
			}
			store.take()
		}
	}
	if !done {
		t.Fatal("run did not complete")
	}
	// One more replay of the completed root shows the steady replay cost.
	if r, err := engine.Start(ctx, root, "o1"); err != nil || r.Status != Completed {
		t.Fatalf("%+v %v", r, err)
	}
	record("Drive root (replay done)", Completed)

	total := map[string]int{}
	var b strings.Builder
	for _, r := range rows {
		n := 0
		var parts []string
		for op, c := range r.counts {
			n += c
			total[op] += c
			parts = append(parts, fmt.Sprintf("%s=%d", op, c))
		}
		sort.Strings(parts)
		fmt.Fprintf(&b, "%-34s %-10s %3d  %s\n", r.label, r.status, n, strings.Join(parts, " "))
	}
	var ops []string
	sum := 0
	for op, c := range total {
		ops = append(ops, fmt.Sprintf("%s=%d", op, c))
		sum += c
	}
	sort.Strings(ops)
	fmt.Fprintf(&b, "%-34s %-10s %3d  %s\n", "TOTAL", "", sum, strings.Join(ops, " "))
	t.Log("\n" + b.String())
}
