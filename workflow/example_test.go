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
// SubmitOrder 是普通具名函数，可被独立测试。真实 HTTP 实现会把稳定的 run ID
// 和 ActionKey(ctx) 转发给远端服务的幂等机制。
func SubmitOrder(ctx context.Context, orderID string) (string, error) {
	return "submitted_" + orderID, nil
}

func (OrderFlow) Run(ctx *workflow.FlowContext, orderID string) (string, error) {
	submitted, err := ctx.Run("submit", workflow.AsAction(SubmitOrder), orderID)
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
	order := workflow.NewWorkflow("order", "1", func(ctx *workflow.WorkflowContext, id string) (string, error) {
		return ctx.RunFlow("order", OrderFlow{}, id)
	})
	ctx := context.Background()
	first, err := engine.Drive(ctx, order, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(first.Status, first.Pending[0].Key)
	request, err := store.Get(ctx, "order|approval")
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
	// SUSPENDED order|approval
	// COMPLETED approved
}

func exampleDefinition[I, O any](name, version string, flow workflow.Flow[I, O]) workflow.Workflow[I, O] {
	return workflow.NewWorkflow(name, version, func(ctx *workflow.WorkflowContext, input I) (O, error) {
		return ctx.RunFlow("main", flow, input)
	})
}
