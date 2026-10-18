package tests

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/k0923/go/dflow"
)

type approvalSuspension struct {
	Approver string `json:"approver"`
	Document string `json:"document"`
}

func (*approvalSuspension) Type() string { return "test.approval" }
func (n *approvalSuspension) MarshalJSON() ([]byte, error) {
	type plain approvalSuspension
	return json.Marshal((*plain)(n))
}

func TestSuspensionCustomTypeRoundTripAndRecovery(t *testing.T) {
	backends(t, func(t *testing.T, infra dflow.Infrastructure) {
		registry := asyncRegistry(t, asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Suspend: &approvalSuspension{Approver: "alice", Document: "doc-1"}}, nil
			},
			resume: func(_ context.Context, _ map[string]any, resumed dflow.ResumeInput) (dflow.Result, error) {
				payload, ok := resumed.Suspension.(*approvalSuspension)
				if !ok || payload.Approver != "alice" || payload.Document != "doc-1" {
					return dflow.Result{}, errors.New("typed suspension was not restored")
				}
				return dflow.Result{Output: resumed.Response}, nil
			},
		})
		if err := registry.RegisterSuspension(func() dflow.Suspension { return &approvalSuspension{} }); err != nil {
			t.Fatal(err)
		}
		if err := registry.RegisterSuspension(func() dflow.Suspension { return &approvalSuspension{} }); err == nil {
			t.Fatal("duplicate suspension registered")
		}
		parent := engine(t, waitDefinition(), registry, infra, dflow.Options{})
		ctx := context.Background()
		state, err := parent.Run(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		messages := childRequests(t, infra.(dflow.Outbox))
		var envelope dflow.SuspensionEnvelope
		if err := json.Unmarshal(messages[0].Request, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Type != "test.approval" {
			t.Fatal(envelope)
		}
		decoded, err := registry.DecodeSuspension(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.(*approvalSuspension).Document != "doc-1" {
			t.Fatal(decoded)
		}
		token := state.Nodes["wait"].Suspension.Token
		if _, err := parent.Wakeup(ctx, "wait", token, true); err != nil {
			t.Fatal(err)
		}
		parent = engine(t, waitDefinition(), registry, infra, dflow.Options{})
		state, err = parent.Drain(ctx)
		if err != nil || state.Nodes["wait"].Output != true {
			t.Fatal(state, err)
		}
	})
}

type invalidPayload struct{}

func (*invalidPayload) Type() string                 { return "invalid" }
func (*invalidPayload) MarshalJSON() ([]byte, error) { return []byte("not json"), nil }
func TestSuspensionRejectsUnknownAndInvalidData(t *testing.T) {
	if _, err := dflow.EncodeSuspension((*approvalSuspension)(nil)); !errors.Is(err, dflow.ErrInvalidSuspension) {
		t.Fatal(err)
	}
	if _, err := dflow.EncodeSuspension(&invalidPayload{}); !errors.Is(err, dflow.ErrInvalidSuspension) {
		t.Fatal(err)
	}
	registry := dflow.DefaultRegistry()
	if _, err := registry.DecodeSuspension(dflow.SuspensionEnvelope{Type: "unknown", Data: json.RawMessage(`{}`)}); !errors.Is(err, dflow.ErrInvalidSuspension) {
		t.Fatal(err)
	}
	if len(registry.SuspensionTypes()) != 3 {
		t.Fatal(registry.SuspensionTypes())
	}
}
