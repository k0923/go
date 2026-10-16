// Package workflow implements durable, replayable workflows.
// Infrastructure instances are scoped by the caller to a single workflow run.
// Package workflow 实现持久、可回放的工作流。
// Infrastructure 实例由调用方限定到单个工作流运行。
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/k0923/go/lease"
)

// Action is an ordinary callable; it has no workflow scheduling capabilities.
// An IO action is not mathematically pure. Its external effects must be idempotent.
// Action 是普通可调用对象；它不具备工作流调度能力。
// IO action 在数学意义上并非纯函数，其外部副作用必须具备幂等性。
type Action[I, O any] func(context.Context, I) (O, error)

// Pure adapts a deterministic computation to an Action.
// Pure 将确定性计算适配为 Action。
func Pure[I, O any](fn func(I) O) Action[I, O] {
	return func(_ context.Context, input I) (O, error) { return fn(input), nil }
}

type Flow[I, O any] interface {
	Run(*FlowContext, I) (O, error)
}

type FlowFunc[I, O any] func(*FlowContext, I) (O, error)

func (f FlowFunc[I, O]) Run(ctx *FlowContext, input I) (O, error) { return f(ctx, input) }

// Workflow names and versions the root definition; Flow is reusable orchestration.
// Workflow 为根定义命名并标记版本；Flow 是可复用的编排。
type Workflow[I, O any] struct {
	Name    string
	Version string
	Flow    Flow[I, O]
}

type Status string

const (
	Running   Status = "RUNNING"
	Completed Status = "COMPLETED"
	Failed    Status = "FAILED"
	Suspended Status = "SUSPENDED"
	// Cancelled is terminal for losing members of a WaitAny group.
	// Cancelled 是 WaitAny 组中失败成员的终态。
	Cancelled Status = "CANCELLED"
)

type Kind string

const (
	ActionKind     Kind = "ACTION"
	CheckpointKind Kind = "CHECKPOINT"
	SignalKind     Kind = "SIGNAL"
	SleepKind      Kind = "SLEEP"
	DefinitionKind Kind = "DEFINITION"
)

// Record is a versioned snapshot, not an append-only execution history.
// Record 是带版本的快照，而不是仅追加的执行历史。
type Record struct {
	Key       string
	Kind      Kind
	Status    Status
	Version   int64
	Input     json.RawMessage
	Result    json.RawMessage
	Error     string
	Until     time.Time
	StartedAt time.Time
	EndedAt   time.Time
}

// Lease is the shared acquisition token; kept as an alias for store adapters.
// Lease 是共享的获取令牌；保留为别名以适配存储适配器。
type Lease = lease.Token

var (
	ErrConflict = lease.ErrConflict
	ErrNotFound = errors.New("workflow record not found")
	ErrInvalid  = errors.New("invalid workflow operation")
	ErrChanged  = errors.New("workflow definition or input changed")
)

type OperationError struct {
	Key   string
	Op    string
	Cause error
}

func (e *OperationError) Error() string { return fmt.Sprintf("%s %s: %v", e.Op, e.Key, e.Cause) }
func (e *OperationError) Unwrap() error { return e.Cause }

// Permanent marks a domain failure that must be persisted and never retried.
// Permanent 标记必须持久化且永不重试的领域失败。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Cause: err}
}

type PermanentError struct{ Cause error }

func (e *PermanentError) Error() string { return e.Cause.Error() }
func (e *PermanentError) Unwrap() error { return e.Cause }

// ReplayError preserves the persisted message. Go error identities cannot
// survive serialization; use a domain code in your output for stable identities.
// ReplayError 保留已持久化的消息。Go 错误身份无法跨序列化保留；
// 如需稳定身份，请在输出中使用领域代码。
type ReplayError struct{ Key, Message string }

func (e *ReplayError) Error() string { return fmt.Sprintf("%s: %s", e.Key, e.Message) }

type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("workflow panic: %v", e.Value) }
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

func invoke[T any](fn func() (T, error)) (output T, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = &PanicError{Value: value, Stack: debug.Stack()}
		}
	}()
	return fn()
}

type Pending struct {
	Key   string
	Kind  Kind
	Until time.Time
}

type SuspendedError struct{ Pending Pending }

func (e *SuspendedError) Error() string { return "workflow suspended: " + e.Pending.Key }

type Result[O any] struct {
	Output  O
	Status  Status
	Pending []Pending
}

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

type Observation struct {
	Key      string
	Phase    string
	At       time.Time
	Duration time.Duration
	Err      error
}

// Observer may be invoked concurrently. It must return promptly.
// Observer 可能被并发调用，必须快速返回。
type Observer func(context.Context, Observation)

type ActionOptions struct{ Timeout time.Duration }
