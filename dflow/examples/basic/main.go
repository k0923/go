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
	graph, err := dflow.Compile(dflow.Definition{
		Version: "basic-v1",
		Nodes: []dflow.NodeSpec{
			{ID: "start", Node: &nodes.StartNode{}, Inputs: dflow.Parameters{"input": dflow.Input()}},
			{ID: "calculate", Node: code, Inputs: dflow.Parameters{"price": dflow.Ref("start", "price")}},
			{ID: "end", Node: &nodes.EndNode{}, Inputs: dflow.Parameters{"total": dflow.Ref("calculate", "total")}},
		},
		Edges: []dflow.Edge{
			{SourceNodeID: "start", TargetNodeID: "calculate"},
			{SourceNodeID: "calculate", TargetNodeID: "end"},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	engine, err := dflow.NewEngine(graph, memory.New(), dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	state, err := driver.Run(context.Background(), engine, map[string]any{"price": 21})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(state.Nodes["end"].Output)
}
