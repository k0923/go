// Run with: go run ./cflow/examples/for_each
// 运行方式：go run ./cflow/examples/for_each
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

type Order struct{ ID string }

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	ctx := context.Background()
	store := memory.New()
	submit := cflow.AsAction(func(_ context.Context, order Order) (string, error) {
		fmt.Println("submit:", order.ID) // Only real Action execution prints.
		return "receipt:" + order.ID, nil
	})
	body := cflow.FlowFunc[Order, string](func(f *cflow.FlowContext, order Order) (string, error) {
		receipt, err := f.Run("submit", submit, order)
		if err != nil {
			return "", err
		}
		approval := f.WaitFor[string]("approval", order)
		return receipt + ":" + approval, nil
	})
	notify := cflow.FlowFunc[Order, string](func(f *cflow.FlowContext, order Order) (string, error) {
		return f.Run("send", cflow.AsAction(func(_ context.Context, order Order) (string, error) {
			fmt.Println("notify:", order.ID)
			return "sent:" + order.ID, nil
		}), order)
	})
	key := func(order Order) string { return order.ID }
	workflow := cflow.FlowFunc[[]Order, []string](
		func(f *cflow.FlowContext, orders []Order) ([]string, error) {
			// Up to two orders await approval at a time; each finished one
			// admits the next. 最多两个订单同时等待审批，每完成一个就补位下一个。
			receipts, err := f.ForEach("orders", body, orders, key, cflow.Concurrency(2))
			if err != nil {
				return nil, err
			}
			// ForItems reuses a Flow for its side effect and drops the output.
			// ForItems 复用 Flow 只为副作用，丢弃输出。
			return receipts, f.ForItems("notify", notify, orders, key, cflow.Concurrency(3))
		})
	orders := []Order{{ID: "101"}, {ID: "102"}, {ID: "103"}}
	options := cflow.Options{Flows: map[string]cflow.FlowDefinition{"order": body, "notify": notify, "batch_orders": workflow}}
	engine, err := cflow.NewEngine(store, options)
	if err != nil {
		return err
	}
	first, err := engine.Start(ctx, workflow, orders)
	if err != nil || first.Status != cflow.Suspended {
		return fmt.Errorf("initial result: %+v: %w", first, err)
	}
	for {
		// Recreate the engine to demonstrate recovery from the same store.
		// 每次重建引擎，演示从同一存储恢复。
		engine, err := cflow.NewEngine(store, options)
		if err != nil {
			return err
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			return fmt.Errorf("no wake or approval request")
		}
		for _, message := range messages {
			switch message.Purpose {
			case memory.RequestNotification:
				fmt.Println("approve:", message.Key)
				if _, err := engine.Signal(ctx, message.Key, "approved"); err != nil {
					return err
				}
			case memory.WakeNotification:
				// One call for every wake: "" is the root, others are items.
				// 所有唤醒同一种调用：空串是根，其他是元素。
				fmt.Printf("resume %q\n", message.ExecutionKey)
				result, err := engine.Resume(ctx, message.ExecutionKey)
				if err != nil {
					return err
				}
				if message.ExecutionKey == "" && result.Status == cflow.Completed {
					if err := store.Ack(ctx, message.ID); err != nil {
						return err
					}
					fmt.Println("results:", string(result.Output))
					return nil
				}
			}
			if err := store.Ack(ctx, message.ID); err != nil {
				return err
			}
		}
	}
}
