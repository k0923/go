// Run from the module root: go run ./dflow/examples/xjson
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/driver"
	"github.com/k0923/go/dflow/sqlite"
)

//go:embed workflow.json
var workflowJSON []byte

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "dflow-xjson-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()
	path := filepath.Join(dir, "review.db")
	if err := publishAndRun(ctx, path); err != nil {
		return err
	}
	token, err := saveCallback(ctx, path)
	if err != nil {
		return err
	}
	output, err := resume(ctx, path, token)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println("JSON 定义发布、挂起和类型恢复成功：")
	fmt.Println(string(raw))
	return nil
}

func publishAndRun(ctx context.Context, path string) error {
	var wire DefinitionJSON
	if err := json.Unmarshal(workflowJSON, &wire); err != nil {
		return err
	}
	// The original DTO remains serializable even after runtime nodes are built.
	raw, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	var restored DefinitionJSON
	if err := json.Unmarshal(raw, &restored); err != nil {
		return err
	}
	def, err := restored.Build(&reviewService{})
	if err != nil {
		return err
	}
	graph, err := dflow.Compile(def)
	if err != nil {
		return err
	}
	store, err := sqlite.Open(ctx, path, "demo", Codec{Service: &reviewService{}})
	if err != nil {
		return err
	}
	defer store.Close()
	provider, err := store.Publish(ctx, "review-v1", graph)
	if err != nil {
		return err
	}
	engine, err := dflow.NewEngineWithProvider(provider, store, dflow.Options{})
	if err != nil {
		return err
	}
	// JSON numeric values are float64 in this application's input/output maps.
	_, err = driver.Run(ctx, engine, map[string]any{"number": float64(21)})
	return err
}

func saveCallback(ctx context.Context, path string) (string, error) {
	// Reopen with fresh dependencies, as a callback handler in another process.
	store, err := sqlite.Open(ctx, path, "demo", Codec{Service: &reviewService{}})
	if err != nil {
		return "", err
	}
	defer store.Close()
	messages, err := store.Pending(ctx)
	if err != nil {
		return "", err
	}
	for _, message := range messages {
		if message.Kind != dflow.RequestNotification {
			continue
		}
		request, ok := message.Request.(*ReviewRequest)
		if !ok || request.Total != 42 {
			return "", fmt.Errorf("request type/value was not restored: %T", message.Request)
		}
		engine, err := dflow.NewEngineWithProvider(store.Provider("review-v1"), store, dflow.Options{})
		if err != nil {
			return "", err
		}
		accepted, err := engine.Wakeup(ctx, message.NodeID, message.Token, &ReviewReply{Approved: true})
		if err != nil {
			return "", err
		}
		if !accepted {
			return "", fmt.Errorf("callback was not accepted")
		}
		return message.Token, store.Ack(ctx, message.ID)
	}
	return "", fmt.Errorf("review request missing")
}

func resume(ctx context.Context, path, token string) (map[string]any, error) {
	// Reopen again: Resume must restore both the suspension and typed callback.
	store, err := sqlite.Open(ctx, path, "demo", Codec{Service: &reviewService{}})
	if err != nil {
		return nil, err
	}
	defer store.Close()
	engine, err := dflow.NewEngineWithProvider(store.Provider("review-v1"), store, dflow.Options{})
	if err != nil {
		return nil, err
	}
	if _, err := driver.Resume(ctx, engine, "review", token); err != nil {
		return nil, err
	}
	end, err := store.GetNode(ctx, "end")
	if err != nil {
		return nil, err
	}
	if end == nil || end.Status != dflow.Succeeded {
		return nil, fmt.Errorf("end did not succeed")
	}
	messages, err := store.Pending(ctx)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if message.Kind == dflow.WakeNotification && message.Token == token {
			if err := store.Ack(ctx, message.ID); err != nil {
				return nil, err
			}
		}
	}
	return end.Output, nil
}
