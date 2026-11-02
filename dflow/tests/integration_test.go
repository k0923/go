package tests

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/dflow/sqlite"
)

type runStore interface {
	dflow.Infrastructure
	driver.RecoveryReader
	nodes.OutputReader
	Snapshot(context.Context) (map[string]dflow.NodeState, error)
}

type storeCase struct {
	name   string
	create func(*testing.T, []dflow.PublishedNode[*nodes.Runtime], string) (runStore, dflow.DefinitionProvider[*nodes.Runtime])
}

func stores() []storeCase {
	return []storeCase{
		{
			name: "memory",
			create: func(t *testing.T, records []dflow.PublishedNode[*nodes.Runtime], root string) (runStore, dflow.DefinitionProvider[*nodes.Runtime]) {
				t.Helper()
				store, publication, err := memory.NewRun(records, root)
				if err != nil {
					t.Fatal(err)
				}
				return store, publication
			},
		},
		{
			name: "sqlite",
			create: func(t *testing.T, records []dflow.PublishedNode[*nodes.Runtime], root string) (runStore, dflow.DefinitionProvider[*nodes.Runtime]) {
				t.Helper()
				store, publication, err := sqlite.NewRun(context.Background(), filepath.Join(t.TempDir(), "run.db"), t.Name(), records, root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { store.Close() })
				return store, publication
			},
		},
	}
}

