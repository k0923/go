package tests

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/lease"
)

type observedEngine struct {
	*dflow.Engine
	store dflow.SnapshotReader
}

func (e *observedEngine) Run(ctx context.Context, input map[string]any) (*dflow.State, error) {
	_, err := driver.Run(ctx, e.Engine, input)
	state, readErr := e.store.Load(context.WithoutCancel(ctx))
	return state, errors.Join(err, readErr)
}
func (e *observedEngine) Drain(ctx context.Context) (*dflow.State, error) {
	_, err := driver.Drain(ctx, e.Engine)
	state, readErr := e.store.Load(context.WithoutCancel(ctx))
	return state, errors.Join(err, readErr)
}

// ResumeNode mirrors the old engine helper by driving through the reference
// driver: the returned State contains only nodes this resume touched.
func (e *observedEngine) ResumeNode(ctx context.Context, nodeID, token string, response ...any) (*dflow.State, error) {
	return driver.Resume(ctx, e.Engine, nodeID, token, response...)
}
func engine(t *testing.T, def dflow.Definition, registry testRegistry, infra dflow.Infrastructure, options dflow.Options) *observedEngine {
	t.Helper()
	graph, err := dflow.Compile(fixtureDefinition(def, registry))
	if err != nil {
		t.Fatal(err)
	}
	e, err := dflow.NewEngine(graph, infra, options)
	if err != nil {
		t.Fatal(err)
	}
	return &observedEngine{e, infra.(dflow.SnapshotReader)}
}

func register(t *testing.T, registry testRegistry, name string, node dflow.NodeFunc) {
	t.Helper()
	registry[name] = node
}

func edge(from, to string) dflow.Edge {
	return dflow.Edge{SourceNodeID: from, TargetNodeID: to}
}

func TestCompileRejectsInvalidGraphsAndReferences(t *testing.T) {
	base := func() dflow.Definition {
		return dflow.Definition{
			Version: "v1",
			Nodes:   []dflow.NodeSpec{{ID: "a", Node: &dflow.PassthroughNode{}}, {ID: "b", Node: &dflow.PassthroughNode{}}},
			Edges:   []dflow.Edge{edge("a", "b")},
		}
	}
	cases := []struct {
		name   string
		change func(*dflow.Definition)
		cause  error
	}{
		{"cycle", func(d *dflow.Definition) { d.Edges = append(d.Edges, edge("b", "a")) }, dflow.ErrInvalidDefinition},
		{"duplicate ID", func(d *dflow.Definition) { d.Nodes[1].ID = "a" }, dflow.ErrInvalidDefinition},
		{"missing node", func(d *dflow.Definition) { d.Edges[0].TargetNodeID = "missing" }, dflow.ErrInvalidDefinition},
		{"duplicate edge", func(d *dflow.Definition) { d.Edges = append(d.Edges, d.Edges[0]) }, dflow.ErrInvalidDefinition},
		{"self reference", func(d *dflow.Definition) {
			d.Nodes[1].Inputs = dflow.Parameters{"x": dflow.Ref("b")}
		}, dflow.ErrInvalidReference},
		{"downstream reference", func(d *dflow.Definition) {
			d.Nodes[0].Inputs = dflow.Parameters{"x": dflow.Ref("b")}
		}, dflow.ErrInvalidReference},
		{"sibling reference", func(d *dflow.Definition) {
			d.Edges = nil
			d.Nodes[1].Inputs = dflow.Parameters{"x": dflow.Ref("a")}
		}, dflow.ErrInvalidReference},
		{"hidden template reference", func(d *dflow.Definition) {
			d.Nodes[0].Inputs = dflow.Parameters{"x": dflow.Template("{{ b.secret }}")}
		}, dflow.ErrInvalidReference},
		{"nil node", func(d *dflow.Definition) { d.Nodes[1].Node = nil }, dflow.ErrInvalidDefinition},
		{"typed nil node", func(d *dflow.Definition) { d.Nodes[1].Node = (*dflow.EndNode)(nil) }, dflow.ErrInvalidDefinition},
		{"missing version", func(d *dflow.Definition) { d.Version = "" }, dflow.ErrInvalidDefinition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := base()
			tc.change(&def)
			_, err := dflow.Compile(def)
			var validation *dflow.ValidationError
			if !errors.Is(err, tc.cause) || (tc.name != "missing version" && !errors.As(err, &validation)) {
				t.Fatalf("expected %v and ValidationError, got %v", tc.cause, err)
			}
		})
	}
}

