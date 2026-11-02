package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/sqlite"
)

func TestSQLiteRestoresJSONBuiltWorkflow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	if err := publishAndRun(ctx, path); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, path, "demo")
	if err != nil {
		t.Fatal(err)
	}
	review, ok, err := store.Node(ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if !ok || review.Status != dflow.Suspended || review.Output["total"] != float64(42) || review.Output["approver"] != "财务" {
		t.Fatalf("review did not suspend with persisted request data: %+v", review)
	}
	approved, err := saveCallback(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := resume(ctx, path, approved)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{
		"approved": true, "total": float64(42),
		"summary": "结果：财务 确认金额 42", "json": `{"approved": true}`,
	}
	if !reflect.DeepEqual(output, expected) {
		t.Fatalf("unexpected output: %v", output)
	}
}

func TestRejectInvalidDefinitionJSON(t *testing.T) {
	for _, raw := range []string{
		`{"version":"v1","nodes":[{"id":"n","node":{"type":"unknown"}}]}`,
		`{"version":"v1","nodes":[{"id":"n","node":null}]}`,
		`{"version":"v1","nodes":[{"id":"n","node":{"type":"script","content":"("}}]}`,
		`{"version":"v1","nodes":[{"id":"n","node":{"type":"review","approver":""}}]}`,
		`{"version":"v1","nodes":[{"id":"n","node":{"type":"end"},"inputs":{"x":null}}]}`,
		`{"version":"v1","nodes":[{"id":"n","node":{"type":"end"},"inputs":{"x":{"type":"reference","path":["other"]}}}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var wire DefinitionJSON
			err := json.Unmarshal([]byte(raw), &wire)
			if err == nil {
				_, err = wire.Compile(context.Background(), &reviewService{})
			}
			if err == nil {
				t.Fatal("invalid definition accepted")
			}
		})
	}
}

func TestDefinitionJSONRoundTrip(t *testing.T) {
	wire, err := loadDefinition()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var restored DefinitionJSON
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	records, err := restored.Compile(context.Background(), &reviewService{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0].GetID() != "calculate" {
		t.Fatalf("unexpected publication order: %+v", records)
	}
	if _, ok := records[2].GetNode().(interface {
		Run(context.Context, *nodes.Runtime) dflow.NodeResult
	}); !ok {
		t.Fatal("published node is not executable")
	}
}
