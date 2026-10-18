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

func (taskNode) Type() string { return "async-task" }

func (taskNode) Run(context.Context, map[string]any) (dflow.Result, error) {
	fmt.Println("首次执行 task 节点")
	return dflow.Result{Suspend: &dflow.JSONSuspension{
		Data: map[string]any{"format": "mp4", "action": "transcode"},
	}}, nil
}

func (taskNode) Resume(_ context.Context, _ map[string]any, input dflow.ResumeInput) (dflow.Result, error) {
	fmt.Println("恢复 task 节点，token:", input.Token)
	payload, ok := input.Suspension.(*dflow.JSONSuspension)
	if !ok {
		return dflow.Result{}, dflow.ErrInvalidSuspension
	}
	return dflow.Result{Output: map[string]any{"operation": payload.Data, "response": input.Response}}, nil
}

func main() {
	path := flag.String("db", "", "SQLite file path (required)")
	scope := flag.String("run", "demo", "unique run ID")
	answer := flag.String("answer", "", "response to the existing wait")
	token := flag.String("token", "", "suspension token from the request notification")
	publish := flag.Bool("publish", false, "publish the definition before first execution")
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
	if err := registry.Register(func() dflow.Node {
		return &taskNode{}
	}); err != nil {
		log.Fatal(err)
	}
	if *publish {
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
		if _, err := store.Publish(ctx, "transcode-v1", graph); err != nil {
			log.Fatal(err)
		}
	}
	// On restart only the publication ID is needed; nodes are decoded on demand.
	engine, err := dflow.NewEngineWithProvider(store.Provider("transcode-v1"), registry, store, dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	if *answer != "" {
		if *token == "" {
			log.Fatal("-token from the original request is required with -answer")
		}
		_, err = engine.ResumeNode(ctx, "task", *token, *answer)
	} else {
		_, err = engine.Run(ctx, nil)
	}
	if err != nil {
		log.Fatal(err)
	}
	state, err := store.Load(ctx) // optional business observation, not engine scheduling
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
