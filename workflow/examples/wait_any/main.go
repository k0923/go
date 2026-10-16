// Run with: go run ./workflow/examples/wait_any
// 运行方式：go run ./workflow/examples/wait_any
package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/k0923/go/workflow"
)

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	ctx := context.Background()
	store := workflow.NewMemoryInfrastructure()
	engine, err := workflow.NewEngine(store, workflow.Options{})
	if err != nil {
		return err
	}
	flow := workflow.FlowFunc[string, string](func(f *workflow.FlowContext, input string) (string, error) {
		winner, err := f.WaitAny[string]("providers",
			workflow.WaitCase[string]{Key: "primary", Request: input},
			workflow.WaitCase[string]{Key: "secondary", Request: input},
		)
		if err != nil {
			return "", err
		}
		// Downstream work starts without waiting for the other response.
		// 下游工作会立即开始，而不会等待另一个响应。
		return f.Run("continue", workflow.Pure(func(answer string) string { return "continued: " + answer }), winner.Response)
	})
	definition := workflow.NewWorkflow("approval", "1", func(w *workflow.WorkflowContext, input string) (string, error) {
		return w.RunFlow("main", flow, input)
	})
	waiting, err := engine.Drive(ctx, definition, "order_123")
	if err != nil {
		return err
	}
	fmt.Println("active waits:", len(waiting.Pending))

	// Two independent external listeners. The durable wait mechanism itself
	// creates no goroutines. These mocks demonstrate cleanup of provider IO.
	// 两个独立的外部监听器。持久化等待机制本身不会创建 goroutine。
	// 这些 mock 展示了如何清理提供方 IO。
	providerCtx, cancelProviders := context.WithCancel(ctx)
	var providers sync.WaitGroup
	defer func() { cancelProviders(); providers.Wait() }()
	primaryListening := make(chan struct{})
	completed := make(chan error, 1)
	providers.Add(2)
	go func() {
		defer providers.Done()
		close(primaryListening)
		<-providerCtx.Done() // primary never replies; cancel its listener
		// primary 永不回复；取消其监听器。
	}()
	go func() {
		defer providers.Done()
		<-primaryListening
		record, err := store.Get(providerCtx, "main|providers|secondary")
		if err == nil {
			_, err = engine.ResolveSignal(providerCtx, record.Key, record.Version, "secondary approved")
		}
		completed <- err
	}()
	if err = <-completed; err != nil {
		return err
	}
	cancelProviders()
	providers.Wait()
	result, err := engine.Drive(ctx, definition, "order_123")
	if err != nil {
		return err
	}
	loser, err := store.Get(ctx, "main|providers|primary")
	if err != nil {
		return err
	}
	fmt.Println(result.Output)
	fmt.Println("primary:", loser.Status)
	fmt.Println("provider listeners stopped")
	return nil
}
