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

// Flow composes Actions and signals using a context without Flow-call methods.
// Flow 使用不含跨流程调用方法的上下文，组合 Action 和信号。
type Flow[I, O any] interface {
	Run(*FlowContext, I) (O, error)
}

type FlowFunc[I, O any] func(*FlowContext, I) (O, error)

func (f FlowFunc[I, O]) Run(ctx *FlowContext, input I) (O, error) { return f(ctx, input) }

// Workflow is a named top-level definition whose entry is invoked by Engine.
// Construct it with NewWorkflow; it does not implement Flow or expose Run.
// Workflow 是由引擎调用的具名顶层定义，使用 NewWorkflow 构造；
// 它不实现 Flow，也不公开 Run 方法。
type Workflow[I, O any] struct {
	Name    string
	Version string
	run     func(*WorkflowContext, I) (O, error)
}

// NewWorkflow defines orchestration with access only to Flow-call primitives.
// Engine.Drive validates the name, version and handler before executing it.
// NewWorkflow 定义顶层编排，仅提供 Flow 调用原语；
// Engine.Drive 会在执行前校验名称、版本和处理函数。
func NewWorkflow[I, O any](name, version string, handler func(*WorkflowContext, I) (O, error)) Workflow[I, O] {
	return Workflow[I, O]{Name: name, Version: version, run: handler}
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
