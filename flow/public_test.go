package flow_test

import (
	"context"
	"testing"

	"github.com/k0923/go/flow"
)

func TestRuntimeIsUsableOutsideFlowPackage(t *testing.T) {
	infrastructure := flow.NewInMemoryInfrastructure[string, string]()
	target := flow.Flow[string, string]{
		Handler: func(runtime *flow.WorkflowContext[string, string]) string {
			return runtime.Go("public-go", func(goContext *flow.GoContext[string, string]) string {
				return goContext.Step(
					"public-step",
					func(stepContext flow.StepContext[string, string]) (string, error) {
						return stepContext.GetInput(), nil
					},
				)
			}).Wait()
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
