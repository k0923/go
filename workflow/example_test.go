package workflow_test

import (
	"context"
	"fmt"

	"github.com/k0923/go/workflow"
)

type OrderFlow struct{}

// SubmitOrder is an ordinary named function and can be tested independently.
// A real HTTP implementation would forward a stable run ID + ActionKey(ctx)
// to the remote service's idempotency mechanism.
func SubmitOrder(ctx context.Context, orderID string) (string, error) {
	return "submitted_" + orderID, nil
}

func (OrderFlow) Run(ctx *workflow.FlowContext, orderID string) (string, error) {
	submitted, err := ctx.Run("submit", SubmitOrder, orderID)
	if err != nil {
		return "", err
	}
	return ctx.WaitFor[string]("approval", submitted)
}

func ExampleEngine_Drive() {
	store := workflow.NewMemoryInfrastructure()
	engine, err := workflow.NewEngine(store, workflow.Options{})
	if err != nil {
		panic(err)
	}
	order := workflow.Workflow[string, string]{
		Name: "order",
		Version: "1",
		Flow: OrderFlow{},
	}
	ctx := context.Background()
	first, err := engine.Drive(ctx, order, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(first.Status, first.Pending[0].Key)
	request, err := store.Get(ctx, "approval")
	if err != nil {
		panic(err)
	}
	_, err = engine.ResolveSignal(ctx, request.Key, request.Version, "approved")
	if err != nil {
		panic(err)
	}
	second, err := engine.Drive(ctx, order, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(second.Status, second.Output)
	// Output:
	// SUSPENDED approval
	// COMPLETED approved
}
