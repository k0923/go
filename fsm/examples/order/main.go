package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"sync"

	"github.com/k0923/go/fsm"
)

// OrderState 是订单专属类型，其他业务应定义独立的命名类型。
type OrderState string

// FSMState 显式声明该类型可以用于状态机。
func (OrderState) FSMState() {}

const (
	Pending   OrderState = "pending"
	Paid      OrderState = "paid"
	Cancelled OrderState = "cancelled"
)

type Order struct {
	ID            string
	Amount        int64
	TransactionID string
	CancelReason  string
}

type Payment struct {
	TransactionID string
	Amount        int64
}

type Cancellation struct {
	Reason string
}

// 描述参数可省略；参数类型仍然由各个事件独立约束。
var (
	Pay    = fsm.NewEvent[OrderState, Payment]("pay", "支付订单")
	Cancel = fsm.NewEvent[OrderState, Cancellation]("cancel", "取消订单")
)

// definition 全局构建一次，所有订单共享，不持有任何单个订单的状态。
func definition() (*fsm.Definition[OrderState, *Order], error) {
	b := fsm.NewBuilder[OrderState, *Order]()

	// 显式声明可添加描述，也可声明没有任何转换的孤立状态。
	// 如果不需要描述，From/To 会自动注册引用到的状态。
	b.State(Pending, "待支付").
		State(Paid, "已支付").
		State(Cancelled, "已取消")

	b.On(Pay).
		From(Pending).
		To(Paid).
		Description("支付金额必须等于订单金额").
		Guard(func(_ context.Context, order *Order, p Payment) (bool, error) {
			// Guard 只读判断，拒绝后不会执行 Action，也不会推进状态。
			return p.Amount == order.Amount, nil
		}).
		Action(func(_ context.Context, order *Order, p Payment) error {
			// Action 执行业务副作用；实际业务可在此调用服务。
			order.TransactionID = p.TransactionID
			return nil
		})

	b.On(Cancel).
		From(Pending, Paid). // 一个规则可以声明多个来源状态。
		To(Cancelled).
		Description("记录取消原因").
		Action(func(_ context.Context, order *Order, p Cancellation) error {
			order.CancelReason = p.Reason
			return nil
		})

	// Build 集中校验配置，并返回可供所有订单共享的不可变定义。
	return b.Build()
}

// repository 模拟持久化：订单状态的权威来源在这里，而不在状态机中。
// CompareAndSwapState 以“期望的旧状态”做乐观并发控制。
type repository struct {
	mu     sync.Mutex
	states map[string]OrderState
}

func newRepository() *repository {
	return &repository{states: map[string]OrderState{}}
}

func (r *repository) load(id string) OrderState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states[id]
}

var errStaleState = errors.New("order state changed concurrently")

func (r *repository) compareAndSwapState(id string, expected, next OrderState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.states[id] != expected {
		return errStaleState
	}
	r.states[id] = next
	return nil
}

func run(diagram bool) error {
	d, err := definition()
	if err != nil {
		return err
	}
	if diagram {
		// 只输出 Mermaid 文本，可以重定向到文件或粘贴到渲染器。
		// 不传配置的 d.Mermaid() 使用默认蓝色样式和完整标签。
		fmt.Print(d.Mermaid(fsm.MermaidOptions[OrderState]{
			Direction:     fsm.TopToBottom,
			NodeSpacing:   60,
			RankSpacing:   80,
			LabelWidth:    24,
			HideCallbacks: true,
			StateStyles: map[OrderState]fsm.MermaidNodeStyle{
				Pending:   {Fill: "#fffbeb", Stroke: "#d97706", Color: "#92400e"},
				Paid:      {Fill: "#ecfdf5", Stroke: "#059669", Color: "#065f46"},
				Cancelled: {Fill: "#f1f5f9", Stroke: "#94a3b8", Color: "#475569"},
			},
		}))
		return nil
	}

	ctx := context.Background()
	repo := newRepository()
	order := &Order{ID: "order-1", Amount: 100}
	repo.states[order.ID] = Pending // 初始状态存入“数据库”。

	// advance 展示无状态用法的完整回合：读当前状态 → Fire 计算 → CAS 写回。
	advance := func(event func(context.Context, OrderState, *Order) (OrderState, error)) (OrderState, error) {
		current := repo.load(order.ID)
		next, err := event(ctx, current, order)
		if err != nil {
			return current, err
		}
		if err := repo.compareAndSwapState(order.ID, current, next); err != nil {
			return current, err
		}
		return next, nil
	}

	payWith := func(amount int64) func(context.Context, OrderState, *Order) (OrderState, error) {
		return func(ctx context.Context, from OrderState, o *Order) (OrderState, error) {
			return d.Fire(ctx, from, o, Pay, Payment{TransactionID: "tx-001", Amount: amount})
		}
	}
	cancelWith := func(reason string) func(context.Context, OrderState, *Order) (OrderState, error) {
		return func(ctx context.Context, from OrderState, o *Order) (OrderState, error) {
			return d.Fire(ctx, from, o, Cancel, Cancellation{Reason: reason})
		}
	}

	fmt.Println("初始状态:", repo.load(order.ID))

	// 金额不符，演示可通过 errors.Is 识别的拒绝；状态不会被写回。
	if _, err := advance(payWith(50)); !errors.Is(err, fsm.ErrGuardRejected) {
		return fmt.Errorf("预期金额校验拒绝，实际结果: %v", err)
	}
	fmt.Println("金额校验拒绝，状态:", repo.load(order.ID))

	// Pay 的类型保证这里传入的是 Payment。
	if _, err := advance(payWith(100)); err != nil {
		return err
	}
	fmt.Printf("支付成功，状态: %s，流水号: %s\n", repo.load(order.ID), order.TransactionID)

	if _, err := advance(cancelWith("用户取消")); err != nil {
		return err
	}
	fmt.Printf("取消成功，状态: %s，原因: %s\n", repo.load(order.ID), order.CancelReason)

	// 用过期的旧状态再触发一次 Pay：Fire 本身成功，但 CAS 发现状态已变而拒绝写回，
	// 演示并发控制由存储层而非状态机负责。
	if _, err := d.Fire(ctx, Pending, order, Pay, Payment{Amount: 100}); err != nil {
		return err
	}
	if err := repo.compareAndSwapState(order.ID, Pending, Paid); !errors.Is(err, errStaleState) {
		return fmt.Errorf("预期 CAS 冲突，实际结果: %v", err)
	}
	fmt.Println("过期状态写回被拒绝，最终状态:", repo.load(order.ID))
	return nil
}

func main() {
	diagram := flag.Bool("diagram", false, "只输出状态机的 Mermaid 图")
	flag.Parse()
	if err := run(*diagram); err != nil {
		log.Fatal(err)
	}
}
