// Package workflow implements durable, replayable workflows.
// Infrastructure instances are scoped by the caller to a single workflow run.
// Package workflow 实现持久、可回放的工作流。
// Infrastructure 实例由调用方限定到单个工作流运行。
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"
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

// NewWorkflowFromFlow is the one-step definition for a workflow that is a single
// Flow. Keys carry no extra namespace: Run("charge") is stored as "charge".
// Use NewWorkflow with RunFlow/GoFlow to compose several Flows.
// NewWorkflowFromFlow 是“整个工作流就是一个 Flow”时的一步式定义。key 不带额外命名空间：
// Run("charge") 存储为 "charge"。需要组合多个 Flow 时使用 NewWorkflow 加 RunFlow/GoFlow。
func NewWorkflowFromFlow[I, O any](name, version string, flow Flow[I, O]) Workflow[I, O] {
	if flow == nil {
		return Workflow[I, O]{Name: name, Version: version} // Drive rejects it with ErrInvalid
	}
	return NewWorkflow(name, version, func(w *WorkflowContext, input I) (O, error) {
		return w.state.runChildFlow("", flow, input)
	})
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
	Key     string
	Kind    Kind
	Status  Status
	Version int64
	// Type is the step's type path, aligned with the key path: one segment
	// per Flow/Loop/WaitAny namespace plus the leaf, e.g.
	// "flow[main.Order,string]|action[main.Order,int]". Replay identity is
	// Key + Kind + Type; Input is kept for observation. Records written
	// before Type existed have it empty and keep strict Input comparison.
	// Type 是步骤的类型路径，与 key 路径对齐：每个 Flow/Loop/WaitAny 命名空间一段，
	// 再加叶子段，例如 "flow[main.Order,string]|action[main.Order,int]"。回放身份为
	// Key + Kind + Type；Input 仅用于观测。引入 Type 之前写入的记录该字段为空，
	// 仍按 Input 严格比较。
	Type      string `json:",omitempty"`
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
	ErrChanged  = errors.New("workflow definition changed")
	// ErrWaitFailed is the terminal Drive error when every WaitAny candidate failed.
	// ErrWaitFailed 是 WaitAny 所有候选项都失败时 Drive 返回的终态错误。
	ErrWaitFailed = errors.New("wait group failed")
	// ErrLoopExhausted is returned by Loop after MaxAttempts unfinished attempts.
	// ErrLoopExhausted 在 Loop 用完 MaxAttempts 次仍未完成时返回。
	ErrLoopExhausted = errors.New("loop attempts exhausted")
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

// ActionError is the ONLY error Run/Go/Loop hand to orchestration code: a
// committed business failure. Cause is set on the executing drive and nil on
// replay, because Go error identities cannot survive serialization; use a
// domain code in Message or in your output for stable identities.
// ActionError 是 Run/Go/Loop 交给编排代码的唯一错误：一个已提交的业务失败。
// 执行时 Cause 有值，回放时为 nil，因为 Go 错误身份无法跨序列化保留；
// 如需稳定身份，请在 Message 或输出中使用领域代码。
type ActionError struct {
	Key     string
	Message string
	Cause   error
}

func (e *ActionError) Error() string { return fmt.Sprintf("%s: %s", e.Key, e.Message) }
func (e *ActionError) Unwrap() error { return e.Cause }

// fatalError marks a deterministic fault (definition drift, misuse, Flow panic,
// undecodable record). Replaying cannot fix it, so the drive reports Failed.
// fatalError 标记确定性故障（定义漂移、误用、Flow panic、无法解码的记录）。
// 重放无法修复，因此 drive 报告 Failed。
type fatalError struct{ cause error }

func (e *fatalError) Error() string { return e.cause.Error() }
func (e *fatalError) Unwrap() error { return e.cause }

func fatal(err error) error {
	var existing *fatalError
	if err == nil || errors.As(err, &existing) {
		return err
	}
	return &fatalError{cause: err}
}

// haltSignal is the private panic value that unwinds orchestration code when
// it must stop: a durable wait is pending or the drive was aborted. It is not
// an error, so it can neither be ignored with `_` nor matched with errors.As.
// haltSignal 是私有 panic 值，用于在编排必须停止时展开调用栈：存在挂起的
// 持久等待，或 drive 已中止。它不是 error，因此既不能用 `_` 忽略，也不能被
// errors.As 匹配。
type haltSignal struct{}

// strand is one sequential orchestration path: the Drive handler, or one
// GoFlow/Go goroutine. Once halted, every further primitive on it halts again,
// so recovering the signal can never let orchestration persist new progress.
// strand 是一条顺序编排路径：Drive 处理函数，或某个 GoFlow/Go goroutine。
// 一旦停止，其上的后续原语都会再次停止，因此 recover 该信号也无法让编排持久化新进度。
type strand struct{ halted atomic.Bool }

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

type Result[O any] struct {
	Output  O
	Status  Status
	Pending []Pending
}

// ActionOptions configures one Action call. Timeout bounds EACH attempt.
// ActionOptions 配置一次 Action 调用。Timeout 限制每一次尝试。
type ActionOptions struct {
	Timeout time.Duration
	Retry   RetryPolicy
}

// RetryPolicy retries retryable failures (plain errors, panics, attempt
// timeouts) inside one drive while holding the Action lease. MaxAttempts=0
// keeps drive-level retry: the first retryable failure aborts the drive and
// the scheduler re-drives later. MaxAttempts>0 commits the last failure as an
// ActionError once exhausted, so orchestration can compensate. Attempts are
// counted per drive; a crash restarts the count.
// RetryPolicy 在一次 drive 内、持有 Action 租约期间重试可重试失败（普通 error、
// panic、单次超时）。MaxAttempts=0 保持 drive 级重试：首次可重试失败即中止 drive，
// 由调度方稍后重新驱动。MaxAttempts>0 时，用尽后会把最后一次失败提交为
// ActionError，使编排可以补偿。尝试次数按 drive 计数，崩溃后重新计数。
type RetryPolicy struct {
	MaxAttempts     int
	InitialInterval time.Duration // default 100ms
	MaxInterval     time.Duration // default 10s
	Multiplier      float64       // default 2
}

func (p RetryPolicy) delay(attempt int) time.Duration {
	interval, ceiling, multiplier := p.InitialInterval, p.MaxInterval, p.Multiplier
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	if ceiling <= 0 {
		ceiling = 10 * time.Second
	}
	if multiplier < 1 {
		multiplier = 2
	}
	for i := 1; i < attempt && interval < ceiling; i++ {
		// Compare in float64: converting an out-of-range float to Duration is
		// platform-defined and may turn negative (a zero-delay retry storm).
		// 在 float64 中比较：超出范围的 float 转为 Duration 依平台而定，可能变为负数（零延迟重试风暴）。
		next := float64(interval) * multiplier
		if next >= float64(ceiling) {
			return ceiling
		}
		interval = time.Duration(next)
	}
	return min(interval, ceiling)
}
