package memory

import (
	"context"
	"time"
)

// Notification is durable delivery intent. Request and Wake are separate
// messages, so acknowledging a request cannot consume a subsequent wakeup.
// Notification 是持久化的投递意图。Request 和 Wake 是不同消息，
// 因此确认一个请求不会消费后续唤醒。
type Notification struct {
	ID          string
	Key         string
	Purpose     string
	AvailableAt time.Time
}

const (
	RequestNotification = "REQUEST"
	WakeNotification    = "WAKE"
	// CancelNotification tells an external provider to stop a losing wait.
	// CancelNotification 告诉外部提供方停止一个失败的等待。
	CancelNotification = "CANCEL"
)

// Outbox is the reference delivery contract of this package: the durable
// outbox a dispatcher owned by the caller polls. The engine never calls it;
// your own store may expose delivery however it likes. Pending returns
// messages with AvailableAt <= now. Delivery is at-least-once, so every
// handler must tolerate duplicates:
//
//   - REQUEST: deliver the record's Input to the provider, then Ack. The
//     provider answers with Engine.Signal / ResolveSignal / FailWait.
//   - WAKE: Drive the run. Ack only when the result is not Running; on a
//     retryable error keep the message and retry with backoff.
//   - CANCEL: tell the provider to stop the losing wait, then Ack.
//
// Duplicate WAKEs only replay; late answers return false. Both are safe.
// Outbox 是本包的参考投递契约：由调用方的调度器轮询的持久化发件箱。引擎从不调用它；
// 自定义存储可以用任意方式暴露投递。Pending 返回 AvailableAt <= now 的消息。
// 投递是至少一次，因此每个处理方都必须容忍重复：
//
//   - REQUEST：把记录的 Input 投递给提供方，然后 Ack。提供方通过
//     Engine.Signal / ResolveSignal / FailWait 回答。
//   - WAKE：Drive 该 run。只有结果不是 Running 时才 Ack；遇到可重试错误时保留消息，
//     退避后重试。
//   - CANCEL：通知提供方停止失败的等待，然后 Ack。
//
// 重复的 WAKE 只会重放；迟到的回答返回 false。两者都是安全的。
type Outbox interface {
	Pending(context.Context, time.Time) ([]Notification, error)
	Ack(context.Context, string) error
}
