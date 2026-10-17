package memory_test

import (
	"context"
	"fmt"
	"time"

	"github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// A dispatcher owned by the caller drains the outbox: REQUEST goes to the
// provider, WAKE drives the run, CANCEL stops a losing provider.
// 由调用方持有的调度器消费发件箱：REQUEST 交给提供方，WAKE 驱动 run，
// CANCEL 停止失败的提供方。
func ExampleOutbox() {
	ctx := context.Background()
	store := memory.New()
	engine, err := workflow.NewEngine(store, workflow.Options{})
	if err != nil {
		panic(err)
	}
	definition := workflow.NewWorkflowFromFlow("approval", "1",
		workflow.FlowFunc[string, string](func(f *workflow.FlowContext, order string) (string, error) {
			return order + " " + f.WaitFor[string]("approval", order), nil
		}),
	)
	result, err := engine.Drive(ctx, definition, "order_1")
	if err != nil {
		panic(err)
	}
	fmt.Println("drive:", result.Status)
	var outbox memory.Outbox = store
	for range 3 {
		messages, err := outbox.Pending(ctx, time.Now())
		if err != nil {
			panic(err)
		}
		for _, message := range messages {
			switch message.Purpose {
			case memory.RequestNotification:
				// Hand the request to the provider; this one answers immediately.
				// 把请求交给提供方；这里的提供方立即回答。
				fmt.Println("request:", message.Key)
				if _, err := engine.Signal(ctx, message.Key, "approved"); err != nil {
					panic(err)
				}
			case memory.WakeNotification:
				result, err := engine.Drive(ctx, definition, "order_1")
				if result.Status == workflow.Running {
					continue // retryable: keep the message and retry later
				}
				fmt.Println("wake:", result.Status, result.Output, err)
			case memory.CancelNotification:
				fmt.Println("cancel:", message.Key)
			}
			if err := outbox.Ack(ctx, message.ID); err != nil {
				panic(err)
			}
		}
	}
	// Output:
	// drive: SUSPENDED
	// request: approval
	// wake: COMPLETED order_1 approved <nil>
}
