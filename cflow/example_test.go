package cflow_test

import (
	"context"
	"fmt"
	"time"

	"github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

type OrderFlow struct{ cflow.FlowBase[string, string] }

// SubmitOrder is an ordinary named function and can be tested independently.
// A real HTTP implementation would forward a stable run ID + ActionKey(ctx)
// to the remote service's idempotency mechanism.
// SubmitOrder 是普通具名函数，可被独立测试。真实 HTTP 实现会把稳定的 run ID
// 和 ActionKey(ctx) 转发给远端服务的幂等机制。
func SubmitOrder(ctx context.Context, orderID string) (string, error) {
	return "submitted_" + orderID, nil
}

func (OrderFlow) Run(f *cflow.FlowContext, orderID string) (string, error) {
	submitted, err := f.Run("submit", cflow.AsAction(SubmitOrder), orderID)
	if err != nil {
		return "", err
	}
	return f.WaitFor[string]("approval", submitted), nil
}

func ExampleEngine_Start() {
	store := memory.New()
	order := OrderFlow{}
	engine, err := cflow.NewEngine(store, cflow.Options{
		Flows: map[string]cflow.FlowDefinition{"order": order},
	})
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	first, err := engine.Start(ctx, order, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(first.Status, first.Pending[0].Key)
	request, err := store.Get(ctx, "approval")
	if err != nil {
		panic(err)
	}
	_, err = engine.ResolveSignal(ctx, request.Key, request.Version, "approved")
	if err != nil {
		panic(err)
	}
	// The callback's WAKE resumes the root; Flow and input are already stored.
	second, err := engine.Resume(ctx, "")
	if err != nil {
		panic(err)
	}
	fmt.Println(second.Status, string(second.Output))
	// Output:
	// SUSPENDED approval
	// COMPLETED "approved"
}

func ExampleEngine_Subscribe() {
	definition := cflow.FlowFunc[int, int](func(f *cflow.FlowContext, input int) (int, error) {
		return f.Run("increment", cflow.Pure(func(n int) int { return n + 1 }), input)
	})
	engine, err := cflow.NewEngine(memory.New(), cflow.Options{
		Flows: map[string]cflow.FlowDefinition{"demo": definition},
	})
	if err != nil {
		panic(err)
	}
	unsubscribe := engine.Subscribe(func(_ context.Context, event cflow.Event) {
		fmt.Println(event.Type, event.Key, event.Status)
	}, cflow.ActionFinished, cflow.DriveFinished)
	defer unsubscribe()

	if _, err := engine.Start(context.Background(), definition, 1); err != nil {
		panic(err)
	}
	// Output:
	// action_finished increment COMPLETED
	// drive_finished demo COMPLETED
}

func ExampleOnSignal() {
	// Every case has its own response type; a timer carries none.
	// 每个候选项都有自己的响应类型；计时器不携带响应。
	type reply struct{ Text string }
	ctx := context.Background()
	store := memory.New()
	flow := cflow.FlowFunc[string, string](func(f *cflow.FlowContext, request string) (string, error) {
		answer := cflow.OnSignal[reply]("reply", request)
		timeout := cflow.OnSleep("timeout", time.Hour)
		winner := f.WaitAny("signals", answer, timeout)
		if winner == timeout {
			return "timeout", nil
		}
		return answer.Value().Text, nil
	})
	engine, err := cflow.NewEngine(store, cflow.Options{
		Flows: map[string]cflow.FlowDefinition{"reply_or_timeout": flow},
	})
	if err != nil {
		panic(err)
	}
	waiting, err := engine.Start(ctx, flow, "request")
	if err != nil {
		panic(err)
	}
	fmt.Println("waiting signals:", len(waiting.Pending))
	record, err := store.Get(ctx, "signals|reply")
	if err != nil {
		panic(err)
	}
	if _, err = engine.ResolveSignal(ctx, record.Key, record.Version, reply{Text: "answer"}); err != nil {
		panic(err)
	}
	completed, err := engine.Start(ctx, flow, "request")
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

func ExampleFlowContext_WaitAny() {
	ctx := context.Background()
	store := memory.New()
	definition := cflow.FlowFunc[string, string](func(f *cflow.FlowContext, input string) (string, error) {
		primary := cflow.OnSignal[string]("primary", input)
		secondary := cflow.OnSignal[string]("secondary", input)
		winner := f.WaitAny("providers", primary, secondary)
		switch winner {
		case primary:
			return winner.Key() + ": " + primary.Value(), nil
		default:
			return winner.Key() + ": " + secondary.Value(), nil
		}
	})
	engine, err := cflow.NewEngine(store, cflow.Options{
		Flows: map[string]cflow.FlowDefinition{"approval": definition},
	})
	if err != nil {
		panic(err)
	}
	waiting, err := engine.Start(ctx, definition, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(waiting.Status, len(waiting.Pending))
	// Both requests are durable and available for independent providers.
	// 两个请求都是持久化的，并且可供独立提供方使用。
	secondary, err := store.Get(ctx, "providers|secondary")
	if err != nil {
		panic(err)
	}
	if _, err = engine.ResolveSignal(ctx, secondary.Key, secondary.Version, "approved"); err != nil {
		panic(err)
	}
	primary, err := store.Get(ctx, "providers|primary")
	if err != nil {
		panic(err)
	}
	fmt.Println("primary:", primary.Status)
	completed, err := engine.Start(ctx, definition, "order_123")
	if err != nil {
		panic(err)
	}
	fmt.Println(completed.Status, completed.Output)
	// Output:
	// SUSPENDED 2
	// primary: CANCELLED
	// COMPLETED providers|secondary: approved
}
