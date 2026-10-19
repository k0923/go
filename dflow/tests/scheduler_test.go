package tests

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/sqlite"
	"github.com/k0923/go/lease"
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
	store, err := sqlite.Open(ctx, path, "run", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	registry := asyncRegistry(t, asyncNode{
		run: func(context.Context, map[string]any) (dflow.Result, error) {
			return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "saved"}}, nil
		},
		resume: func(_ context.Context, _ map[string]any, input dflow.ResumeInput) (dflow.Result, error) {
			return dflow.Result{Output: input.Response}, nil
		},
	})
	def := waitDefinition()
	def.Nodes = append([]dflow.NodeSpec{{ID: "before", Node: &dflow.PassthroughNode{}}}, def.Nodes...)
	def.Edges = append(def.Edges, edge("before", "wait"))
	graph, err := dflow.Compile(fixtureDefinition(def, registry))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := store.Publish(ctx, "publication-1", graph)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedProvider{DefinitionProvider: provider, loads: map[string]int{}}
	e, err := dflow.NewEngineWithProvider(counted, store, dflow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := driver.Run(ctx, e, nil)
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
	store, err = sqlite.Open(ctx, path, "run", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	counted = &countedProvider{DefinitionProvider: store.Provider("publication-1"), loads: map[string]int{}}
	e, err = dflow.NewEngineWithProvider(counted, store, dflow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, err = driver.Drain(ctx, e)
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
	if again, err := driver.Drain(ctx, e); err != nil || len(again.Nodes) != 0 {
		t.Fatal("terminal graph was reloaded", again, err)
	}
}

func TestAtomicFanInAndDuplicateCommit(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		if err := infra.Initialize(ctx, dflow.Manifest{Hash: "v1", Roots: []string{"a", "b"}}, nil); err != nil {
			t.Fatal(err)
		}
		first, err := infra.ClaimNode(ctx, "a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		second, err := infra.ClaimNode(ctx, "b", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil || second == nil || first.NodeID == second.NodeID {
			t.Fatal(first, second)
		}
		commits := make(chan []string, 2)
		errs := make(chan error, 2)
		for _, task := range []*dflow.NodeTask{first, second} {
			go func() {
				ready, err := infra.Commit(ctx, task.Lease, dflow.NodeCommit{
					NodeID: task.NodeID, ExpectedVersion: task.State.Version,
					State:    dflow.NodeState{Status: dflow.Succeeded},
					Arrivals: []dflow.Arrival{{TargetNodeID: "join", Incoming: []string{"a", "b"}, Active: task.NodeID == "a"}},
				})
				commits <- ready
				errs <- err
			}()
		}
		reported := 0
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			reported += len(<-commits)
		}
		if reported != 1 {
			t.Fatal("join must be reported ready exactly once", reported)
		}
		join, err := infra.GetNode(ctx, "join")
		if err != nil || !join.Ready || !join.Enabled || len(join.Incoming) != 2 {
			t.Fatal(join, err)
		}
		var wg sync.WaitGroup
		tasks := make(chan *dflow.NodeTask, 2)
		claimErrs := make(chan error, 2)
		for range 2 {
			wg.Go(func() { task, err := infra.ClaimNode(ctx, "join", time.Minute); tasks <- task; claimErrs <- err })
		}
		wg.Wait()
		close(tasks)
		close(claimErrs)
		for err := range claimErrs {
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
		_, err = infra.Commit(ctx, first.Lease, dflow.NodeCommit{
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
	registry := testRegistry{}
	register(t, registry, "select", func(context.Context, map[string]any) (dflow.Result, error) {
		return dflow.Result{Output: "ok", Ports: []string{"true"}}, nil
	})
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{{ID: "a", Node: namedNode("select")}, {ID: "b", Node: &dflow.EndNode{}}},
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
