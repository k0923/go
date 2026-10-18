package main

import (
	"context"
	"encoding/json"
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

func main() {
	service := greetingService{}
	registry := dflow.DefaultRegistry()
	err := registry.Register("greet", func(raw json.RawMessage) (dflow.Node, error) {
		var config struct {
			Suffix string `json:"suffix"`
		}
		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, err
		}
		return dflow.NodeFunc(func(ctx context.Context, input map[string]any) (dflow.Result, error) {
			name, ok := input["name"].(string)
			if !ok {
				return dflow.Result{}, errors.New("name must be a string")
			}
			text, err := service.Greet(ctx, name)
			return dflow.Result{Output: map[string]any{"message": text + config.Suffix}}, err
		}), nil
	})
	if err != nil {
		log.Fatal(err)
	}
	graph, err := dflow.Compile(dflow.Definition{
		Version: "1",
		Nodes: []dflow.NodeSpec{
			{ID: "greeting", Type: "greet", Config: json.RawMessage(`{"suffix":"！"}`),
				Inputs: dflow.Parameters{"name": dflow.Input("name")}},
			{ID: "end", Type: "end", Inputs: dflow.Parameters{"text": dflow.Ref("greeting", "message")}},
		},
		Edges: []dflow.Edge{{SourceNodeID: "greeting", TargetNodeID: "end"}},
	}, registry)
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
