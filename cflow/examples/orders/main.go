// Run: go run ./cflow/examples/orders
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

type Item struct {
	ID    string
	Price int
}

type Order struct {
	ID    string
	Items []Item
}

type Payment struct {
	OrderID string
	Amount  int
}

type Summary struct {
	OrderID, Receipt string
	Amount           int
}

type PrepareItemFlow struct {
	cflow.FlowBase[Item, int]
	Reserve cflow.Action[Item, int]
}

func (p PrepareItemFlow) Run(f *cflow.FlowContext, item Item) (int, error) {
	return f.Run("reserve", p.Reserve, item)
}

type PaymentFlow struct {
	cflow.FlowBase[Payment, string]
	Charge cflow.Action[Payment, string]
}

func (p PaymentFlow) Run(f *cflow.FlowContext, payment Payment) (string, error) {
	if !f.WaitFor[bool]("approval", payment) {
		return "", fmt.Errorf("payment declined")
	}
	return f.Run("charge", p.Charge, payment)
}

type OrderFlow struct {
	cflow.FlowBase[Order, Summary]
	Prepare cflow.Flow[Item, int]
	Payment cflow.Flow[Payment, string]
}

func (o OrderFlow) Run(f *cflow.FlowContext, order Order) (Summary, error) {
	amounts, err := f.ForEach("items", o.Prepare, order.Items, func(item Item) string { return item.ID })
	if err != nil {
		return Summary{}, err
	}
	total := 0
	for _, amount := range amounts {
		total += amount
	}
	receipt, err := f.RunFlow("payment", o.Payment, Payment{OrderID: order.ID, Amount: total})
	return Summary{OrderID: order.ID, Receipt: receipt, Amount: total}, err
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	ctx := context.Background()
	store := memory.New()
	prepare := PrepareItemFlow{Reserve: cflow.AsAction(func(_ context.Context, item Item) (int, error) {
		fmt.Println("reserve:", item.ID)
		return item.Price, nil
	})}
	payment := PaymentFlow{Charge: cflow.AsAction(func(ctx context.Context, payment Payment) (string, error) {
		fmt.Println("charge:", payment.OrderID, payment.Amount, cflow.ActionKey(ctx))
		return "receipt:" + payment.OrderID, nil
	})}
	orderFlow := OrderFlow{Prepare: prepare, Payment: payment}
	options := cflow.Options{Flows: map[string]cflow.FlowDefinition{
		"order": orderFlow, "prepareItem": prepare, "payment": payment,
	}}
	order := Order{ID: "order_101", Items: []Item{{ID: "book", Price: 40}, {ID: "pen", Price: 10}}}
	engine, err := cflow.NewEngine(store, options)
	if err != nil {
		return err
	}
	if result, err := engine.Start(ctx, orderFlow, order); err != nil || result.Status != cflow.Suspended {
		return fmt.Errorf("start: %s: %v", result.Status, err)
	}
	for {
		// Only persistent snapshots and the registry are needed after restart.
		engine, err := cflow.NewEngine(store, options)
		if err != nil {
			return err
		}
		messages, err := store.Pending(ctx, time.Now())
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			return fmt.Errorf("missing wake")
		}
		for _, message := range messages {
			switch message.Purpose {
			case memory.RequestNotification:
				fmt.Println("approve:", message.Key)
				if _, err := engine.Signal(ctx, message.Key, true); err != nil {
					return err
				}
			case memory.WakeNotification:
				// One call for every wake: "" is the root, others are children.
				// 所有唤醒同一种调用：空串是根，其他是子 Flow。
				fmt.Printf("resume %q\n", message.ExecutionKey)
				result, err := engine.Resume(ctx, message.ExecutionKey)
				if err != nil {
					return err
				}
				if message.ExecutionKey == "" && result.Status == cflow.Completed {
					if err := store.Ack(ctx, message.ID); err != nil {
						return err
					}
					fmt.Println("completed:", string(result.Output))
					return nil
				}
			}
			if err := store.Ack(ctx, message.ID); err != nil {
				return err
			}
		}
	}
}
