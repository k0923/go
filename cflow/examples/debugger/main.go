// Demo worker for the workflow debugger. Start the server first:
//
//	go run ./workflow/cmd/wfdebug
//	go run ./workflow/examples/debugger -run order-1
//
// Open http://127.0.0.1:7788, answer the "approval" WaitFor with a JSON such
// as {"approved": true, "by": "alice"}, or fire the Sleep timers early.
// Start several workers with different -run values to debug them side by side.
// workflow debugger 的演示 worker。请先启动服务：
//
//	go run ./workflow/cmd/wfdebug
//	go run ./workflow/examples/debugger -run order-1
//
// 打开 http://127.0.0.1:7788，用类似 {"approved": true, "by": "alice"} 的 JSON
// 回答 "approval" WaitFor，或提前触发 Sleep 计时器。使用不同 -run 值启动多个 worker，
// 可以并排调试它们。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/google/uuid"
	"github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/debugger"
)

type Order struct {
	ID     string  `json:"id"`
	Amount float64 `json:"amount"`
}

type Approval struct {
	Approved bool   `json:"approved"`
	By       string `json:"by"`
	Reason   string `json:"reason,omitempty"`
}

type Shipment struct {
	Attempt  int    `json:"attempt"`
	State    string `json:"state"`
	Finished bool   `json:"finished"`
}

func (s Shipment) Done() bool { return s.Finished }

type Summary struct {
	OrderID  string   `json:"order_id"`
	Approval Approval `json:"approval"`
	Payment  string   `json:"payment"`
	Shipment Shipment `json:"shipment"`
	Notice   string   `json:"notice"`
}

func validate(ctx context.Context, order Order) (Order, error) {
	if err := pause(ctx, 400*time.Millisecond); err != nil {
		return order, err
	}
	if order.Amount <= 0 {
		return order, cflow.Permanent(errors.New("amount must be positive"))
	}
	return order, nil
}

func charge(ctx context.Context, order Order) (string, error) {
	if err := pause(ctx, 700*time.Millisecond); err != nil {
		return "", err
	}
	// ActionKey is a stable idempotency key for the payment provider.
	// ActionKey 是支付提供方使用的稳定幂等键。
	return "pay_" + cflow.ActionKey(ctx), nil
}

var polls int

func pollShipment(ctx context.Context, orderID string) (Shipment, error) {
	if err := pause(ctx, 300*time.Millisecond); err != nil {
		return Shipment{}, err
	}
	polls++
	states := []string{"packing", "in_transit", "delivered"}
	state := states[min(polls-1, len(states)-1)]
	return Shipment{Attempt: polls, State: state, Finished: state == "delivered"}, nil
}

func pause(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

var prepareFlow = cflow.FlowFunc[Order, Summary](func(f *cflow.FlowContext, order Order) (Summary, error) {
	summary := Summary{OrderID: order.ID}
	if _, err := f.Run("validate", cflow.AsAction(validate), order); err != nil {
		return summary, err
	}
	// No error to check: until the UI answers, WaitFor halts this Flow.
	// 无需检查错误：在 UI 回答之前，WaitFor 会停止此 Flow。
	approval := f.WaitFor[Approval]("approval", map[string]any{
		"order": order, "question": "Approve this order?",
	})
	summary.Approval = approval
	if !approval.Approved {
		// A returned error is terminal; no Permanent wrapper is needed in Flow code.
		// 返回的 error 即为终态；Flow 代码中无需包装 Permanent。
		return summary, fmt.Errorf("rejected by %s: %s", approval.By, approval.Reason)
	}
	payment, err := f.Run("charge", cflow.AsAction(charge), order)
	summary.Payment = payment
	return summary, err
})

func orchestrateOrder(root *cflow.FlowContext, order Order) (Summary, error) {
	summary, err := root.RunFlow("prepare", prepareFlow, order)
	if err != nil {
		return summary, err
	}
	// Shipment polling and customer notification progress concurrently.
	// 物流轮询和客户通知会并发推进。
	notice := root.GoFlow("notify", cflow.FlowFunc[string, string](func(f *cflow.FlowContext, id string) (string, error) {
		// Acks are typed in by hand in the debugger, so accept any JSON shape.
		// ack 是在 debugger 中手动输入的，因此接受任意 JSON 形状。
		email := cflow.OnSignal[json.RawMessage]("email_ack", map[string]string{"order": id, "channel": "email"})
		sms := cflow.OnSignal[json.RawMessage]("sms_ack", map[string]string{"order": id, "channel": "sms"})
		winner := f.WaitAny("channel", email, sms, cflow.OnSleep("give_up", 2*time.Minute))
		switch winner {
		case email:
			return winner.Key() + ": " + string(email.Value()), nil
		case sms:
			return winner.Key() + ": " + string(sms.Value()), nil
		}
		return "no acknowledgement", nil
	}), order.ID)

	shipping := cflow.FlowFunc[string, Shipment](func(f *cflow.FlowContext, id string) (Shipment, error) {
		return f.Loop("ship", cflow.AsAction(pollShipment), id, cflow.LoopOptions{
			Interval: 15 * time.Second, MaxAttempts: 5,
		})
	})
	if summary.Shipment, err = root.RunFlow("shipping", shipping, order.ID); err != nil {
		return summary, err
	}
	if summary.Notice, err = notice.Wait(); err != nil {
		return summary, err
	}
	cooldown := cflow.FlowFunc[Summary, Summary](func(f *cflow.FlowContext, summary Summary) (Summary, error) {
		f.Sleep("cooldown", 5*time.Second)
		return summary, nil
	})
	return root.RunFlow("finish", cooldown, summary)
}

func main() {
	server := flag.String("server", debugger.DefaultServer, "debugger server URL")
	run := flag.String("run", "", "run ID; reuse it to resume after restarting this process")
	amount := flag.Float64("amount", 128, "order amount (<=0 triggers a permanent failure)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	definition := cflow.NewWorkflow("order", "v2", cflow.FlowFunc[Order, Summary](orchestrateOrder))
	id := *run
	if id == "" {
		id = "order-" + uuid.NewString()
	}
	result, err := debugger.Run(ctx, definition, Order{ID: id, Amount: *amount}, debugger.Options{
		Server: *server, RunID: id,
	})
	if err != nil {
		log.Fatalf("workflow ended: %v", err)
	}
	log.Printf("result: %+v", result.Output)
}
