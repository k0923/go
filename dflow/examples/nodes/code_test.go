package nodes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/memory"
)

func TestCodeNode(t *testing.T) {
	for _, script := range []string{
		`function main({params}) { return {value: params.x * 2}; }`,
		`const main = ({params}) => ({value: params.x * 2});`,
		`async function main({params}) { await sleep(1); return {value: params.x * 2}; }`,
		`function run(params) { return {value: params.x * 2}; }`,
	} {
		t.Run(script, func(t *testing.T) {
			node, err := nodes.NewCodeNode(nodes.CodeConfig{Script: nodes.Script{Content: script}})
			if err != nil {
				t.Fatal(err)
			}
			def := dflow.Definition{
				Version: "code-v1",
				Nodes: []dflow.NodeSpec{
					{ID: "js", Node: node, Inputs: dflow.Parameters{"x": dflow.Input("number")}},
					{ID: "end", Node: &nodes.EndNode{}, Inputs: dflow.Parameters{"result": dflow.Ref("js", "value")}},
				},
				Edges: []dflow.Edge{{SourceNodeID: "js", TargetNodeID: "end"}},
			}
			graph, err := dflow.Compile(def)
			if err != nil {
				t.Fatal(err)
			}
			store := memory.New()
			engine, err := dflow.NewEngine(graph, store, dflow.Options{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = driver.Run(context.Background(), engine, map[string]any{"number": 21})
			if err != nil {
				t.Fatal(err)
			}
			state, err := store.Load(context.Background())
			if err != nil || state.Nodes["end"].Output["result"] != float64(42) {
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
			//
			// Compile 绝不能执行顶层无限循环。
			node, err := nodes.NewCodeNode(nodes.CodeConfig{Script: nodes.Script{Content: script}, TimeoutMS: 20})
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
		`function main() { return null; }`,
		`const notMain = 1;`,
	} {
		node, err := nodes.NewCodeNode(nodes.CodeConfig{Script: nodes.Script{Content: script}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := node.Run(context.Background(), nil); err == nil {
			t.Fatalf("expected script failure for %s", script)
		}
	}
	node, err := nodes.NewCodeNode(nodes.CodeConfig{Script: nodes.Script{Content: `function main() { while (true) {} }`}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := node.Run(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled context, got %v", err)
	}
}
