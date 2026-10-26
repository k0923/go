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

func TestSQLiteRestoresPublicationSuspensionAndCallback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	if err := publishAndRun(ctx, path); err != nil {
		t.Fatal(err)
	}
	service := &reviewService{}
	store, err := sqlite.Open(ctx, path, "demo", Codec{Service: service})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.Provider("review-v1").LoadNode(ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	review, ok := node.Spec.Node.(*reviewNode)
	if !ok || review.service != service {
		t.Fatal("publication did not rebuild the node with the current service")
	}
	state, err := store.GetNode(ctx, "review")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Status != dflow.Suspended || state.Suspension == nil {
		t.Fatal("suspension was not persisted", state)
	}
	request, ok := state.Suspension.Payload.(*ReviewRequest)
	if !ok || request.Total != 42 || request.Approver != "财务" {
		t.Fatal("suspension type/value was lost", state.Suspension.Payload)
	}

	token, err := saveCallback(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	response, err := store.GetResponse(ctx, "review", token)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil {
		t.Fatal("callback was not persisted")
	}
	reply, ok := response.Value.(*ReviewReply)
	if !ok || !reply.Approved {
		t.Fatal("callback type/value was lost", response.Value)
	}
	output, err := resume(ctx, path, token)
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
	messages, err := store.Pending(ctx)
	if err != nil || len(messages) != 0 {
		t.Fatal("notifications were not acknowledged", messages, err)
	}
}

func TestCodecPreservesPortSelectionAndNullCallback(t *testing.T) {
	codec := Codec{}
	for _, ports := range [][]string{nil, {}, {"true"}} {
		original := dflow.NodeState{Status: dflow.Succeeded, Ports: ports}
		raw, err := codec.Encode(original)
		if err != nil {
			t.Fatal(err)
		}
		var restored dflow.NodeState
		if err := codec.Decode(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(original, restored) {
			t.Fatalf("port selection changed: %s", raw)
		}
	}
	raw, err := codec.Encode(dflow.Response{Value: nil})
	if err != nil {
		t.Fatal(err)
	}
	var response dflow.Response
	if err := codec.Decode(raw, &response); err != nil || response.Value != nil {
		t.Fatal("explicit nil callback was lost", response, err)
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
				var def dflow.Definition
				def, err = wire.Build(&reviewService{})
				if err == nil {
					_, err = dflow.Compile(def)
				}
			}
			if err == nil {
				t.Fatal("invalid definition accepted")
			}
		})
	}
}

func TestCodecRejectsUnsupportedValues(t *testing.T) {
	for _, value := range []any{
		dflow.Response{Value: make(chan int)},
		dflow.PublishedNode{Spec: dflow.NodeSpec{Node: &nodes.HTTPNode{}}},
		dflow.PublishedNode{Spec: dflow.NodeSpec{
			Node: &nodes.EndNode{}, Inputs: dflow.Parameters{"x": dflow.Constant("unregistered")},
		}},
		make(chan int),
	} {
		if _, err := (Codec{}).Encode(value); err == nil {
			t.Fatalf("unsupported value %T accepted", value)
		}
	}
	if err := (Codec{}).Decode([]byte(`{}`), new(dflow.Result)); err == nil {
		t.Fatal("unsupported target accepted")
	}
}
