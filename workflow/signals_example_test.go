package workflow_test

import (
	"context"
	"fmt"
	"time"

	"github.com/k0923/go/workflow"
)

func ExampleWaitForSignal() {
	ctx := context.Background()
	store := workflow.NewMemoryInfrastructure()
	engine, err := workflow.NewEngine(store, workflow.Options{})
	if err != nil {
		panic(err)
	}
	flow := workflow.Workflow[string, string]{
		Name: "reply_or_timeout", Version: "1",
		Flow: workflow.FlowFunc[string, string](func(f *workflow.FlowContext, request string) (string, error) {
			winner, err := f.WaitAny[string]("signals",
				workflow.WaitForSignal("reply", request),
				workflow.SleepSignal("timeout", time.Hour),
			)
			if err != nil {
				return "", err
			}
			if winner.Kind == workflow.SleepKind {
				return "timeout", nil
			}
			return winner.Response, nil
		}),
	}
	waiting, err := engine.Drive(ctx, flow, "request")
	if err != nil {
		panic(err)
	}
	fmt.Println("waiting signals:", len(waiting.Pending))
	record, err := store.Get(ctx, "signals|reply")
	if err != nil {
		panic(err)
	}
	if _, err = engine.ResolveSignal(ctx, record.Key, record.Version, "answer"); err != nil {
		panic(err)
	}
	completed, err := engine.Drive(ctx, flow, "request")
	if err != nil {
		panic(err)
	}
	timeout, err := store.Get(ctx, "signals|timeout")
	if err != nil {
		panic(err)
	}
	fmt.Println("completed:", completed.Output)
	fmt.Println("timeout:", timeout.Status)
	// Output:
	// waiting signals: 2
	// completed: answer
	// timeout: CANCELLED
}
