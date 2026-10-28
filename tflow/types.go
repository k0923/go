package tflow

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"time"
)

var (
	ErrInvalid               = errors.New("invalid tflow argument")
	ErrInvalidPlan           = errors.New("invalid subtask plan")
	ErrReconcileLimit        = errors.New("reconcile pass limit reached")
	ErrNotFound              = errors.New("task not found")
	ErrAlreadyExists         = errors.New("task already exists")
	ErrConflict              = errors.New("concurrent state conflict")
	ErrAlreadyTerminal       = errors.New("task already terminal")
	ErrStaleAttempt          = errors.New("stale subtask attempt")
	ErrNoSubTaskToRerun      = errors.New("no subtask to rerun")
	ErrDeliveryLeaseLost     = errors.New("delivery lease lost")
	ErrDispatcherUnavailable = errors.New("dispatcher unavailable")
	ErrExecutorUnavailable   = errors.New("executor unavailable")
	ErrResumeUnsupported     = errors.New("executor does not support resume")
	ErrCancelUnsupported     = errors.New("executor does not support cancellation")
)

type MainStatus string

const (
	MainRunning   MainStatus = "RUNNING"
	MainSucceeded MainStatus = "SUCCEEDED"
	MainFailed    MainStatus = "FAILED"
	MainCanceled  MainStatus = "CANCELED"
)

func (s MainStatus) Terminal() bool {
	return s == MainSucceeded || s == MainFailed || s == MainCanceled
}

type SubStatus string

const (
	SubPending   SubStatus = "PENDING"
	SubRunning   SubStatus = "RUNNING"
	SubSucceeded SubStatus = "SUCCEEDED"
	SubFailed    SubStatus = "FAILED"
	SubCanceled  SubStatus = "CANCELED"
)

func (s SubStatus) Terminal() bool {
	return s == SubSucceeded || s == SubFailed || s == SubCanceled
}

// SubmitOptions configures a new MAIN_TASK.
// SubmitOptions 用来创建一个新的 MAIN_TASK。
type SubmitOptions struct {
	ConcurrencyLimit int64
	// Nil uses the Engine default. Store persists the chosen immutable policy.
	PlanningPolicy PlanningPolicy
	// Nil uses the Engine default. The selected immutable decider is persisted.
	MainDecider MainDecider
}

// MainConfig is persisted once for a logical main task and reused on recovery.
type MainConfig struct {
	ConcurrencyLimit int64
	PlanningPolicy   PlanningPolicy
	MainDecider      MainDecider
}

