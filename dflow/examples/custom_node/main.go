package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
)

// A business dependency belongs in a node, not in the framework's Infrastructure.
type greetingService struct{}

func (greetingService) Greet(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "你好，" + name, nil
}

type greetingNode struct {
	Suffix  string `json:"suffix"`
	service greetingService
}

func (n *greetingNode) Run(ctx context.Context, input map[string]any) (dflow.Result, error) {
	name, ok := input["name"].(string)
	if !ok {
		return dflow.Result{}, errors.New("name must be a string")
	}
	text, err := n.service.Greet(ctx, name)
	return dflow.Result{Output: map[string]any{"message": text + n.Suffix}}, err
}

func main() {
	service := greetingService{}
	graph, err := dflow.Compile(dflow.Definition{
		Version: "1",
		Nodes: []dflow.NodeSpec{
			{ID: "greeting", Node: &greetingNode{Suffix: "！", service: service},
				Inputs: dflow.Parameters{"name": dflow.Input("name")}},
			{ID: "end", Node: &dflow.EndNode{}, Inputs: dflow.Parameters{"text": dflow.Ref("greeting", "message")}},
		},
		Edges: []dflow.Edge{{SourceNodeID: "greeting", TargetNodeID: "end"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	engine, err := dflow.NewEngine(graph, memory.New(), dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	state, err := engine.Run(context.Background(), map[string]any{"name": "DAG"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(state.Nodes["end"].Output.(map[string]any)["text"])
}
