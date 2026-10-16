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
	definition := exampleDefinition("approval", "1",
		workflow.FlowFunc[string, string](func(f *workflow.FlowContext, input string) (string, error) {
			winner, err := f.WaitAny[string]("providers",
				workflow.WaitCase[string]{Key: "primary", Request: input},
				workflow.WaitCase[string]{Key: "secondary", Request: input},
			)
			if err != nil {
				return "", err
			}
			return winner.Key + ": " + winner.Response, nil
		}),
	)
	waiting, err := engine.Drive(ctx, definition, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(waiting.Status, len(waiting.Pending))
	// Both requests are durable and available for independent providers.
	// 两个请求都是持久化的，并且可供独立提供方使用。
	secondary, err := store.Get(ctx, "main|providers|secondary")
	if err != nil {
		panic(err)
	}
	if _, err = engine.ResolveSignal(ctx, secondary.Key, secondary.Version, "approved"); err != nil {
		panic(err)
	}
	primary, err := store.Get(ctx, "main|providers|primary")
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
	// COMPLETED main|providers|secondary: approved
}
