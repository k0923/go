package tests

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/childflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/dflow/sqlite"
)

func compile(t *testing.T, def dflow.Definition, registry testRegistry) *dflow.Graph {
	t.Helper()
	graph, err := dflow.Compile(fixtureDefinition(def, registry))
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func childRequests(t *testing.T, store dflow.Outbox) []dflow.Notification {
	t.Helper()
	messages, err := store.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var requests []dflow.Notification
	for _, msg := range messages {
		if msg.Kind == dflow.RequestNotification {
			requests = append(requests, msg)
		}
	}
	return requests
}

func TestSubflowSQLiteRestartAndDuplicateDelivery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "children.db")
	store, err := sqlite.Open(ctx, path, "parent", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	registry := testRegistry{}
	var calls atomic.Int32
	register(t, registry, "echo", func(_ context.Context, input map[string]any) (dflow.Result, error) {
		calls.Add(1)
		return dflow.Result{Output: input}, nil
	})
	child := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{
		{ID: "out", Node: namedNode("echo"), Inputs: dflow.Parameters{"text": dflow.Input("text")}},
	}}, registry)
	parentGraph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{
		{ID: "call", Node: &childflow.SubflowNode{Publication: "child-v1", OutputNode: "out"},
			Inputs: dflow.Parameters{"text": dflow.Input("text")}},
	}}, registry)
	if _, err := store.Publish(ctx, "child-v1", child); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, "parent-v1", parentGraph); err != nil {
		t.Fatal(err)
	}
	parent, _ := dflow.NewEngineWithProvider(store.Provider("parent-v1"), store, dflow.Options{})
	if _, err := driver.Run(ctx, parent, map[string]any{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	request := childRequests(t, store)[0]
	var opened []*sqlite.Infrastructure
	dispatcher := childflow.ChildDispatcher{Bind: func(ctx context.Context, call childflow.ChildExecution) (*dflow.Engine, dflow.Infrastructure, dflow.DefinitionProvider, error) {
		childStore, err := sqlite.Open(ctx, path, call.ScopeKey(), fixtureCodec{})
		if err != nil {
			return nil, nil, nil, err
		}
		opened = append(opened, childStore)
		provider := childStore.Provider(call.Request.Publication)
		engine, err := dflow.NewEngineWithProvider(provider, childStore, dflow.Options{})
		return engine, childStore, provider, err
	}}
	if complete, err := dispatcher.Handle(ctx, parent, store, "parent", request); err != nil || !complete {
		t.Fatal(complete, err)
	}
	for _, childStore := range opened {
		if err := childStore.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, path, "parent", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, _ = dflow.NewEngineWithProvider(store.Provider("parent-v1"), store, dflow.Options{})
	// Callback committed but parent not resumed; request re-delivery must not restart child.
	if complete, err := dispatcher.Handle(ctx, parent, store, "parent", request); err != nil || !complete || calls.Load() != 1 {
		t.Fatal(complete, err, calls.Load())
	}
	if _, err := driver.Drain(ctx, parent); err != nil {
		t.Fatal(err)
	}
	node, _ := store.GetNode(ctx, "call")
	if node.Status != dflow.Succeeded || node.Output.(map[string]any)["text"] != "hello" {
		t.Fatal(node)
	}
	if complete, err := dispatcher.Handle(ctx, parent, store, "parent", request); err != nil || !complete {
		t.Fatal("late request", complete, err)
	}
}

func TestChildSuspensionAndError(t *testing.T) {
	for _, businessError := range []bool{false, true} {
		registry := testRegistry{}
		childStore := memory.New()
		parentStore := memory.New()
		var childGraph *dflow.Graph
		if businessError {
			register(t, registry, "fail", func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{}, errors.New("child business failure")
			})
			childGraph = compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "out", Node: namedNode("fail")}}}, registry)
		} else {
			registry = asyncRegistry(t, asyncNode{
				run: func(context.Context, map[string]any) (dflow.Result, error) {
					return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "external"}}, nil
				},
				resume: func(_ context.Context, _ map[string]any, input dflow.ResumeInput) (dflow.Result, error) {
					return dflow.Result{Output: input.Response}, nil
				},
			})
			childGraph = compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "out", Node: namedNode("custom")}}}, registry)
		}
		parentGraph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{
			{ID: "call", Node: &childflow.SubflowNode{Publication: "child", OutputNode: "out"}},
		}}, registry)
		parent, _ := dflow.NewEngine(parentGraph, parentStore, dflow.Options{})
		child, _ := dflow.NewEngine(childGraph, childStore, dflow.Options{})
		_, _ = driver.Run(context.Background(), parent, nil)
		request := childRequests(t, parentStore)[0]
		dispatcher := childflow.ChildDispatcher{Bind: func(context.Context, childflow.ChildExecution) (*dflow.Engine, dflow.Infrastructure, dflow.DefinitionProvider, error) {
			return child, childStore, childGraph, nil
		}}
		complete, err := dispatcher.Handle(context.Background(), parent, parentStore, "parent", request)
		if err != nil {
			t.Fatal(err)
		}
		if !businessError {
			if complete {
				t.Fatal("quiescent child incorrectly considered complete")
			}
			node, _ := childStore.GetNode(context.Background(), "out")
			if _, err := child.Wakeup(context.Background(), "out", node.Suspension.Token, "answer"); err != nil {
				t.Fatal(err)
			}
			complete, err = dispatcher.Handle(context.Background(), parent, parentStore, "parent", request)
			if err != nil || !complete {
				t.Fatal(complete, err)
			}
		} else if !complete {
			t.Fatal("durable child failure not returned")
		}
		_, err = driver.Drain(context.Background(), parent)
		node, _ := parentStore.GetNode(context.Background(), "call")
		if businessError {
			if err == nil || node.Status != dflow.Failed {
				t.Fatal(node, err)
			}
		} else if err != nil || node.Output != "answer" {
			t.Fatal(node, err)
		}
	}
}