// MainState contains scheduling state only. Its identity comes from Store.
type MainState struct {
	Status MainStatus
	// DispatchCursor counts materialized units, not source pages or offsets.
	DispatchCursor    int64
	DispatchExhausted bool
	Version           int64
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// SubSummary is a Store-owned aggregation of current subtask states.
// Running includes queued starts and waiting executions. Readers must receive
// it from the same snapshot as MainState; never maintain it in application code.
type SubSummary struct {
	Pending   int64
	Running   int64
	Succeeded int64
	Failed    int64
	Canceled  int64
}

func (s SubSummary) TerminalCount() int64 { return s.Succeeded + s.Failed + s.Canceled }

// MainProgress combines durable main state with its consistent subtask summary.
// The summary is derived by Store and is not part of SaveMain's input.
type MainProgress struct {
	MainState
	Subs SubSummary
}

func (m MainProgress) State() MainState    { return m.MainState }
func (m MainProgress) Summary() SubSummary { return m.Subs }

// MainRecord is an atomic Store read of business progress, config and state.
// State-only updates cannot replace its business Task or persistent config.
type MainRecord struct {
	Task MainTask
	MainConfig
	MainProgress
}

func (m MainProgress) ReadyToComplete() bool {
	return m.Status == MainRunning && m.DispatchExhausted &&
		m.Subs.Pending == 0 && m.Subs.Running == 0
}

// SubState uses a key local to its bound Store; Attempt isolates reruns.
type SubState struct {
	Key         string
	Status      SubStatus
	Cursor      int64
	Attempt     int64
	Version     int64
	ExternalRef string
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type SubRecord struct {
	Task SubTask
	SubState
}

// Execution identifies one concrete SUB_TASK attempt.
// Execution 标识某个 SUB_TASK 的一次具体执行，用于外部幂等和回调隔离。
type Execution struct {
	MainID      string
	SubKey      string
	Attempt     int64
	ExternalRef string
}

func (s SubState) Execution(mainID string) Execution {
	return Execution{MainID: mainID, SubKey: s.Key, Attempt: s.Attempt, ExternalRef: s.ExternalRef}
}

// Key is a stable external idempotency key, unchanged across delivery retries.
// Key 是稳定的外部幂等键；同一执行重投时保持不变。
func (r Execution) Key() string {
	return fmt.Sprintf("%s/%s/%d", encode(r.MainID), encode(r.SubKey), r.Attempt)
}

func encode(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// PlanRequest is the framework-owned request passed to MainTask.Plan.
// PlanRequest 是框架传给业务 Plan 的请求，目前只表达本次最多生成多少子任务。
type PlanRequest interface{ Limit() int }

// MainTask owns business configuration and pagination state. Plan must not
// mutate committed state. The Store commits business progress from the Plan.
// MainTask 由业务实现，持有配置和分页进度。Plan 只生成计划，
// 分页进度由 Store 从业务 Plan 中提取并原子提交。
type MainTask interface {
	Plan(context.Context, PlanRequest) (Plan, error)
}

// SubTask is a business execution unit under one MAIN_TASK.
// SubTask 是 MAIN_TASK 下的业务执行单元；框架只需要稳定 Key。
type SubTask interface{ Key() string }

// Plan is an opaque business planning result with pure, stable protocol methods.
// Its concrete type may include source progress understood only by its adapter.
// Plan 是业务规划结果；框架只读取子任务及是否还有下一批，
// 业务分页信息由具体 Plan 类型和 Store 适配器处理。
type Plan interface {
	Tasks() []SubTask
	HasMore() bool
}

// Page is a convenience Plan for tasks without adapter-specific progress.
// Stateful pagination should keep a business Plan understood by CommitPlan.
type Page struct {
	Items []SubTask
	More  bool
}

func (p Page) Tasks() []SubTask { return p.Items }
func (p Page) HasMore() bool    { return p.More }

// Event is an application-owned resume signal.
// Event 是业务自定义的恢复信号，Key 用于恢复指令的幂等。
type Event interface{ Key() string }

type DispatchStatus string

const (
	Accepted           DispatchStatus = "ACCEPTED"
	Waiting            DispatchStatus = "WAITING"
	ExecutionSucceeded DispatchStatus = "SUCCEEDED"
	ExecutionFailed    DispatchStatus = "FAILED"
)

type DispatchResult struct {
	Status      DispatchStatus
	ExternalRef string
	Error       string
}

// Executor implementations must use Execution.Key() for idempotency.
// Accepted/Waiting is a durable handoff: the implementation has persisted the
// request or transferred it to a reliable runtime, and owns subsequent recovery.
// Merely starting a goroutine is not a durable acceptance. Start/Resume errors
// retry delivery; terminal business failure uses ExecutionFailed. Permanent is
// a planning classification and does not change Executor error semantics.
// Executor 由业务实现，用来启动、恢复或取消 SUB_TASK 内部执行。
// Accepted/Waiting 表示业务方已经可靠接管，不能只是启动了一个临时 goroutine。
// Start/Resume 返回 error 表示接入失败需重试，业务终态失败用 ExecutionFailed 表达。
type Executor interface {
	Start(context.Context, SubTask, Execution) (DispatchResult, error)
	Resume(context.Context, SubTask, Execution, Event) (DispatchResult, error)
	Cancel(context.Context, SubTask, Execution) error
}

type CommandKind string

const (
	StartSub       CommandKind = "START_SUB"
	ResumeSub      CommandKind = "RESUME_SUB"
	CancelSub      CommandKind = "CANCEL_SUB"
	ReconcileMain  CommandKind = "RECONCILE_MAIN"
	MainTerminated CommandKind = "MAIN_TERMINATED"
)

// Command is durable work intent, not a third task layer. It represents a start,
// resume, cancel, reconciliation, or terminal notification. Cancel carries the
// historical attempt, and MainTerminated carries the historical main snapshot.
// Command 是持久化的待办指令，不是第三层任务。它表达启动、恢复、取消、
// 调度修复或主任务终态通知；取消携带历史执行，终态通知携带历史主任务快照。
type Command struct {
	ID        string
	MainID    string
	Main      MainRecord
	Kind      CommandKind
	Sub       SubRecord
	Event     Event
	EventKey  string
	NotBefore time.Time
}

func NewCommand(kind CommandKind, mainID string, sub SubRecord, event Event) Command {
	key := string(kind) + "/" + sub.Execution(mainID).Key()
	eventKey := ""
	if kind == ResumeSub {
		if !IsNil(event) {
			eventKey = event.Key()
		}
		key += "/" + encode(eventKey)
	}
	if kind == CancelSub {
		key += "/" + encode(sub.ExternalRef)
	}
	return Command{ID: key, MainID: mainID, Kind: kind, Sub: sub, Event: event, EventKey: eventKey}
}

func ReconcileCommand(mainID string, m MainState) Command {
	return Command{
		ID:     fmt.Sprintf("reconcile/%s/%d", encode(mainID), m.Version),
		MainID: mainID, Kind: ReconcileMain,
	}
}

func TerminationCommand(mainID string, m MainRecord) Command {
	return Command{
		ID:     fmt.Sprintf("terminated/%s/%d", encode(mainID), m.Version),
		MainID: mainID, Kind: MainTerminated, Main: m,
	}
}

// MainTermination identifies an immutable historical terminal transition.
// ID remains stable across delivery retries, and changes for a later rerun's
// terminal transition. Main is never replaced with the main's latest snapshot.
// Handle delivers it to the selected handler after validating the bound Command.
// MainTermination 表示一次不可变的历史终态迁移。重试投递时 ID 不变；
// 重跑后的下一次终态会生成新的 ID，Main 不会被替换成最新快照。
type MainTermination struct {
	ID   string
	Main MainProgress
}

// TerminationHandler is an optional capability of a persisted MainTask.
// Store adapters must preserve its routing identity in historical command tasks
// and reconstruct its dependencies when reading them. OnTerminated must reliably
// enqueue/record a wake by event.ID before returning nil; delivery is at least once.
// It takes precedence over Options.TerminationHandler and runs outside transactions.
// Both handlers use this interface and must be idempotent by event.ID.
// TerminationHandler 由业务 MainTask 可选实现。路由身份随任务持久保存，
// Store 读取历史终态命令时重建通知依赖；新 Engine 无需重新注册实例回调。
type TerminationHandler interface {
	OnTerminated(context.Context, MainTermination) error
}

type TerminationHandlerFunc func(context.Context, MainTermination) error

func (f TerminationHandlerFunc) OnTerminated(ctx context.Context, event MainTermination) error {
	return f(ctx, event)
}

type Dispatcher interface {
	Dispatch(context.Context, Command) error
}

type DispatcherFunc func(context.Context, Command) error

func (f DispatcherFunc) Dispatch(ctx context.Context, c Command) error { return f(ctx, c) }

type Clock interface{ Now() time.Time }
type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time { return f() }

// Report marks one SUB_TASK attempt as terminal.
// Report 将某个 SUB_TASK 执行代次上报为终态。
type Report struct {
	Execution
	Status SubStatus
	Error  string
}

// ReportReceipt describes the committed report result.
// ReportReceipt 描述本次终态上报的提交结果；Changed=false 表示重复上报。
type ReportReceipt struct {
	Main    MainProgress
	Sub     SubState
	Changed bool
}

type StopReceipt struct {
	Main     MainProgress
	SubTasks []SubState
}

type RerunScope string

const (
	RerunFailed         RerunScope = "FAILED"
	RerunCanceled       RerunScope = "CANCELED"
	RerunFailedCanceled RerunScope = "FAILED_CANCELED"
)

type RerunReceipt struct {
	Main     MainProgress
	SubTasks []SubState
}

// CommittedError tells callers that retrying submission is not a rollback.
// Retry reconciliation/delivery using MainID instead.
type CommittedError struct {
	MainID string
	Cause  error
}

func (e *CommittedError) Error() string {
	return fmt.Sprintf("tflow %s committed: %v", e.MainID, e.Cause)
}
func (e *CommittedError) Unwrap() error { return e.Cause }

// IsNil rejects typed nil implementations without inspecting business fields.
func IsNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
