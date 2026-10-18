package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/sqlite"
)

type taskNode struct{}

func (taskNode) Run(context.Context, map[string]any) (dflow.Result, error) {
	fmt.Println("首次执行 task 节点")
	return dflow.Result{Suspend: &dflow.Suspension{
		State: map[string]any{"format": "mp4"}, Request: map[string]any{"action": "transcode"},
	}}, nil
}

func (taskNode) Resume(_ context.Context, _ map[string]any, input dflow.ResumeInput) (dflow.Result, error) {
	fmt.Println("恢复 task 节点，token:", input.Token)
	return dflow.Result{Output: map[string]any{"savedState": input.State, "response": input.Response}}, nil
}

func main() {
	path := flag.String("db", "", "SQLite file path (required)")
	scope := flag.String("run", "demo", "unique run ID")
	answer := flag.String("answer", "", "response to the existing wait")
	token := flag.String("token", "", "suspension token from the request notification")
	flag.Parse()
	if *path == "" {
		log.Fatal("-db is required")
	}
	ctx := context.Background()
	store, err := sqlite.Open(ctx, *path, *scope)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	registry := dflow.DefaultRegistry()
	if err := registry.Register("async-task", func(json.RawMessage) (dflow.Node, error) {
		return taskNode{}, nil
	}); err != nil {
		log.Fatal(err)
	}
	graph, err := dflow.Compile(dflow.Definition{
		Version: "1",
		Nodes: []dflow.NodeSpec{
			{ID: "task", Type: "async-task"},
			{ID: "end", Type: "end", Inputs: dflow.Parameters{"result": dflow.Ref("task")}},
		},
		Edges: []dflow.Edge{{SourceNodeID: "task", TargetNodeID: "end"}},
	}, registry)
	if err != nil {
		log.Fatal(err)
	}
	engine, err := dflow.NewEngine(graph, store, dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	if *answer != "" {
		if *token == "" {
			log.Fatal("-token from the original request is required with -answer")
		}
		accepted, err := engine.Wakeup(ctx, "task", *token, *answer)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("写入唤醒结果:", accepted)
	}
	state, err := engine.Run(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("task 节点状态:", state.Nodes["task"].Status)
	// This application's own completion rule; the engine has no workflow status.
	if state.Nodes["end"].Status == dflow.Succeeded {
		data, _ := json.Marshal(state.Nodes["end"].Output)
		fmt.Println(string(data))
	}
	messages, err := store.Pending(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, message := range messages {
		// Production: REQUEST goes to a business provider using run+node+token as an
		// idempotency key; WAKE retries Resume until it returns without error.
		fmt.Printf("通知 %d: %s node=%s token=%s request=%s\n", message.ID, message.Kind, message.NodeID, message.Token, message.Request)
		if state.Nodes["end"].Status == dflow.Succeeded {
			if err := store.Ack(ctx, message.ID); err != nil {
				log.Fatal(err)
			}
		}
	}
}
