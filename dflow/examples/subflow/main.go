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

func main() {
	path := flag.String("db", "", "SQLite path (required)")
	scope := flag.String("run", "loop-demo", "parent execution scope")
	publish := flag.Bool("publish", false, "publish definitions")
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
	if *publish {
		body, err := dflow.Compile(dflow.Definition{
			Version: "1",
			Nodes: []dflow.NodeSpec{{ID: "out", Type: "end", Inputs: dflow.Parameters{
				"item": dflow.Input("item"), "index": dflow.Input("index"),
			}}},
		}, registry)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := store.Publish(ctx, "loop-body-v1", body); err != nil {
			log.Fatal(err)
		}
		parent, err := dflow.Compile(dflow.Definition{
			Version: "1",
			Nodes: []dflow.NodeSpec{{ID: "loop", Type: "loop",
				Config: json.RawMessage(`{"publication":"loop-body-v1","outputNode":"out","mode":"foreach","maxIterations":10}`),
				Inputs: dflow.Parameters{"items": dflow.Input("items")},
			}},
		}, registry)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := store.Publish(ctx, "loop-parent-v1", parent); err != nil {
			log.Fatal(err)
		}
	}
	parent, err := dflow.NewEngineWithProvider(store.Provider("loop-parent-v1"), registry, store, dflow.Options{})
	if err != nil {
		log.Fatal(err)
	}
	if _, err := parent.Run(ctx, map[string]any{"items": []any{"alpha", "beta", "gamma"}}); err != nil {
		log.Fatal(err)
	}
	var handles []*sqlite.Infrastructure
	defer func() {
		for _, handle := range handles {
			_ = handle.Close()
		}
	}()
	dispatcher := dflow.ChildDispatcher{Bind: func(ctx context.Context, call dflow.ChildExecution) (*dflow.Engine, dflow.Infrastructure, error) {
		childStore, err := sqlite.Open(ctx, *path, call.ScopeKey())
		if err != nil {
			return nil, nil, err
		}
		handles = append(handles, childStore)
		child, err := dflow.NewEngineWithProvider(childStore.Provider(call.Request.Publication), registry, childStore, dflow.Options{})
		return child, childStore, err
	}}
	// This example's children are synchronous. Production dispatchers retain
	// blocked requests, resume child scopes on Wake, then retry parent requests.
	for {
		node, err := store.GetNode(ctx, "loop")
		if err != nil {
			log.Fatal(err)
		}
		if node != nil && node.Status == dflow.Succeeded {
			data, _ := json.Marshal(node.Output)
			fmt.Println(string(data))
			break
		}
		messages, err := store.Pending(ctx)
		if err != nil {
			log.Fatal(err)
		}
		progress := false
		for _, msg := range messages {
			switch msg.Kind {
			case dflow.RequestNotification:
				complete, err := dispatcher.Handle(ctx, parent, *scope, msg)
				if err != nil {
					log.Fatal(err)
				}
				if !complete {
					continue
				}
			case dflow.WakeNotification:
				if _, err := parent.ResumeNode(ctx, msg.NodeID, msg.Token); err != nil {
					log.Fatal(err)
				}
			default:
				continue
			}
			if err := store.Ack(ctx, msg.ID); err != nil {
				log.Fatal(err)
			}
			progress = true
		}
		if !progress {
			log.Fatal("parent is blocked; retain notifications and resume after child progress")
		}
	}
}
