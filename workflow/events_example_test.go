package workflow_test

import (
	"context"
	"fmt"

	"github.com/k0923/go/workflow"
)

func ExampleEngine_Subscribe() {
	engine, err := workflow.NewEngine(workflow.NewMemoryInfrastructure(), workflow.Options{})
	if err != nil {
		panic(err)
	}
	unsubscribe := engine.Subscribe(func(_ context.Context, event workflow.Event) {
		fmt.Println(event.Type, event.Key, event.Status)
	}, workflow.ActionFinished, workflow.DriveFinished)
	defer unsubscribe()

	definition := workflow.Workflow[int, int]{
		Name: "demo", Version: "1",
		Flow: workflow.FlowFunc[int, int](func(ctx *workflow.FlowContext, input int) (int, error) {
			return ctx.Run("increment", workflow.Pure(func(n int) int { return n + 1 }), input)
		}),
	}
	if _, err := engine.Drive(context.Background(), definition, 1); err != nil {
		panic(err)
	}
	// Output:
	// action_finished increment COMPLETED
	// drive_finished demo COMPLETED
}
