package tests

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
	"github.com/k0923/go/lease"
)

func TestCodeNode(t *testing.T) {
	for _, script := range []string{
		`function main({params}) { return {value: params.x * 2}; }`,
		`const main = ({params}) => ({value: params.x * 2});`,
		`async function main({params}) { await sleep(1); return {value: params.x * 2}; }`,
		`function run(params) { return {value: params.x * 2}; }`,
	} {
		t.Run(script, func(t *testing.T) {
			config, _ := json.Marshal(dflow.CodeConfig{Script: dflow.Script{Content: script}})
			def := dflow.Definition{
				Nodes: []dflow.NodeSpec{
					{ID: "js", Node: configuredFixture("code", config), Inputs: dflow.Parameters{"x": dflow.Input("number")}},
					{ID: "end", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"result": dflow.Ref("js", "value")}},
				},
				Edges: []dflow.Edge{edge("js", "end")},
			}
			state, err := engine(t, def, nil, memory.New(), dflow.Options{}).Run(context.Background(), map[string]any{"number": 21})
			if err != nil || state.Nodes["end"].Output.(map[string]any)["result"] != float64(42) {
				t.Fatalf("unexpected JS result: %+v %v", state, err)
			}
		})
	}
}

func TestCodeTimeoutIncludesTopLevelAndSerialization(t *testing.T) {
	for _, script := range []string{
		`while (true) {} function main() { return 1; }`,
		`function main() { while (true) {} }`,
		`async function main() { await sleep(999999999); return 1; }`,
		`function main() { return {toJSON() { while (true) {} }}; }`,
	} {
		t.Run(script, func(t *testing.T) {
			// Compile must never execute the top-level infinite loop.
			node, err := dflow.NewCodeNode(dflow.CodeConfig{Script: dflow.Script{Content: script}, TimeoutMS: 20})
			if err != nil {
				t.Fatal(err)
			}
			_, err = node.Run(context.Background(), nil)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected deadline, got %v", err)
			}
		})
	}
}

func TestCodeRejectsInvalidResultsAndSupportsCancellation(t *testing.T) {
	for _, script := range []string{
		`function main() { throw new Error("oops"); }`,
		`async function main() { throw new Error("oops"); }`,
		`function main() { return new Promise(() => {}); }`,
		`function main() { const x = {}; x.x = x; return x; }`,
		`function main() { return undefined; }`,
		`const notMain = 1;`,
	} {
		node, err := dflow.NewCodeNode(dflow.CodeConfig{Script: dflow.Script{Content: script}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := node.Run(context.Background(), nil); err == nil {
			t.Fatalf("expected script failure for %s", script)
		}
	}
	node, err := dflow.NewCodeNode(dflow.CodeConfig{Script: dflow.Script{Content: `function main() { while (true) {} }`}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := node.Run(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled context, got %v", err)
	}
}

func TestMemoryFencingAndVersionCAS(t *testing.T) {
	ctx := context.Background()
	infra := memory.New()
	first, err := infra.Claim(ctx, "dflow/run", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := infra.Initialize(ctx, *first, dflow.Manifest{Hash: "v1", Roots: []string{"node"}}, nil); err != nil {
		t.Fatal(err)
	}
	task, err := infra.ClaimReady(ctx, *first)
	if err != nil {
		t.Fatal(err)
	}
	if err := infra.Release(ctx, *first); err != nil {
		t.Fatal(err)
	}
	second, err := infra.Claim(ctx, "dflow/run", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	commit := dflow.NodeCommit{NodeID: "node", ExpectedVersion: task.State.Version, State: dflow.NodeState{Status: dflow.Succeeded}}
	if err := infra.Commit(ctx, *first, commit); !errors.Is(err, lease.ErrConflict) {
		t.Fatalf("stale owner accepted: %v", err)
	}
	if second.Generation <= first.Generation {
		t.Fatal("lease generation did not advance")
	}
	if err := infra.Commit(ctx, *second, commit); !errors.Is(err, dflow.ErrConflict) {
		t.Fatal("old node generation accepted", err)
	}
	task, err = infra.ClaimReady(ctx, *second)
	if err != nil {
		t.Fatal(err)
	}
	commit.ExpectedVersion = task.State.Version
	if err := infra.Commit(ctx, *second, commit); err != nil {
		t.Fatal(err)
	}
	if err := infra.Commit(ctx, *second, commit); !errors.Is(err, dflow.ErrConflict) {
		t.Fatal("duplicate terminal commit accepted", err)
	}
	loaded, err := infra.Load(ctx)
	if err != nil || loaded.Nodes["node"].Version != task.State.Version+1 {
		t.Fatalf("incorrect snapshot version: %+v %v", loaded, err)
	}
	loaded.Nodes["node"] = dflow.NodeState{Status: dflow.Failed}
	again, err := infra.Load(ctx)
	if err != nil || again.Nodes["node"].Status != dflow.Succeeded {
		t.Fatalf("load aliases internal state: %+v %v", again, err)
	}
}
