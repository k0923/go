// Interface Action with explicit dependency injection and durable replay.
// Run: go run ./cflow/examples/action_context_design
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

type Order struct {
	ID    string
	State string
}

type OrderRepository interface {
	Find(context.Context, string) (Order, error)
}

type Repository struct{ calls atomic.Int32 }

func (r *Repository) Find(ctx context.Context, id string) (Order, error) {
	if err := ctx.Err(); err != nil {
		return Order{}, err
	}
	r.calls.Add(1)
	return Order{ID: id, State: "approved"}, nil
}

type LoadOrderAction struct{ Repository OrderRepository }

func (a LoadOrderAction) Run(ctx context.Context, id string) (Order, error) {
	return a.Repository.Find(ctx, id)
}

// Flow holds executable steps; external clients belong to those Actions.
type OrderFlow struct {
	LoadOrder cflow.Action[string, Order]
}

func (flow OrderFlow) Run(base *cflow.FlowContext, input string) (Order, error) {
	// Safe closure: it captures no mutable state or IO dependencies.
	id, err := base.Run("normalize", cflow.AsAction(func(_ context.Context, value string) (string, error) {
		return value, nil
	}), input)
	if err != nil {
		return Order{}, err
	}
	return base.Run("load_order", flow.LoadOrder, id)
}

func main() {
	engine, err := cflow.NewEngine(memory.New(), cflow.Options{})
	if err != nil {
		panic(err)
	}
	repository := &Repository{}
	definition := cflow.NewWorkflow("order", "1", cflow.FlowFunc[string, Order](func(f *cflow.FlowContext, input string) (Order, error) {
		return f.RunFlow("order", OrderFlow{LoadOrder: LoadOrderAction{Repository: repository}}, input)
	}))
	for i := 0; i < 2; i++ {
		result, err := engine.Drive(context.Background(), definition, "order_123")
		if err != nil {
			panic(err)
		}
		fmt.Println(result.Status, result.Output.ID, result.Output.State)
	}
	if repository.calls.Load() != 1 {
		panic("Action IO was repeated during replay")
	}
	fmt.Println("repository calls:", repository.calls.Load())
}
