package flow_test

import (
	"context"
	"testing"

	"github.com/k0923/go/flow"
)

func TestRuntimeIsUsableOutsideFlowPackage(t *testing.T) {
	infrastructure := flow.NewInMemoryInfrastructure[string, string]()
	target := flow.Flow[string, string]{
		Handler: func(runtime *flow.Runtime[string, string]) string {
			return runtime.Step(
				runtime.Context,
				"public-step",
				func(flow.StepContext[string]) (string, error) {
					return runtime.GetInput(), nil
				},
			)
		},
	}

	target.StartRun(context.Background(), "input", infrastructure)

	state, err := infrastructure.GetState(context.Background())
	if err != nil {
		t.Fatalf("get state failed: %v", err)
	}
	if state.Status != flow.FlowStatusCompleted || state.Output != "input" {
		t.Fatalf("unexpected state: %+v", state)
	}
}
