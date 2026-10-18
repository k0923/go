package cflow_test

import (
	"context"
	"fmt"
	"time"

	"github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
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

func (OrderFlow) Run(ctx *cflow.FlowContext, orderID string) (string, error) {
	submitted, err := ctx.Run("submit", cflow.AsAction(SubmitOrder), orderID)
	if err != nil {
		return "", err
	}
	return ctx.WaitFor[string]("approval", submitted), nil
}

func ExampleEngine_Drive() {
	store := memory.New()
	engine, err := cflow.NewEngine(store, cflow.Options{})
	if err != nil {
		panic(err)
	}
	order := cflow.NewWorkflow("order", "1", cflow.FlowFunc[string, string](func(f *cflow.FlowContext, id string) (string, error) {
		return f.RunFlow("order", OrderFlow{}, id)
	}))
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

func exampleDefinition[I, O any](name, version string, flow cflow.Flow[I, O]) cflow.Workflow[I, O] {
	return cflow.NewWorkflow(name, version, cflow.FlowFunc[I, O](func(f *cflow.FlowContext, input I) (O, error) {
		return f.RunFlow("main", flow, input)
	}))
}

func ExampleEngine_Subscribe() {
	engine, err := cflow.NewEngine(memory.New(), cflow.Options{})
	if err != nil {
		panic(err)
	}
	unsubscribe := engine.Subscribe(func(_ context.Context, event cflow.Event) {
		fmt.Println(event.Type, event.Key, event.Status)
	}, cflow.ActionFinished, cflow.DriveFinished)
	defer unsubscribe()

	definition := exampleDefinition("demo", "1",
		cflow.FlowFunc[int, int](func(ctx *cflow.FlowContext, input int) (int, error) {
			return ctx.Run("increment", cflow.Pure(func(n int) int { return n + 1 }), input)
		}),
	)
	if _, err := engine.Drive(context.Background(), definition, 1); err != nil {
		panic(err)
	}
	// Output:
	// action_finished main|increment COMPLETED
	// drive_finished demo COMPLETED
}

func ExampleOnSignal() {
	// Every case has its own response type; a timer carries none.
	// 每个候选项都有自己的响应类型；计时器不携带响应。
	type reply struct{ Text string }
	ctx := context.Background()
	store := memory.New()
	engine, err := cflow.NewEngine(store, cflow.Options{})
	if err != nil {
		panic(err)
	}
	flow := exampleDefinition("reply_or_timeout", "1",
		cflow.FlowFunc[string, string](func(f *cflow.FlowContext, request string) (string, error) {
			answer := cflow.OnSignal[reply]("reply", request)
			timeout := cflow.OnSleep("timeout", time.Hour)
			winner := f.WaitAny("signals", answer, timeout)
			if winner == timeout {
				return "timeout", nil
			}
			return answer.Value().Text, nil
		}),
	)
	waiting, err := engine.Drive(ctx, flow, "request")
	if err != nil {
		panic(err)
	}
	fmt.Println("waiting signals:", len(waiting.Pending))
	record, err := store.Get(ctx, "main|signals|reply")
	if err != nil {
		panic(err)
	}
	if _, err = engine.ResolveSignal(ctx, record.Key, record.Version, reply{Text: "answer"}); err != nil {
		panic(err)
	}
	completed, err := engine.Drive(ctx, flow, "request")
	if err != nil {
		panic(err)
	}
	timeout, err := store.Get(ctx, "main|signals|timeout")
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

func ExampleFlowContext_WaitAny() {
	ctx := context.Background()
	store := memory.New()
	engine, err := cflow.NewEngine(store, cflow.Options{})
	if err != nil {
		panic(err)
	}
	definition := exampleDefinition("approval", "1",
		cflow.FlowFunc[string, string](func(f *cflow.FlowContext, input string) (string, error) {
			primary := cflow.OnSignal[string]("primary", input)
			secondary := cflow.OnSignal[string]("secondary", input)
			winner := f.WaitAny("providers", primary, secondary)
			switch winner {
			case primary:
				return winner.Key() + ": " + primary.Value(), nil
			default:
				return winner.Key() + ": " + secondary.Value(), nil
			}
		}),
	)
	waiting, err := engine.Drive(ctx, definition, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(waiting.Status, len(waiting.Pending))
	// Both requests are durable and available for independent providers.
	// 两个请求都是持久化的，并且可供独立提供方使用。
	secondary, err := store.Get(ctx, "main|providers|secondary")
	if err != nil {
		panic(err)
	}
	if _, err = engine.ResolveSignal(ctx, secondary.Key, secondary.Version, "approved"); err != nil {
		panic(err)
	}
	primary, err := store.Get(ctx, "main|providers|primary")
	if err != nil {
		panic(err)
	}
	fmt.Println("primary:", primary.Status)
	completed, err := engine.Drive(ctx, definition, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(completed.Status, completed.Output)
	// Output:
	// SUSPENDED 2
	// primary: CANCELLED
	// COMPLETED main|providers|secondary: approved
}