func TestLoopReusesChildProtocol(t *testing.T) {
	for _, mode := range []string{"foreach", "until"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			registry := testRegistry{}
			register(t, registry, "body", func(_ context.Context, input map[string]any) (dflow.Result, error) {
				if mode == "foreach" {
					return dflow.Result{Output: input["item"]}, nil
				}
				next := input["value"].(int) + 1
				return dflow.Result{Output: map[string]any{"done": next == 3, "value": next}}, nil
			})
			body := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{
				{ID: "out", Node: namedNode("body"), Inputs: dflow.Parameters{"item": dflow.Input("item"), "value": dflow.Input("value")}},
			}}, registry)
			config, _ := json.Marshal(childflow.LoopNode{Publication: "body", OutputNode: "out", Mode: mode, MaxIterations: 3})
			parentGraph := compile(t, dflow.Definition{Nodes: []dflow.NodeSpec{
				{ID: "loop", Node: configuredLoop(config), Inputs: dflow.Parameters{
					"items": dflow.Constant([]any{"a", "b", "c"}), "value": dflow.Constant(0),
				}},
			}}, registry)
			parentStore := memory.New()
			children := map[string]*memory.Infrastructure{}
			dispatcher := childflow.ChildDispatcher{Bind: func(_ context.Context, call childflow.ChildExecution) (*dflow.Engine, dflow.Infrastructure, dflow.DefinitionProvider, error) {
				store := children[call.ScopeKey()]
				if store == nil {
					store = memory.New()
					children[call.ScopeKey()] = store
				}
				child, err := dflow.NewEngine(body, store, dflow.Options{})
				return child, store, body, err
			}}
			parent, _ := dflow.NewEngine(parentGraph, parentStore, dflow.Options{})
			_, _ = driver.Run(ctx, parent, nil)
			for iteration := 0; iteration < 3; iteration++ {
				requests := childRequests(t, parentStore)
				request := requests[len(requests)-1]
				if complete, err := dispatcher.Handle(ctx, parent, parentStore, "parent", request); err != nil || !complete {
					t.Fatal(complete, err)
				}
				if complete, err := dispatcher.Handle(ctx, parent, parentStore, "parent", request); err != nil || !complete {
					t.Fatal("duplicate", complete, err)
				}
				// Recreate the parent engine each iteration: no local loop state survives.
				parent, _ = dflow.NewEngine(parentGraph, parentStore, dflow.Options{})
				if _, err := driver.Drain(ctx, parent); err != nil {
					t.Fatal(err)
				}
			}
			node, _ := parentStore.GetNode(ctx, "loop")
			if node.Status != dflow.Succeeded || len(children) != 3 {
				t.Fatal(node, len(children))
			}
			if mode == "foreach" {
				result := node.Output.([]any)
				if len(result) != 3 || result[0] != "a" || result[2] != "c" {
					t.Fatal(result)
				}
			} else if node.Output != 3 {
				t.Fatal(node.Output)
			}
		})
	}
}

func TestLoopEmptyAndLimit(t *testing.T) {
	node := &childflow.LoopNode{Publication: "body", OutputNode: "out", MaxIterations: 1}
	result, err := node.Run(context.Background(), map[string]any{"items": []any{}})
	if err != nil || result.Suspend != nil || len(result.Output.([]any)) != 0 {
		t.Fatal(result, err)
	}
	if _, err := node.Run(context.Background(), map[string]any{"items": []any{1, 2}}); err == nil {
		t.Fatal("limit ignored")
	}
	node.Mode = "until"
	result, err = node.Run(context.Background(), map[string]any{"value": 0})
	if err != nil {
		t.Fatal(err)
	}
	_, err = node.Resume(context.Background(), map[string]any{"value": 0}, dflow.ResumeInput{
		Suspension: result.Suspend, Response: childflow.ChildResponse{Output: map[string]any{"done": false, "value": 1}},
	})
	if err == nil {
		t.Fatal("until iteration cap ignored")
	}
}
