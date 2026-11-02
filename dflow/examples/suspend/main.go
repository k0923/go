package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/sqlite"
)

type taskNode struct{}

func (taskNode) RunInput(_ context.Context, input map[string]any) (dflow.NodeResult, error) {
	answer, _ := input["answer"].(string)
	if answer == "" {
		fmt.Println("task 节点挂起，等待业务回调")
		return dflow.AsyncResult{Result: dflow.Result{
			Output: map[string]any{"request": "transcode mp4"},
		}}, nil
	}
	fmt.Println("task 节点收到业务回调:", answer)
	return dflow.AsyncResult{Done: true, Result: dflow.Result{
		Output: map[string]any{"request": "transcode mp4", "response": answer},
	}}, nil
}

func compile(ctx context.Context) ([]dflow.PublishedNode[*nodes.Runtime], error) {
	return nodes.Compile(ctx, nodes.Definition{
		Nodes: []nodes.Spec{
			{ID: "task", Node: taskNode{}, Inputs: map[string]nodes.Param{"answer": nodes.Input("answer")}},
			{ID: "end", Node: &nodes.EndNode{}, Inputs: map[string]nodes.Param{"result": nodes.Ref("task")}},
		},
		Edges: []nodes.Edge{{Source: "task", Target: "end"}},
	})
}

func main() {
	path := flag.String("db", "", "SQLite file path (required)")
	scope := flag.String("run", "demo", "unique run ID")
	initRun := flag.Bool("init", false, "initialize a new run before driving")
	answer := flag.String("answer", "", "business response used to continue the suspended task")
	flag.Parse()
	if *path == "" {
		log.Fatal("-db is required")
	}
	ctx := context.Background()
	records, err := compile(ctx)
	if err != nil {
		log.Fatal(err)
	}
	var store *sqlite.Infrastructure
	var publication *sqlite.Publication[*nodes.Runtime]
	if *initRun {
		store, publication, err = sqlite.NewRun(ctx, *path, *scope, records, "task")
	} else {
		store, err = sqlite.Open(ctx, *path, *scope)
		if err == nil {
			publication, err = sqlite.Publish(records)
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	engine, err := dflow.NewEngine(publication, store, dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	state, err := driver.Run(ctx, engine, "task", nodes.RuntimeWithStore(map[string]any{"answer": *answer}, store))
	if err != nil {
		log.Fatal(err)
	}
	for id, node := range state.Nodes {
		fmt.Printf("%s: %s\n", id, node.Status)
	}
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if snapshot["end"].Status == dflow.Succeeded {
		data, _ := json.Marshal(snapshot["end"].Output)
		fmt.Println(string(data))
	}
}
