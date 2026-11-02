package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/memory"
)

// A business dependency belongs in a node, not in the framework's Infrastructure.
//
// 业务依赖应属于节点，而不是框架的 Infrastructure。
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

func (n *greetingNode) RunInput(ctx context.Context, input map[string]any) (dflow.NodeResult, error) {
	name, ok := input["name"].(string)
	if !ok {
		return dflow.Result{}, errors.New("name must be a string")
	}
	text, err := n.service.Greet(ctx, name)
	return dflow.Result{Output: map[string]any{"message": text + n.Suffix}}, err
}

func main() {
	ctx := context.Background()
	service := greetingService{}
	records, err := nodes.Compile(ctx, nodes.Definition{
		Nodes: []nodes.Spec{
			{ID: "greeting", Node: &greetingNode{Suffix: "！", service: service},
				Inputs: map[string]nodes.Param{"name": nodes.Input("name")}},
			{ID: "end", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"text": nodes.Ref("greeting", "message")}},
		},
		Edges: []nodes.Edge{{Source: "greeting", Target: "end"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	store, publication, err := memory.NewRun(records, "greeting")
	if err != nil {
		log.Fatal(err)
	}
	engine, err := dflow.NewEngine(publication, store, dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	state, err := driver.Run(ctx, engine, "greeting", nodes.RuntimeWithStore(map[string]any{"name": "DAG"}, store))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(state.Nodes["end"].Output["text"])
}
