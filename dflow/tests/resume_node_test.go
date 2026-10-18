package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/lease"
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
				return dflow.Result{Output: resume.Response}, nil
			},
		})
		graph := compile(t, dflow.Definition{
			Nodes: []dflow.NodeSpec{
				{ID: "a", Node: namedNode("custom"), Inputs: dflow.Parameters{"saved": dflow.Input("value")}},
				{ID: "b", Node: namedNode("custom"), Inputs: dflow.Parameters{"saved": dflow.Input("value")}},
				{ID: "after", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{
					"text": dflow.PlainTemplate(`{{$input.value}} {{a}}`),
				}},
			},
			Edges: []dflow.Edge{edge("a", "after")},
		}, registry)
		e, err := dflow.NewEngine(graph, counter, dflow.Options{MaxConcurrency: 1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.Run(ctx, map[string]any{"value": "original"})
		if err != nil {
			t.Fatal(err)
		}
		a, _ := infra.GetNode(ctx, "a")
		b, _ := infra.GetNode(ctx, "b")
		if _, err := e.Wakeup(ctx, "b", b.Suspension.Token, "other"); err != nil {
			t.Fatal(err)
		}
		counter.reads = 0
		state, err := e.ResumeNode(ctx, "a", a.Suspension.Token, "answer")
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Nodes) != 2 || state.Nodes["a"].Status != dflow.Succeeded ||
			state.Nodes["after"].Output.(map[string]any)["text"] != "original answer" {
			t.Fatal(state)
		}
		b, _ = infra.GetNode(ctx, "b")
		if b.Status != dflow.Suspended || !b.Ready {
			t.Fatal("unrelated callback was executed", b)
		}
		if counter.reads != 1 {
			t.Fatal("resume read original input unnecessarily", counter.reads)
		}
		if state, err := e.ResumeNode(ctx, "a", a.Suspension.Token); err != nil || len(state.Nodes) != 0 {
			t.Fatal("duplicate resume executed work", state, err)
		}
		counter.reads = 0
		state, err = e.ResumeNode(ctx, "b", b.Suspension.Token)
		if err != nil || state.Nodes["b"].Output != "other" || counter.reads != 0 {
			t.Fatal("persisted wake needs no input read", state, err, counter.reads)
		}
	})
}

func TestGetInputMissingNullAndArray(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		ctx := context.Background()
		token, err := infra.Claim(ctx, "dflow/run", 30_000_000_000)
		if err != nil {
			t.Fatal(err)
		}
		defer infra.Release(ctx, *token)
		err = infra.Initialize(ctx, *token, dflow.Manifest{Hash: "input"},
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
		initial, err := e.Run(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		old := initial.Nodes["wait"].Suspension.Token
		if _, err := e.ResumeNode(ctx, "wait", old, nil); err != nil {
			t.Fatal(err)
		}
		current, _ := infra.GetNode(ctx, "wait")
		if current.Suspension.Token == old {
			t.Fatal("token did not change")
		}
		if state, err := e.ResumeNode(ctx, "wait", old, "late"); err != nil || len(state.Nodes) != 0 {
			t.Fatal(state, err)
		}
		// A occupied execution lease is still enforced for targeted resume.
		token, err := infra.Claim(ctx, "dflow/run", 30_000_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.ResumeNode(ctx, "wait", current.Suspension.Token); !errors.Is(err, lease.ErrConflict) {
			t.Fatal(err)
		}
		_ = infra.Release(ctx, *token)
	})
}
