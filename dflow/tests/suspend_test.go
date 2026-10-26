package tests

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/dflow/sqlite"
	"github.com/k0923/go/lease"
)

type asyncNode struct {
	run    dflow.NodeFunc
	resume func(context.Context, map[string]any, dflow.ResumeInput) (dflow.Result, error)
}

func (asyncNode) Type() string { return "custom" }

func (n asyncNode) Run(ctx context.Context, input map[string]any) (dflow.Result, error) {
	return n.run(ctx, input)
}
func (n asyncNode) Resume(ctx context.Context, input map[string]any, resume dflow.ResumeInput) (dflow.Result, error) {
	return n.resume(ctx, input, resume)
}

func asyncRegistry(t *testing.T, node asyncNode) testRegistry {
	t.Helper()
	return testRegistry{"custom": &node}
}

func waitDefinition() dflow.Definition {
	return dflow.Definition{
		Nodes: []dflow.NodeSpec{
			{ID: "start", Node: &dflow.StartNode{}},
			{ID: "wait", Node: namedNode("custom"), Inputs: dflow.Parameters{"original": dflow.Constant("input")}},
			{ID: "end", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"value": dflow.Ref("wait", "value")}},
		},
		Edges: []dflow.Edge{edge("start", "wait"), edge("wait", "end")},
	}
}

func backends(t *testing.T, test func(*testing.T, dflow.Infrastructure)) {
	t.Helper()
	for _, name := range []string{"memory", "sqlite"} {
		t.Run(name, func(t *testing.T) {
			var infra dflow.Infrastructure = memory.New()
			if name == "sqlite" {
				store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "run.db"), "run", fixtureCodec{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { store.Close() })
				infra = store
			}
			test(t, infra)
		})
	}
}

func TestNodeSuspensionAndResume(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		var runs, resumes atomic.Int32
		node := asyncNode{
			run: func(_ context.Context, input map[string]any) (dflow.Result, error) {
				runs.Add(1)
				input["original"] = "private mutation"
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external",
					Data: map[string]any{"job": "job-123", "action": "submit"},
				}}, nil
			},
			resume: func(_ context.Context, input map[string]any, resume dflow.ResumeInput) (dflow.Result, error) {
				resumes.Add(1)
				if input["original"] != "input" || resume.Suspension.(*dflow.ValueSuspension).Data.(map[string]any)["job"] != "job-123" || resume.Token == "" {
					return dflow.Result{}, errors.New("missing saved input or continuation state")
				}
				return dflow.Result{Output: map[string]any{"value": resume.Response}}, nil
			},
		}
		registry := asyncRegistry(t, node)
		e := engine(t, waitDefinition(), registry, infra, dflow.Options{})
		ctx := context.Background()
		state, err := e.Run(ctx, nil)
		if err != nil || state.Nodes["wait"].Status != dflow.Suspended {
			t.Fatalf("not suspended: %+v %v", state, err)
		}
		token := state.Nodes["wait"].Suspension.Token
		outbox := infra.(testOutbox)
		requests, err := outbox.Pending(ctx)
		if err != nil || len(requests) != 1 || requests[0].Token != token || requests[0].Request.Type() != "external" {
			t.Fatal(requests, err)
		}
		if _, err := e.Drain(ctx); err != nil || runs.Load() != 1 || resumes.Load() != 0 {
			t.Fatal("unresolved suspension executed node", err)
		}
		ok, err := e.Wakeup(ctx, "wait", token, nil)
		if err != nil || !ok {
			t.Fatal("null callback was not accepted", ok, err)
		}
		if ok, err := e.Wakeup(ctx, "wait", token, "duplicate"); err != nil || ok {
			t.Fatal("duplicate callback accepted", ok, err)
		}
		if err := outbox.Ack(ctx, requests[0].ID); err != nil {
			t.Fatal(err)
		}
		messages, err := outbox.Pending(ctx)
		if err != nil || len(messages) != 1 || messages[0].Kind != dflow.WakeNotification {
			t.Fatal("request ack consumed wake", messages, err)
		}
		// A fresh engine must call Resume, not re-enter Run.
		//
		// 新 engine 必须调用 Resume，而不是重新进入 Run。
		e = engine(t, waitDefinition(), registry, infra, dflow.Options{})
		state, err = e.Drain(ctx)
		if err != nil || state.Nodes["end"].Status != dflow.Succeeded || state.Nodes["wait"].Output["value"] != nil ||
			state.Nodes["wait"].Suspension != nil || runs.Load() != 1 || resumes.Load() != 1 {
			t.Fatal(state, err, runs.Load(), resumes.Load())
		}
		if _, err := e.Drain(ctx); err != nil || runs.Load() != 1 || resumes.Load() != 1 {
			t.Fatal("terminal node reran", err)
		}
		// Framework state intentionally exposes no aggregate workflow lifecycle.
		//
		// 框架状态刻意不暴露聚合的工作流生命周期。
		if _, exists := reflect.TypeFor[dflow.State]().FieldByName("Status"); exists {
			t.Fatal("workflow status must be external")
		}
	})
}

