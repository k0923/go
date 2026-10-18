package tests

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/sqlite"
)

type countedProvider struct {
	dflow.DefinitionProvider
	mu    sync.Mutex
	loads map[string]int
}

func (p *countedProvider) LoadNode(ctx context.Context, id string) (*dflow.PublishedNode, error) {
	p.mu.Lock()
	p.loads[id]++
	p.mu.Unlock()
	return p.DefinitionProvider.LoadNode(ctx, id)
}

func TestPersistedPublicationLazyResume(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lazy.db")
	store, err := sqlite.Open(ctx, path, "run")
	if err != nil {
		t.Fatal(err)
	}
	registry := asyncRegistry(t, asyncNode{
		run: func(context.Context, map[string]any) (dflow.Result, error) {
			return dflow.Result{Suspend: &dflow.JSONSuspension{Data: "saved"}}, nil
		},
		resume: func(_ context.Context, _ map[string]any, input dflow.ResumeInput) (dflow.Result, error) {
			return dflow.Result{Output: input.Response}, nil
		},
	})
	def := waitDefinition()
	def.Nodes = append([]dflow.NodeSpec{{ID: "before", Type: "passthrough"}}, def.Nodes...)
	def.Edges = append(def.Edges, edge("before", "wait"))
	graph, err := dflow.Compile(def, registry)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := store.Publish(ctx, "publication-1", graph)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedProvider{DefinitionProvider: provider, loads: map[string]int{}}
	e, err := dflow.NewEngineWithProvider(counted, registry, store, dflow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := e.Run(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if counted.loads["end"] != 0 || counted.loads["before"] != 1 || counted.loads["wait"] != 1 {
		t.Fatal("unready node configuration was loaded", counted.loads)
	}
	token := state.Nodes["wait"].Suspension.Token
	if _, err := e.Wakeup(ctx, "wait", token, "answer"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// New process needs ONLY the publication ID, no original graph or definition.
	store, err = sqlite.Open(ctx, path, "run")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	counted = &countedProvider{DefinitionProvider: store.Provider("publication-1"), loads: map[string]int{}}
	e, err = dflow.NewEngineWithProvider(counted, registry, store, dflow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, err = e.Drain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counted.loads["before"] != 0 || counted.loads["wait"] != 1 || counted.loads["end"] != 1 {
		t.Fatal("resume decoded irrelevant node definitions", counted.loads)
	}
	if _, exists := state.Nodes["before"]; exists {
		t.Fatal("driver loaded whole state")
	}
	if state.Nodes["end"].Status != dflow.Succeeded {
		t.Fatal(state)
	}
	if again, err := e.Drain(ctx); err != nil || len(again.Nodes) != 0 {
		t.Fatal("terminal graph was reloaded", again, err)
	}
}

func TestAtomicFanInAndDuplicateCommit(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		token, err := infra.Claim(ctx, "dflow/run", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := infra.Initialize(ctx, *token, dflow.Manifest{Hash: "v1", Roots: []string{"a", "b"}}, nil); err != nil {
			t.Fatal(err)
		}
		first, err := infra.ClaimReady(ctx, *token)
		if err != nil {
			t.Fatal(err)
		}
		second, err := infra.ClaimReady(ctx, *token)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil || second == nil || first.NodeID == second.NodeID {
			t.Fatal(first, second)
		}
		commits := make(chan error, 2)
		for _, task := range []*dflow.NodeTask{first, second} {
			go func() {
				commits <- infra.Commit(ctx, *token, dflow.NodeCommit{
					NodeID: task.NodeID, ExpectedVersion: task.State.Version,
					State:    dflow.NodeState{Status: dflow.Succeeded},
					Arrivals: []dflow.Arrival{{TargetNodeID: "join", Incoming: []string{"a", "b"}, Active: task.NodeID == "a"}},
				})
			}()
		}
		for range 2 {
			if err := <-commits; err != nil {
				t.Fatal(err)
			}
		}
		join, err := infra.GetNode(ctx, "join")
		if err != nil || !join.Ready || !join.Enabled || len(join.Incoming) != 2 {
			t.Fatal(join, err)
		}
		var wg sync.WaitGroup
		tasks := make(chan *dflow.NodeTask, 2)
		errs := make(chan error, 2)
		for range 2 {
			wg.Go(func() { task, err := infra.ClaimReady(ctx, *token); tasks <- task; errs <- err })
		}
		wg.Wait()
		close(tasks)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		claimed := 0
		for task := range tasks {
			if task != nil {
				claimed++
				if task.NodeID != "join" {
					t.Fatal(task)
				}
			}
		}
		if claimed != 1 {
			t.Fatal("join claimed multiple times", claimed)
		}
		err = infra.Commit(ctx, *token, dflow.NodeCommit{
			NodeID: first.NodeID, ExpectedVersion: first.State.Version, State: dflow.NodeState{Status: dflow.Succeeded},
			Arrivals: []dflow.Arrival{{TargetNodeID: "join", Incoming: []string{"a", "b"}, Active: true}},
		})
		if !errors.Is(err, dflow.ErrConflict) {
			t.Fatal("duplicate completion accepted", err)
		}
	})
}

func TestArrivalDedupAndInactiveJoin(t *testing.T) {
	arrival := dflow.Arrival{TargetNodeID: "target", Incoming: []string{"a", "b"}, Active: false}
	node, err := dflow.ApplyArrival(dflow.NodeState{}, "a", arrival)
	if err != nil {
		t.Fatal(err)
	}
	version := node.Version
	node, err = dflow.ApplyArrival(node, "a", arrival)
	if err != nil || node.Version != version || node.Ready {
		t.Fatal("duplicate arrival counted twice", node, err)
	}
	node, err = dflow.ApplyArrival(node, "b", arrival)
	if err != nil || !node.Ready || node.Enabled {
		t.Fatal("all inactive predecessors must skip", node, err)
	}
	arrival.Active = true
	if _, err := dflow.ApplyArrival(node, "a", arrival); !errors.Is(err, dflow.ErrConflict) {
		t.Fatal("arrival overwritten", err)
	}
}

func TestParallelPortsFromOnePredecessor(t *testing.T) {
	registry := dflow.DefaultRegistry()
	register(t, registry, "select", func(context.Context, map[string]any) (dflow.Result, error) {
		return dflow.Result{Output: "ok", Ports: []string{"true"}}, nil
	})
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{{ID: "a", Type: "select"}, {ID: "b", Type: "end"}},
		Edges: []dflow.Edge{
			{SourceNodeID: "a", TargetNodeID: "b", SourcePortID: "true"},
			{SourceNodeID: "a", TargetNodeID: "b", SourcePortID: "false"},
		},
	}
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		state, err := engine(t, def, registry, infra, dflow.Options{}).Run(context.Background(), nil)
		if err != nil || state.Nodes["b"].Status != dflow.Succeeded || len(state.Nodes["b"].Incoming) != 1 {
			t.Fatal(state, err)
		}
	})
}
