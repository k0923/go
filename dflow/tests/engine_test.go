package tests

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/lease"
)

type observedEngine struct {
	*dflow.Engine
	store dflow.SnapshotReader
}

func (e *observedEngine) Run(ctx context.Context, input map[string]any) (*dflow.State, error) {
	_, err := e.Engine.Run(ctx, input)
	state, readErr := e.store.Load(context.WithoutCancel(ctx))
	return state, errors.Join(err, readErr)
}
func (e *observedEngine) Drain(ctx context.Context) (*dflow.State, error) {
	_, err := e.Engine.Drain(ctx)
	state, readErr := e.store.Load(context.WithoutCancel(ctx))
	return state, errors.Join(err, readErr)
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

func TestConcurrentFanoutWaitsForAllPredecessors(t *testing.T) {
	registry := testRegistry{}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	register(t, registry, "wait", func(ctx context.Context, input map[string]any) (dflow.Result, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return dflow.Result{}, ctx.Err()
		}
		return dflow.Result{Output: input}, nil
	})
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{
			{ID: "a", Node: namedNode("wait"), Inputs: dflow.Parameters{"value": dflow.Constant(1)}},
			{ID: "b", Node: namedNode("wait"), Inputs: dflow.Parameters{"value": dflow.Constant(2)}},
			{ID: "join", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"a": dflow.Ref("a", "value"), "b": dflow.Ref("b", "value")}},
		},
		Edges: []dflow.Edge{edge("a", "join"), edge("b", "join")},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e := engine(t, def, registry, memory.New(), dflow.Options{MaxConcurrency: 2})
	done := make(chan error, 1)
	go func() {
		state, err := e.Run(ctx, nil)
		if err == nil && (state.Nodes["join"].Output.(map[string]any)["a"] != 1 ||
			state.Nodes["join"].Output.(map[string]any)["b"] != 2) {
			err = errors.New("join has wrong inputs")
		}
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("fanout did not start concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunLeaseAndCancellation(t *testing.T) {
	registry := testRegistry{}
	started := make(chan struct{})
	var exited atomic.Bool
	register(t, registry, "wait", func(ctx context.Context, _ map[string]any) (dflow.Result, error) {
		close(started)
		<-ctx.Done()
		exited.Store(true)
		return dflow.Result{}, ctx.Err()
	})
	e := engine(t, dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "a", Node: namedNode("wait")}}}, registry, memory.New(), dflow.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.Run(ctx, nil); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("node not started")
	}
	if _, err := e.Run(context.Background(), nil); !errors.Is(err, lease.ErrConflict) {
		t.Fatalf("second driver acquired occupied run: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || !exited.Load() {
		t.Fatalf("cancellation did not drain node: %v", err)
	}
}

type failingSave struct {
	*memory.Infrastructure
	fail atomic.Bool
}

func (s *failingSave) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) error {
	if commit.NodeID == "a" && commit.State.Status == dflow.Succeeded && s.fail.CompareAndSwap(true, false) {
		return errors.New("storage unavailable")
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