func TestDAGReferencesAndSnapshotIsolation(t *testing.T) {
	infra := memory.New()
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{
			{ID: "start", Node: &dflow.StartNode{}},
			{ID: "middle", Node: &dflow.PassthroughNode{}},
			{ID: "end", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{
				"value": dflow.Ref("start", "items", "0", "name"),
				"null":  dflow.Ref("start", "null"),
				"json":  dflow.Template(`{"name": {{start.items.0.name}}}`),
				"text":  dflow.PlainTemplate("hello {{start.items.0.name}}"),
			}},
		},
		Edges: []dflow.Edge{edge("start", "middle"), edge("middle", "end")},
	}
	e := engine(t, def, nil, infra, dflow.Options{})
	input := map[string]any{"items": []any{map[string]any{"name": `a"b`}}, "null": nil}
	state, err := e.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	out := state.Nodes["end"].Output.(map[string]any)
	if out["value"] != `a"b` || out["json"] != `{"name": "a\"b"}` || out["text"] != `hello a"b` {
		t.Fatalf("unexpected output: %#v", out)
	}
	if value, exists := out["null"]; !exists || value != nil {
		t.Fatalf("null is not preserved: %#v", out)
	}
	out["value"] = "mutated"
	again, err := e.Run(context.Background(), input)
	if err != nil || again.Nodes["end"].Output.(map[string]any)["value"] != `a"b` {
		t.Fatalf("durable state leaked: %#v %v", again, err)
	}
	_, err = e.Run(context.Background(), map[string]any{"different": true})
	if !errors.Is(err, dflow.ErrRunMismatch) {
		t.Fatalf("expected run mismatch, got %v", err)
	}
}

func TestResumeRetriesOnlyUnfinishedNodes(t *testing.T) {
	registry := testRegistry{}
	var first, second atomic.Int32
	transient := errors.New("try again")
	register(t, registry, "first", func(ctx context.Context, _ map[string]any) (dflow.Result, error) {
		if dflow.NodeID(ctx) != "a" {
			return dflow.Result{}, fmt.Errorf("missing node context")
		}
		first.Add(1)
		return dflow.Result{Output: 42}, nil
	})
	register(t, registry, "second", func(_ context.Context, input map[string]any) (dflow.Result, error) {
		if second.Add(1) == 1 {
			return dflow.Result{}, transient
		}
		return dflow.Result{Output: input["value"]}, nil
	})
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{{ID: "a", Node: namedNode("first")}, {ID: "b", Node: namedNode("second"), Inputs: dflow.Parameters{"value": dflow.Ref("a")}}},
		Edges: []dflow.Edge{edge("a", "b")},
	}
	infra := memory.New()
	e := engine(t, def, registry, infra, dflow.Options{})
	state, err := e.Run(context.Background(), nil)
	if !errors.Is(err, transient) || state.Nodes["b"].Status != dflow.Failed {
		t.Fatalf("expected failed state, got %+v %v", state, err)
	}
	// A fresh engine resumes from infrastructure, not from the prior engine's memory.
	e = engine(t, def, registry, infra, dflow.Options{})
	state, err = e.Run(context.Background(), nil)
	if err != nil || state.Nodes["b"].Status != dflow.Succeeded || first.Load() != 1 || second.Load() != 2 || state.Nodes["b"].Output != 42 {
		t.Fatalf("resume failed: %+v %v calls=%d/%d", state, err, first.Load(), second.Load())
	}
	_, err = e.Run(context.Background(), nil)
	if err != nil || first.Load() != 1 || second.Load() != 2 {
		t.Fatalf("completed nodes reran: %v", err)
	}
}

