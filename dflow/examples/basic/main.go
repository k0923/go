package main

import (
	"context"
	"fmt"
	"log"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
)

func main() {
	code, err := dflow.NewCodeNode(dflow.CodeConfig{
		Script: dflow.Script{Content: `function main({params}) { return {total: params.price * 2}; }`},
	})
	if err != nil {
		log.Fatal(err)
	}
	graph, err := dflow.Compile(dflow.Definition{
		Version: "basic-v1",
		Nodes: []dflow.NodeSpec{
			{ID: "start", Node: &dflow.StartNode{}},
			{ID: "calculate", Node: code, Inputs: dflow.Parameters{"price": dflow.Ref("start", "price")}},
			{ID: "end", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"total": dflow.Ref("calculate", "total")}},
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
	state, err := engine.Run(context.Background(), map[string]any{"price": 21})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(state.Nodes["end"].Output)
}
