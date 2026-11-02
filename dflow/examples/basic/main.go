package main

import (
	"context"
	"fmt"
	"log"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/memory"
)

func main() {
	code, err := nodes.NewCodeNode(nodes.CodeConfig{
		Script: nodes.Script{Content: `function main({params}) { return {total: params.price * 2}; }`},
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	records, err := nodes.Compile(ctx, nodes.Definition{
		Nodes: []nodes.Spec{
			{ID: "start", Node: &nodes.StartNode{}, Inputs: map[string]nodes.Param{"input": nodes.Input()}},
			{ID: "calculate", Node: code, Inputs: map[string]nodes.Param{"price": nodes.Ref("start", "price")}},
			{ID: "end", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"total": nodes.Ref("calculate", "total")}},
		},
		Edges: []nodes.Edge{
			{Source: "start", Target: "calculate"},
			{Source: "calculate", Target: "end"},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	store, publication, err := memory.NewRun(records, "start")
	if err != nil {
		log.Fatal(err)
	}
	engine, err := dflow.NewEngine(publication, store, dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	state, err := driver.Run(ctx, engine, "start", nodes.RuntimeWithStore(map[string]any{"price": 21}, store))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(state.Nodes["end"].Output)
}
