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
			records, err := nodes.Compile(context.Background(), nodes.Definition{
				Nodes: []nodes.Spec{
					{ID: "js", Node: node, Inputs: map[string]nodes.Param{"x": nodes.Input("number")}},
					{ID: "end", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"result": nodes.Ref("js", "value")}},
				},
				Edges: []nodes.Edge{{Source: "js", Target: "end"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			store, publication, err := memory.NewRun(records, "js")
			if err != nil {
				t.Fatal(err)
			}
			engine, err := dflow.NewEngine(publication, store, dflow.Options{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = driver.Run(context.Background(), engine, "js", nodes.RuntimeWithStore(map[string]any{"number": 21}, store))
			if err != nil {
				t.Fatal(err)
			}
			state, err := store.Snapshot(context.Background())
			if err != nil || state["end"].Output["result"] != float64(42) {
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
			_, err = node.RunInput(context.Background(), nil)
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
		if _, err := node.RunInput(context.Background(), nil); err == nil {
			t.Fatalf("expected script failure for %s", script)
		}
	}
	node, err := nodes.NewCodeNode(nodes.CodeConfig{Script: nodes.Script{Content: `function main() { while (true) {} }`}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := node.RunInput(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled context, got %v", err)
	}
}
