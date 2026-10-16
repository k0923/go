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
		return order, workflow.Permanent(errors.New("amount must be positive"))
	}
	return order, nil
}

func charge(ctx context.Context, order Order) (string, error) {
	if err := pause(ctx, 700*time.Millisecond); err != nil {
		return "", err
	}
	// ActionKey is a stable idempotency key for the payment provider.
	// ActionKey 是支付提供方使用的稳定幂等键。
	return "pay_" + workflow.ActionKey(ctx), nil
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

var prepareFlow = workflow.FlowFunc[Order, Summary](func(f *workflow.FlowContext, order Order) (Summary, error) {
	summary := Summary{OrderID: order.ID}
	if _, err := f.Run("validate", workflow.AsAction(validate), order); err != nil {
		return summary, err
	}
	approval, err := f.WaitFor[Approval]("approval", map[string]any{
		"order": order, "question": "Approve this order?",
	})
	if err != nil {
		return summary, err
	}
	summary.Approval = approval
	if !approval.Approved {
		return summary, workflow.Permanent(fmt.Errorf("rejected by %s: %s", approval.By, approval.Reason))
	}
	if summary.Payment, err = f.Run("charge", workflow.AsAction(charge), order); err != nil {
		return summary, err
	}
	return summary, nil
})

func orchestrateOrder(w *workflow.WorkflowContext, order Order) (Summary, error) {
	summary, err := w.RunFlow("prepare", prepareFlow, order)
	if err != nil {
		return summary, err
	}
	// Shipment polling and customer notification progress concurrently.
	// 物流轮询和客户通知会并发推进。
	notice := w.GoFlow("notify", workflow.FlowFunc[string, string](func(f *workflow.FlowContext, id string) (string, error) {
		// Acks are typed in by hand in the debugger, so accept any JSON shape.
		// ack 是在 debugger 中手动输入的，因此接受任意 JSON 形状。
		winner, err := f.WaitAny[json.RawMessage]("channel",
			workflow.WaitForSignal("email_ack", map[string]string{"order": id, "channel": "email"}),
			workflow.WaitForSignal("sms_ack", map[string]string{"order": id, "channel": "sms"}),
			workflow.SleepSignal("give_up", 2*time.Minute),
		)
		if err != nil {
			return "", err
		}
		if winner.Kind == workflow.SleepKind {
			return "no acknowledgement", nil
		}
		return winner.Key + ": " + string(winner.Response), nil
	}), order.ID)

	shipping := workflow.FlowFunc[string, Shipment](func(f *workflow.FlowContext, id string) (Shipment, error) {
		return f.Loop("ship", workflow.AsAction(pollShipment), id, workflow.LoopOptions{
			Interval: 15 * time.Second, MaxAttempts: 5,
		})
	})
	if summary.Shipment, err = w.RunFlow("shipping", shipping, order.ID); err != nil {
		return summary, err
	}
	if summary.Notice, err = notice.Wait(); err != nil {
		return summary, err
	}
	cooldown := workflow.FlowFunc[Summary, Summary](func(f *workflow.FlowContext, summary Summary) (Summary, error) {
		return summary, f.Sleep("cooldown", 5*time.Second)
	})
	return w.RunFlow("finish", cooldown, summary)
}

func main() {
	server := flag.String("server", debugger.DefaultServer, "debugger server URL")
	run := flag.String("run", "", "run ID; reuse it to resume after restarting this process")
	amount := flag.Float64("amount", 128, "order amount (<=0 triggers a permanent failure)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	definition := workflow.NewWorkflow("order", "v2", orchestrateOrder)
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