func TestRepeatedSuspensionAndResumeFailure(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		var runs, resumes atomic.Int32
		node := asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				runs.Add(1)
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "first"}}, nil
			},
			resume: func(_ context.Context, _ map[string]any, resume dflow.ResumeInput) (dflow.Result, error) {
				count := resumes.Add(1)
				if resume.Suspension.(*dflow.ValueSuspension).Data == "first" {
					return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "second"}}, nil
				}
				if count == 2 {
					return dflow.Result{}, errors.New("temporary resume error")
				}
				return dflow.Result{Output: map[string]any{"value": resume.Response}}, nil
			},
		}
		e := engine(t, waitDefinition(), asyncRegistry(t, node), infra, dflow.Options{})
		ctx := context.Background()
		state, err := e.Run(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		first := state.Nodes["wait"].Suspension.Token
		if _, err := e.Wakeup(ctx, "wait", first, "one"); err != nil {
			t.Fatal(err)
		}
		state, err = e.Drain(ctx)
		if err != nil || state.Nodes["wait"].Status != dflow.Suspended {
			t.Fatal(state, err)
		}
		second := state.Nodes["wait"].Suspension.Token
		if first == second {
			t.Fatal("new suspension reused old token")
		}
		if accepted, err := e.Wakeup(ctx, "wait", first, "late"); err != nil || accepted {
			t.Fatal("stale callback accepted", err)
		}
		if _, err := e.Wakeup(ctx, "wait", second, "two"); err != nil {
			t.Fatal(err)
		}
		state, err = e.Drain(ctx)
		if err == nil || state.Nodes["wait"].Status != dflow.Failed || state.Nodes["wait"].Suspension.Token != second {
			t.Fatal("failed Resume lost continuation", state, err)
		}
		state, err = e.Drain(ctx)
		if err != nil || state.Nodes["wait"].Output["value"] != "two" || runs.Load() != 1 || resumes.Load() != 3 {
			t.Fatal(state, err, runs.Load(), resumes.Load())
		}
	})
}

type earlyWake struct{ *memory.Infrastructure }

func (s *earlyWake) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) ([]string, error) {
	ready, err := s.Infrastructure.Commit(ctx, token, commit)
	if err != nil {
		return nil, err
	}
	if commit.NodeID == "wait" && commit.State.Status == dflow.Suspended {
		_, err := s.Resolve(ctx, "wait", commit.State.Suspension.Token, "early")
		return ready, err
	}
	return ready, nil
}

func TestEarlyResponseAndIndependentBranch(t *testing.T) {
	node := asyncNode{
		run: func(context.Context, map[string]any) (dflow.Result, error) {
			return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "submit"}}, nil
		},
		resume: func(_ context.Context, _ map[string]any, input dflow.ResumeInput) (dflow.Result, error) {
			return dflow.Result{Output: map[string]any{"value": input.Response}}, nil
		},
	}
	def := waitDefinition()
	def.Nodes = append(def.Nodes, dflow.NodeSpec{ID: "independent", Node: &dflow.PassthroughNode{}})
	def.Edges = append(def.Edges, edge("start", "independent"))
	for _, early := range []bool{false, true} {
		var infra dflow.Infrastructure = memory.New()
		if early {
			infra = &earlyWake{infra.(*memory.Infrastructure)}
		}
		state, err := engine(t, def, asyncRegistry(t, node), infra, dflow.Options{}).Run(context.Background(), nil)
		if err != nil || state.Nodes["independent"].Status != dflow.Succeeded {
			t.Fatal("suspension occupied execution slot", state, err)
		}
		if early && (state.Nodes["wait"].Output["value"] != "early" || state.Nodes["end"].Status != dflow.Succeeded) {
			t.Fatal("early response was lost", state)
		}
		if !early && state.Nodes["wait"].Status != dflow.Suspended {
			t.Fatal(state)
		}
	}
}

func TestInvalidSuspension(t *testing.T) {
	for _, resumable := range []bool{false, true} {
		registry := testRegistry{}
		run := dflow.NodeFunc(func(context.Context, map[string]any) (dflow.Result, error) {
			return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external"}}, nil
		})
		if resumable {
			registry = asyncRegistry(t, asyncNode{
				run: func(ctx context.Context, input map[string]any) (dflow.Result, error) {
					result, _ := run(ctx, input)
					result.Output = map[string]any{"value": "ambiguous outcome"}
					return result, nil
				},
			})
		} else {
			register(t, registry, "custom", run)
		}
		state, err := engine(t, waitDefinition(), registry, memory.New(), dflow.Options{}).Run(context.Background(), nil)
		if !errors.Is(err, dflow.ErrInvalidSuspension) || state.Nodes["wait"].Status != dflow.Failed {
			t.Fatal(state, err)
		}
	}
}
