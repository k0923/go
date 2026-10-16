// Run with: go run ./workflow/examples/wait_any
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
			// Downstream work starts without waiting for the other response.
			return f.Run("continue", workflow.Pure(func(answer string) string { return "continued: " + answer }), winner.Response)
		}),
	}
	waiting, err := engine.Drive(ctx, definition, "order_123")
	if err != nil {
		return err
	}
	fmt.Println("active waits:", len(waiting.Pending))

	// Two independent external listeners. The durable wait mechanism itself
	// creates no goroutines. These mocks demonstrate cleanup of provider IO.
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
	}()
	go func() {
		defer providers.Done()
		<-primaryListening
		record, err := store.Get(providerCtx, "providers|secondary")
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
	loser, err := store.Get(ctx, "providers|primary")
	if err != nil {
		return err
	}
	fmt.Println(result.Output)
	fmt.Println("primary:", loser.Status)
	fmt.Println("provider listeners stopped")
	return nil
}
