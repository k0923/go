package tests

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/memory"
)

// recordingHooks captures lifecycle events for assertions. It is safe for the
// concurrent OnFinish calls the engine makes from node goroutines.
//
// recordingHooks 捕获生命周期事件用于断言。它可以安全处理引擎从节点 goroutine 中
// 并发发起的 OnFinish 调用。
type recordingHooks struct {
	mu      sync.Mutex
	claims  []dflow.NodeEvent
	starts  []dflow.NodeEvent
	finishs []dflow.NodeEvent
}

func (h *recordingHooks) hooks() dflow.Hooks {
	return dflow.Hooks{
		OnClaim: func(_ context.Context, e dflow.NodeEvent) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.claims = append(h.claims, e)
		},
		OnStart: func(_ context.Context, e dflow.NodeEvent) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.starts = append(h.starts, e)
		},
		OnFinish: func(_ context.Context, e dflow.NodeEvent) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.finishs = append(h.finishs, e)
		},
	}
}

func (h *recordingHooks) outcome(nodeID string) (dflow.Status, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.finishs {
		if e.NodeID == nodeID {
			return e.Outcome, true
		}
	}
	return "", false
}

func TestHooksObserveNodeLifecycle(t *testing.T) {
	registry := testRegistry{}
	register(t, registry, "value", func(context.Context, map[string]any) (dflow.Result, error) {
		return dflow.Result{Output: map[string]any{"value": 7}}, nil
	})
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{
			{ID: "a", Node: namedNode("value")},
			{ID: "b", Node: &nodes.EndNode{}, Inputs: dflow.Parameters{"v": dflow.Ref("a")}},
		},
		Edges: []dflow.Edge{edge("a", "b")},
	}
	rec := &recordingHooks{}
	e := engine(t, def, registry, memory.New(), dflow.Options{Hooks: rec.hooks()})
	if _, err := e.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.claims) != 2 || len(rec.starts) != 2 || len(rec.finishs) != 2 {
		t.Fatalf("expected 2 of each event, got claims=%d starts=%d finishs=%d", len(rec.claims), len(rec.starts), len(rec.finishs))
	}
	for _, e := range rec.finishs {
		if e.Outcome != dflow.Succeeded {
			t.Fatalf("node %q outcome = %q, want succeeded", e.NodeID, e.Outcome)
		}
		if e.Duration < 0 {
			t.Fatalf("node %q negative duration %v", e.NodeID, e.Duration)
		}
	}
}

func TestHooksReportFailureOutcome(t *testing.T) {
	registry := testRegistry{}
	boom := errors.New("boom")
	register(t, registry, "fail", func(context.Context, map[string]any) (dflow.Result, error) {
		return dflow.Result{}, boom
	})
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{{ID: "a", Node: namedNode("fail")}},
	}
	rec := &recordingHooks{}
	e := engine(t, def, registry, memory.New(), dflow.Options{Hooks: rec.hooks()})
	if _, err := e.Run(context.Background(), nil); !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	outcome, ok := rec.outcome("a")
	if !ok || outcome != dflow.Failed {
		t.Fatalf("expected failed finish outcome, got %q (found=%v)", outcome, ok)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if last := rec.finishs[len(rec.finishs)-1]; !errors.Is(last.Err, boom) {
		t.Fatalf("expected finish event to carry boom, got %v", last.Err)
	}
}

// A panicking hook must never corrupt execution.
//
// 发生 panic 的 hook 绝不能破坏执行。
func TestHooksPanicIsContained(t *testing.T) {
	registry := testRegistry{}
	register(t, registry, "value", func(context.Context, map[string]any) (dflow.Result, error) {
		return dflow.Result{Output: map[string]any{"value": 1}}, nil
	})
	def := dflow.Definition{Nodes: []dflow.NodeSpec{{ID: "a", Node: namedNode("value")}}}
	hooks := dflow.Hooks{OnStart: func(context.Context, dflow.NodeEvent) { panic("hook blew up") }}
	e := engine(t, def, registry, memory.New(), dflow.Options{Hooks: hooks})
	state, err := e.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("panicking hook leaked into execution: %v", err)
	}
	if state.Nodes["a"].Status != dflow.Succeeded {
		t.Fatalf("node did not succeed despite contained hook panic: %+v", state.Nodes["a"])
	}
}