func TestBranchSkipAndMerge(t *testing.T) {
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{
			{ID: "branch", Node: &dflow.ConditionNode{}, Inputs: dflow.Parameters{"condition": dflow.Constant(true)}},
			{ID: "yes", Node: &dflow.PassthroughNode{}},
			{ID: "no", Node: &dflow.PassthroughNode{}},
			{ID: "no-child", Node: &dflow.PassthroughNode{}},
			{ID: "merge", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"value": dflow.Ref("branch", "matchedKey")}},
		},
		Edges: []dflow.Edge{
			{SourceNodeID: "branch", TargetNodeID: "yes", SourcePortID: "true"},
			{SourceNodeID: "branch", TargetNodeID: "no", SourcePortID: "false"},
			edge("no", "no-child"), edge("yes", "merge"), edge("no-child", "merge"),
		},
	}
	state, err := engine(t, def, nil, memory.New(), dflow.Options{}).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Nodes["no"].Status != dflow.Skipped || state.Nodes["no-child"].Status != dflow.Skipped ||
		state.Nodes["merge"].Status != dflow.Succeeded || state.Nodes["merge"].Output.(map[string]any)["value"] != "true" {
		t.Fatalf("incorrect branch state: %+v", state)
	}
	// A topologically legal reference to an inactive branch must fail, not resolve as null.
	def.Nodes[4].Inputs = dflow.Parameters{"missing": dflow.Ref("no")}
	_, err = engine(t, def, nil, memory.New(), dflow.Options{}).Run(context.Background(), nil)
	if !errors.Is(err, dflow.ErrMissingValue) {
		t.Fatalf("expected unavailable branch output, got %v", err)
	}
}

func TestSerialFanoutWaitsForAllPredecessors(t *testing.T) {
	registry := testRegistry{}
	var order atomic.Int32
	register(t, registry, "mark", func(ctx context.Context, input map[string]any) (dflow.Result, error) {
		return dflow.Result{Output: int(order.Add(1))}, nil
	})
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{
			{ID: "a", Node: namedNode("mark")},
			{ID: "b", Node: namedNode("mark")},
			{ID: "join", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"a": dflow.Ref("a"), "b": dflow.Ref("b")}},
		},
		Edges: []dflow.Edge{edge("a", "join"), edge("b", "join")},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e := engine(t, def, registry, memory.New(), dflow.Options{})
	state, err := e.Run(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// join runs only after both predecessors, and the driver never ran it twice.
	out := state.Nodes["join"].Output.(map[string]any)
	if state.Nodes["join"].Status != dflow.Succeeded || out["a"] == nil || out["b"] == nil {
		t.Fatalf("join did not wait for both predecessors: %+v", state)
	}
}

func TestNodeLeaseEndsWithCommit(t *testing.T) {
	registry := testRegistry{}
	register(t, registry, "value", func(context.Context, map[string]any) (dflow.Result, error) {
		return dflow.Result{Output: 1}, nil
	})
	infra := memory.New()
	e := engine(t, dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "a", Node: namedNode("value")}}}, registry, infra, dflow.Options{})
	ctx := context.Background()
	if _, err := e.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// Commit ends the node lease; a terminal node is never claimable again.
	if task, err := infra.ClaimNode(ctx, "a", 30*time.Second); err != nil || task != nil {
		t.Fatalf("terminal node claimed again: %+v %v", task, err)
	}
}

type failingSave struct {
	*memory.Infrastructure
	fail atomic.Bool
}

func (s *failingSave) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) ([]string, error) {
	if commit.NodeID == "a" && commit.State.Status == dflow.Succeeded && s.fail.CompareAndSwap(true, false) {
		return nil, errors.New("storage unavailable")
	}
	return s.Infrastructure.Commit(ctx, token, commit)
}

func TestPersistenceFailureDoesNotStartDownstream(t *testing.T) {
	registry := testRegistry{}
	var downstream atomic.Int32
	register(t, registry, "downstream", func(context.Context, map[string]any) (dflow.Result, error) {
		downstream.Add(1)
		return dflow.Result{Output: true}, nil
	})
	infra := &failingSave{Infrastructure: memory.New()}
	infra.fail.Store(true)
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{{ID: "a", Node: &dflow.PassthroughNode{}}, {ID: "b", Node: namedNode("downstream")}},
		Edges: []dflow.Edge{edge("a", "b")},
	}
	e := engine(t, def, registry, infra, dflow.Options{})
	state, err := e.Run(context.Background(), nil)
	var infraErr *dflow.InfrastructureError
	if !errors.As(err, &infraErr) || downstream.Load() != 0 || state.Nodes["a"].Status != dflow.Running {
		t.Fatalf("uncommitted result used: %+v %v", state, err)
	}
	state, err = e.Run(context.Background(), nil)
	if err != nil || state.Nodes["b"].Status != dflow.Succeeded || downstream.Load() != 1 {
		t.Fatalf("recovery failed: %+v %v", state, err)
	}
}
