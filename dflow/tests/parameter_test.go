package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/memory"
)

type doubledParameter struct{ Path []string }

func (p doubledParameter) References() ([][]string, error) {
	return [][]string{p.Path}, nil
}
func (p doubledParameter) Resolve(ctx context.Context, resolver dflow.ValueResolver) (any, error) {
	value, err := resolver.Resolve(ctx, p.Path...)
	if err != nil {
		return nil, err
	}
	return value.(int64) * 2, nil
}

func TestCustomParameterAndConcreteGoValues(t *testing.T) {
	type order struct{ Amount int64 }
	value := &order{Amount: 21}
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	graph, err := dflow.Compile(dflow.Definition{
		Version: "typed-v1",
		Nodes: []dflow.NodeSpec{
			{ID: "source", Node: dflow.NodeFunc(func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Output: value}, nil
			})},
			{ID: "out", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{
				"amount": doubledParameter{Path: []string{"source", "Amount"}},
				"bytes":  dflow.Constant([]byte{1, 2}),
				"cycle":  dflow.Constant(cyclic),
			}},
		},
		Edges: []dflow.Edge{edge("source", "out")},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := memory.New()
	engine, err := dflow.NewEngine(graph, store, dflow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := driver.Run(context.Background(), engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	output := state.Nodes["out"].Output.(map[string]any)
	if output["amount"] != int64(42) || output["bytes"].([]byte)[0] != 1 {
		t.Fatal(output)
	}
	value.Amount = 100
	output["bytes"].([]byte)[0] = 99
	output["cycle"].(map[string]any)["changed"] = true
	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	saved := snapshot.Nodes["out"].Output.(map[string]any)
	if snapshot.Nodes["source"].Output.(*order).Amount != 21 || saved["bytes"].([]byte)[0] != 1 {
		t.Fatal("typed snapshot aliases caller values")
	}
	cycle := saved["cycle"].(map[string]any)
	cycle["marker"] = true
	if cycle["self"].(map[string]any)["marker"] != true {
		t.Fatal("cyclic reference was lost")
	}
}

func TestParameterDependencyValidation(t *testing.T) {
	for _, parameter := range []dflow.Parameter{
		nil,
		(*dflow.ReferenceParameter)(nil),
		doubledParameter{Path: nil},
		doubledParameter{Path: []string{"sibling"}},
		dflow.TemplateParameter{Text: "{{source..amount}}"},
	} {
		_, err := dflow.Compile(dflow.Definition{
			Version: "v1",
			Nodes: []dflow.NodeSpec{
				{ID: "source", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"bad": parameter}},
			},
		})
		if !errors.Is(err, dflow.ErrInvalidReference) {
			t.Fatal("invalid custom dependency accepted", err)
		}
	}
}

func TestImmutableParameterRenderingAndInputPaths(t *testing.T) {
	graph, err := dflow.Compile(dflow.Definition{
		Version: "v1",
		Nodes: []dflow.NodeSpec{{ID: "out", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{
			"text": dflow.PlainTemplate("hello {{$input.name}}"),
			"json": dflow.Template(`{"name": {{$input.name}}}`),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := dflow.NewEngine(graph, memory.New(), dflow.Options{})
	state, err := driver.Run(context.Background(), engine, map[string]any{"name": "{{unknown.secret}}"})
	if err != nil {
		t.Fatal(err)
	}
	output := state.Nodes["out"].Output.(map[string]any)
	if output["text"] != "hello {{unknown.secret}}" || output["json"] != `{"name": "{{unknown.secret}}"}` {
		t.Fatal("rendered user content was interpreted twice", output)
	}
	var nilValue *struct{ Name string }
	if _, err := dflow.InputValue(nilValue, "Name"); !errors.Is(err, dflow.ErrMissingValue) {
		t.Fatal(err)
	}
	if value, err := dflow.InputValue(map[string]*int{"null": nil}, "null"); err != nil || value.(*int) != nil {
		t.Fatal("typed nil was not preserved", value, err)
	}
}
