package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/examples/nodes"
)

type inputCounter struct {
	dflow.Infrastructure
	reads int
}

func (s *inputCounter) GetInput(ctx context.Context, path ...string) (any, error) {
	s.reads++
	return s.Infrastructure.GetInput(ctx, path...)
}

func TestResumeNodeTargetsOneBranchAndReadsInputOnDemand(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		counter := &inputCounter{Infrastructure: infra}
		registry := asyncRegistry(t, asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "wait"}}, nil
			},
			resume: func(_ context.Context, input map[string]any, resume dflow.ResumeInput) (dflow.Result, error) {
				if input["saved"] != "original" {
					return dflow.Result{}, errors.New("saved node input missing")
				}
				return dflow.Result{Output: map[string]any{"value": resume.Response}}, nil
			},
		})
		graph := compile(t, dflow.Definition{
			Nodes: []dflow.NodeSpec{
				{ID: "start", Node: &nodes.StartNode{}, Inputs: dflow.Parameters{"input": dflow.Input()}},
				{ID: "a", Node: namedNode("custom"), Inputs: dflow.Parameters{"saved": dflow.Input("value")}},
				{ID: "b", Node: namedNode("custom"), Inputs: dflow.Parameters{"saved": dflow.Input("value")}},
				{ID: "after", Node: &nodes.EndNode{}, Inputs: dflow.Parameters{
					"text": dflow.PlainTemplate(`{{$input.value}} {{a.value}}`),
				}},
			},
			Edges: []dflow.Edge{edge("start", "a"), edge("start", "b"), edge("a", "after")},
		}, registry)
		e, err := dflow.NewEngine(graph, counter, dflow.Options{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = driver.Run(ctx, e, map[string]any{"value": "original"})
		if err != nil {
			t.Fatal(err)
		}
		a, _ := infra.GetNode(ctx, "a")
		b, _ := infra.GetNode(ctx, "b")
		if _, err := e.Wakeup(ctx, "b", b.Suspension.Token, "other"); err != nil {
			t.Fatal(err)
		}
		counter.reads = 0
		state, err := driver.Resume(ctx, e, "a", a.Suspension.Token, "answer")
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Nodes) != 2 || state.Nodes["a"].Status != dflow.Succeeded ||
			state.Nodes["after"].Output["text"] != "original answer" {
			t.Fatal(state)
		}
		b, _ = infra.GetNode(ctx, "b")
		if b.Status != dflow.Suspended || !b.Ready {
			t.Fatal("unrelated callback was executed", b)
		}
		if counter.reads != 1 {
			t.Fatal("resume read original input unnecessarily", counter.reads)
		}
		if state, err := driver.Resume(ctx, e, "a", a.Suspension.Token); err != nil || len(state.Nodes) != 0 {
			t.Fatal("duplicate resume executed work", state, err)
		}
		counter.reads = 0
		state, err = driver.Resume(ctx, e, "b", b.Suspension.Token)
		if err != nil || state.Nodes["b"].Output["value"] != "other" || counter.reads != 0 {
			t.Fatal("persisted wake needs no input read", state, err, counter.reads)
		}
	})
}

func TestGetInputMissingNullAndArray(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		err := infra.Initialize(ctx, dflow.Manifest{Hash: "input", Start: "start"},
			map[string]any{"items": []any{map[string]any{"value": "ok"}}, "null": nil})
		if err != nil {
			t.Fatal(err)
		}
		if value, err := infra.GetInput(ctx, "items", "0", "value"); err != nil || value != "ok" {
			t.Fatal(value, err)
		}
		if value, err := infra.GetInput(ctx, "null"); err != nil || value != nil {
			t.Fatal(value, err)
		}
		if _, err := infra.GetInput(ctx, "missing"); !errors.Is(err, dflow.ErrMissingValue) {
			t.Fatal(err)
		}
		if _, err := infra.GetInput(ctx, "items", "99"); !errors.Is(err, dflow.ErrMissingValue) {
			t.Fatal(err)
		}
		value, err := infra.GetInput(ctx)
		if err != nil {
			t.Fatal(err)
		}
		value.(map[string]any)["null"] = "mutated"
		if value, err := infra.GetInput(ctx, "null"); err != nil || value != nil {
			t.Fatal("input aliases storage", value, err)
		}
	})
}

func TestResumeNodeDoesNotAcceptOldLoopToken(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		registry := asyncRegistry(t, asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "first"}}, nil
			},
			resume: func(context.Context, map[string]any, dflow.ResumeInput) (dflow.Result, error) {
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "second"}}, nil
			},
		})
		e, err := dflow.NewEngine(compile(t, waitDefinition(), registry), infra, dflow.Options{})
		if err != nil {
			t.Fatal(err)
		}
		initial, err := driver.Run(ctx, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		old := initial.Nodes["wait"].Suspension.Token
		if _, err := driver.Resume(ctx, e, "wait", old, nil); err != nil {
			t.Fatal(err)
		}
		current, _ := infra.GetNode(ctx, "wait")
		if current.Suspension.Token == old {
			t.Fatal("token did not change")
		}
		if state, err := driver.Resume(ctx, e, "wait", old, "late"); err != nil || len(state.Nodes) != 0 {
			t.Fatal(state, err)
		}
		// A live node lease held by another instance blocks the targeted resume:
		// it drives nothing instead of executing Resume twice.
		//
		// 由另一个实例持有的有效节点租约会阻塞定向 resume：它不会驱动任何内容，
		// 也不会执行两次 Resume。
		if accepted, err := e.Wakeup(ctx, "wait", current.Suspension.Token, "answer"); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		task, err := infra.ClaimNode(ctx, "wait", 30*time.Second)
		if err != nil || task == nil {
			t.Fatal(task, err)
		}
		if state, err := driver.Resume(ctx, e, "wait", current.Suspension.Token); err != nil || len(state.Nodes) != 0 {
			t.Fatal(state, err)
		}
		if err := infra.ReleaseNode(ctx, task.Lease); err != nil {
			t.Fatal(err)
		}
	})
}
