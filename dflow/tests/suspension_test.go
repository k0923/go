package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/k0923/go/dflow"
)

type approvalSuspension struct {
	Approver string `json:"approver"`
	Document string `json:"document"`
}

func (*approvalSuspension) Type() string { return "test.approval" }

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
		parent := engine(t, waitDefinition(), registry, infra, dflow.Options{})
		ctx := context.Background()
		state, err := parent.Run(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		messages := childRequests(t, infra.(dflow.Outbox))
		decoded := messages[0].Request
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

func TestSuspensionRejectsTypedNilAndEmptyType(t *testing.T) {
	for _, payload := range []dflow.Suspension{(*approvalSuspension)(nil), &dflow.ValueSuspension{}} {
		registry := asyncRegistry(t, asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Suspend: payload}, nil
			},
		})
		backends(t, func(t *testing.T, infra dflow.Infrastructure) {
			_, err := engine(t, waitDefinition(), registry, infra, dflow.Options{}).Run(context.Background(), nil)
			if !errors.Is(err, dflow.ErrInvalidSuspension) {
				t.Fatal(err)
			}
		})
	}
}