func compile(t *testing.T, definition nodes.Definition) []dflow.PublishedNode[*nodes.Runtime] {
	t.Helper()
	records, err := nodes.Compile(context.Background(), definition)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func newEngine(t *testing.T, provider dflow.DefinitionProvider[*nodes.Runtime], store dflow.Infrastructure) *dflow.Engine[*nodes.Runtime] {
	t.Helper()
	engine, err := dflow.NewEngine(provider, store, dflow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestMemoryAndSQLiteRunFanOutJoin(t *testing.T) {
	definition := nodes.Definition{
		Nodes: []nodes.Spec{
			{ID: "start", Node: &nodes.StartNode{}, Inputs: map[string]nodes.Param{"input": nodes.Input()}},
			{ID: "left", Node: namedNode("left"), Inputs: map[string]nodes.Param{"value": nodes.Ref("start", "value")}},
			{ID: "right", Node: namedNode("right"), Inputs: map[string]nodes.Param{"value": nodes.Ref("start", "value")}},
			{ID: "join", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{
				"left": nodes.Ref("left", "value"), "right": nodes.Ref("right", "value"),
			}},
		},
		Edges: []nodes.Edge{
			{Source: "start", Target: "left"},
			{Source: "start", Target: "right"},
			{Source: "left", Target: "join"},
			{Source: "right", Target: "join"},
		},
	}
	records := compile(t, definition)
	for _, tc := range stores() {
		t.Run(tc.name, func(t *testing.T) {
			store, provider := tc.create(t, records, "start")
			engine := newEngine(t, provider, store)
			_, err := driver.Run(context.Background(), engine, "start", nodes.RuntimeWithStore(map[string]any{"value": "root"}, store))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if snapshot["join"].Status != dflow.Succeeded ||
				snapshot["join"].Output["left"] != "left:root" ||
				snapshot["join"].Output["right"] != "right:root" {
				t.Fatalf("unexpected join state: %+v", snapshot["join"])
			}
		})
	}
}

func TestDrainRecoversReadySuccessor(t *testing.T) {
	records := compile(t, nodes.Definition{
		Nodes: []nodes.Spec{
			{ID: "start", Node: &nodes.StartNode{}, Inputs: map[string]nodes.Param{"input": nodes.Input()}},
			{ID: "end", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"value": nodes.Ref("start", "value")}},
		},
		Edges: []nodes.Edge{{Source: "start", Target: "end"}},
	})
	for _, tc := range stores() {
		t.Run(tc.name, func(t *testing.T) {
			store, provider := tc.create(t, records, "start")
			engine := newEngine(t, provider, store)
			step, err := engine.Drive(context.Background(), "start", nodes.RuntimeWithStore(map[string]any{"value": 42}, store))
			if err != nil || len(step.Ready) != 1 || step.Ready[0] != "end" {
				t.Fatalf("start did not ready end: %+v %v", step, err)
			}
			state, err := driver.Drain(context.Background(), engine, store, nodes.RuntimeWithStore(nil, store))
			if err != nil {
				t.Fatal(err)
			}
			if state.Nodes["end"].Status != dflow.Succeeded || state.Nodes["end"].Output["value"] != 42 {
				t.Fatalf("drain did not recover end: %+v", state.Nodes["end"])
			}
		})
	}
}

func TestSuspendedNodeRequiresExplicitDrive(t *testing.T) {
	records := compile(t, nodes.Definition{
		Nodes: []nodes.Spec{
			{ID: "task", Node: asyncNode{}, Inputs: map[string]nodes.Param{"answer": nodes.Input("answer")}},
			{ID: "end", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"answer": nodes.Ref("task", "answer")}},
		},
		Edges: []nodes.Edge{{Source: "task", Target: "end"}},
	})
	store, provider, err := memory.NewRun(records, "task")
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, provider, store)
	if _, err := driver.Run(context.Background(), engine, "task", nodes.RuntimeWithStore(map[string]any{"answer": ""}, store)); err != nil {
		t.Fatal(err)
	}
	drained, err := driver.Drain(context.Background(), engine, store, nodes.RuntimeWithStore(nil, store))
	if err != nil {
		t.Fatal(err)
	}
	if len(drained.Nodes) != 0 {
		t.Fatalf("suspended node was recovered automatically: %+v", drained.Nodes)
	}
	if _, err := driver.Run(context.Background(), engine, "task", nodes.RuntimeWithStore(map[string]any{"answer": "done"}, store)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot["task"].Status != dflow.Succeeded || snapshot["end"].Output["answer"] != "done" {
		t.Fatalf("async continuation did not finish: %+v", snapshot)
	}
}

func TestBranchDeselectionSkipsInactivePath(t *testing.T) {
	records := compile(t, nodes.Definition{
		Nodes: []nodes.Spec{
			{ID: "condition", Node: &nodes.ConditionNode{}, Inputs: map[string]nodes.Param{"condition": nodes.Input("flag")}},
			{ID: "true", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"matched": nodes.Ref("condition", "matchedKey")}},
			{ID: "false", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"matched": nodes.Ref("condition", "matchedKey")}},
		},
		Edges: []nodes.Edge{{Source: "condition", Target: "true"}, {Source: "condition", Target: "false"}},
	})
	store, provider, err := memory.NewRun(records, "condition")
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, provider, store)
	if _, err := driver.Run(context.Background(), engine, "condition", nodes.RuntimeWithStore(map[string]any{"flag": true}, store)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot["true"].Status != dflow.Succeeded || snapshot["false"].Status != dflow.Skipped {
		t.Fatalf("branch states changed: true=%+v false=%+v", snapshot["true"], snapshot["false"])
	}
}

type namedNode string

func (n namedNode) RunInput(_ context.Context, input map[string]any) (dflow.NodeResult, error) {
	return dflow.Result{Output: map[string]any{"value": fmt.Sprintf("%s:%v", n, input["value"])}}, nil
}

type asyncNode struct{}

func (asyncNode) RunInput(_ context.Context, input map[string]any) (dflow.NodeResult, error) {
	answer, _ := input["answer"].(string)
	if answer == "" {
		return dflow.AsyncResult{Result: dflow.Result{Output: map[string]any{"request": "answer"}}}, nil
	}
	return dflow.AsyncResult{Done: true, Result: dflow.Result{Output: map[string]any{"answer": answer}}}, nil
}
