// Command fulfillment exports a larger state machine with branches and retries.
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/k0923/go/fsm"
)

type State string

func (State) FSMState() {}

const (
	Created   State = "created"
	Checking  State = "checking"
	Review    State = "review"
	Payment   State = "payment"
	Paid      State = "paid"
	Packing   State = "packing"
	Shipping  State = "shipping"
	Retry     State = "retry"
	Completed State = "completed"
	Cancelled State = "cancelled"
)

func main() {
	elk := flag.Bool("elk", false, "使用 ELK 排版，需要渲染环境支持")
	flag.Parse()
	b := fsm.NewBuilder[State, struct{}]()
	b.State(Created, "创建订单").State(Checking, "风险检查").
		State(Review, "人工审核").State(Payment, "等待支付").
		State(Paid, "支付完成").State(Packing, "打包出库").
		State(Shipping, "配送中").State(Retry, "等待重试").
		State(Completed, "订单完成").State(Cancelled, "订单取消")

	// 为每个事件保留独立身份；图从以下真实转换规则生成。
	b.On(fsm.NewEvent[State, struct{}]("submit", "提交")).From(Created).To(Checking)
	b.On(fsm.NewEvent[State, struct{}]("pass", "检查通过")).From(Checking).To(Payment)
	b.On(fsm.NewEvent[State, struct{}]("review", "需要审核")).From(Checking).To(Review)
	b.On(fsm.NewEvent[State, struct{}]("approve", "审核通过")).From(Review).To(Payment)
	b.On(fsm.NewEvent[State, struct{}]("reject", "审核拒绝")).From(Review).To(Cancelled)
	b.On(fsm.NewEvent[State, struct{}]("pay", "支付成功")).From(Payment).To(Paid)
	b.On(fsm.NewEvent[State, struct{}]("pack", "开始备货")).From(Paid).To(Packing)
	b.On(fsm.NewEvent[State, struct{}]("ship", "交付物流")).From(Packing).To(Shipping)
	b.On(fsm.NewEvent[State, struct{}]("fail", "配送失败")).From(Shipping).To(Retry)
	b.On(fsm.NewEvent[State, struct{}]("retry", "重新配送")).From(Retry).To(Shipping)
	b.On(fsm.NewEvent[State, struct{}]("deliver", "确认收货")).From(Shipping).To(Completed)
	b.On(fsm.NewEvent[State, struct{}]("cancel", "主动取消")).From(Created, Payment).To(Cancelled)
	d, err := b.Build()
	if err != nil {
		log.Fatal(err)
	}
	layout := fsm.LayoutDagre
	if *elk {
		layout = fsm.LayoutELK
	}
	fmt.Print(d.Mermaid(fsm.MermaidOptions[State]{
		Direction: fsm.TopToBottom, Layout: layout,
		NodeSpacing: 65, RankSpacing: 75, LabelWidth: 20,
		// 精简标签保留全部节点和转换，减少复杂图上的文字密度。
		Compact: true,
		StateStyles: map[State]fsm.MermaidNodeStyle{
			Review:    {Fill: "#fffbeb", Stroke: "#d97706", Color: "#92400e"},
			Retry:     {Fill: "#fff7ed", Stroke: "#ea580c", Color: "#9a3412"},
			Completed: {Fill: "#ecfdf5", Stroke: "#059669", Color: "#065f46"},
			Cancelled: {Fill: "#f1f5f9", Stroke: "#94a3b8", Color: "#475569"},
		},
	}))
}
