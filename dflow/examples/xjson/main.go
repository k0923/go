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
	"github.com/k0923/go/dflow/examples/nodes"
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
	approved, err := saveCallback(ctx, path)
	if err != nil {
		return err
	}
	output, err := resume(ctx, path, approved)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println("JSON 定义构建、挂起和恢复成功：")
	fmt.Println(string(raw))
	return nil
}

func loadDefinition() (DefinitionJSON, error) {
	var wire DefinitionJSON
	if err := json.Unmarshal(workflowJSON, &wire); err != nil {
		return wire, err
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return wire, err
	}
	var restored DefinitionJSON
	return restored, json.Unmarshal(raw, &restored)
}

func compileRecords(ctx context.Context) ([]dflow.PublishedNode[*nodes.Runtime], error) {
	wire, err := loadDefinition()
	if err != nil {
		return nil, err
	}
	return wire.Compile(ctx, &reviewService{})
}

func publishAndRun(ctx context.Context, path string) error {
	records, err := compileRecords(ctx)
	if err != nil {
		return err
	}
	store, publication, err := sqlite.NewRun(ctx, path, "demo", records, "calculate")
	if err != nil {
		return err
	}
	defer store.Close()
	engine, err := dflow.NewEngine(publication, store, dflow.Options{})
	if err != nil {
		return err
	}
	_, err = driver.Run(ctx, engine, "calculate", nodes.RuntimeWithStore(map[string]any{"number": float64(21)}, store))
	return err
}

func saveCallback(ctx context.Context, path string) (bool, error) {
	store, err := sqlite.Open(ctx, path, "demo")
	if err != nil {
		return false, err
	}
	defer store.Close()
	state, ok, err := store.Node(ctx, "review")
	if err != nil {
		return false, err
	}
	if !ok || state.Status != dflow.Suspended || state.Output["total"] != float64(42) || state.Output["approver"] != "财务" {
		return false, fmt.Errorf("review request was not persisted as suspended output: %+v", state)
	}
	return true, nil
}

func resume(ctx context.Context, path string, approved bool) (map[string]any, error) {
	records, err := compileRecords(ctx)
	if err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, path, "demo")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	publication, err := sqlite.Publish(records)
	if err != nil {
		return nil, err
	}
	engine, err := dflow.NewEngine(publication, store, dflow.Options{})
	if err != nil {
		return nil, err
	}
	if _, err := driver.Run(ctx, engine, "review", nodes.RuntimeWithStore(map[string]any{"approved": approved}, store)); err != nil {
		return nil, err
	}
	end, ok, err := store.Node(ctx, "end")
	if err != nil {
		return nil, err
	}
	if !ok || end.Status != dflow.Succeeded {
		return nil, fmt.Errorf("end did not succeed")
	}
	return end.Output, nil
}
