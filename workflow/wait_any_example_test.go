package workflow_test

import (
	"context"
	"fmt"

	"github.com/k0923/go/workflow"
)

func ExampleFlowContext_WaitAny() {
	ctx := context.Background()
	store := workflow.NewMemoryInfrastructure()
	engine, err := workflow.NewEngine(store, workflow.Options{})
	if err != nil {
		panic(err)
	}
	definition := workflow.Workflow[string, string]{
		Name: "approval", Version: "1",
		Flow: workflow.FlowFunc[string, string](func(f *workflow.FlowContext, input string) (string, error) {
			winner, err := f.WaitAny[string]("providers",
				workflow.WaitCase[string]{Key: "primary", Request: input},
				workflow.WaitCase[string]{Key: "secondary", Request: input},
			)
			if err != nil {
				return "", err
			}
			return winner.Key + ": " + winner.Response, nil
		}),
	}
	waiting, err := engine.Drive(ctx, definition, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(waiting.Status, len(waiting.Pending))
	// Both requests are durable and available for independent providers.
	secondary, err := store.Get(ctx, "providers|secondary")
	if err != nil {
		panic(err)
	}
	if _, err = engine.ResolveSignal(ctx, secondary.Key, secondary.Version, "approved"); err != nil {
		panic(err)
	}
	primary, err := store.Get(ctx, "providers|primary")
	if err != nil {
		panic(err)
	}
	fmt.Println("primary:", primary.Status)
	completed, err := engine.Drive(ctx, definition, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(completed.Status, completed.Output)
	// Output:
	// SUSPENDED 2
	// primary: CANCELLED
	// COMPLETED providers|secondary: approved
}
