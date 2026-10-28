package tflow

import (
	"context"
	"time"
)

type SubQuery struct {
	Statuses []SubStatus
	Keys     []string
	Limit    int
	// Nil starts at the first cursor; otherwise only Cursor > *AfterCursor.
	AfterCursor *int64
}

// Tx must not escape WithTx. Reads return isolated value state, ordered by cursor.
// Business interface objects are immutable or independently copied by the adapter.
// SaveMain/SaveSub check expected Version and require next.Version=expected+1.
// CreateSubs enforces unique keys and cursors within the main.
// Tx 是 Store 提供的单 MAIN_TASK 聚合事务视图。Tx 不能逃逸出 WithTx；
// 读出的值状态必须隔离，业务接口对象要么不可变，要么由适配器复制。
type Tx interface {
	// Main reads the current main record in this transaction, including the
	// transaction-local subtask summary.
	// Main 读取本事务内的当前主记录，包括事务候选状态下的子任务摘要。
	Main() (MainRecord, error)
	// CreateMain inserts a new main record at Version=1 and resets the persisted
	// subtask summary to the store-owned empty summary.
	// CreateMain 插入 Version=1 的新主记录，并将持久化子任务摘要重置为
	// Store 拥有的空摘要。
	CreateMain(MainRecord) error
	// CommitPlan persists the scheduling snapshot and adapter-owned source
	// progress atomically. expectedVersion=0 inserts a new main at Version=1;
	// otherwise it CAS-updates at Version=expected+1. The adapter interprets the
	// concrete Plan, without re-running Plan/Tasks/HasMore inside the transaction.
	// Child creation and commands are committed in the same surrounding WithTx.
	// CommitPlan 原子提交调度快照和适配器拥有的来源进度。
	// expectedVersion=0 表示插入 Version=1 的新主记录；否则按 expectedVersion
	// 做 CAS，并更新到 expectedVersion+1。适配器解释具体 Plan，但事务内不得重新
	// 调用 Plan/Tasks/HasMore。子任务创建和命令写入必须在同一个 WithTx 中提交。
	CommitPlan(expectedVersion int64, next MainRecord, plan Plan) error
	// SaveMain updates only the main runtime state, preserving the persisted task
	// and config. expectedVersion must match and next.Version must be expected+1.
	// SaveMain 只更新主任务运行态，保留已持久化的业务 Task 和配置。
	// expectedVersion 必须匹配，且 next.Version 必须等于 expectedVersion+1。
	SaveMain(expectedVersion int64, next MainState) error
	// SaveTask replaces business facts and advances Main.Version by one, keeping
	// config and scheduling state. Callers must enqueue Reconcile in the same tx.
	SaveTask(expectedVersion int64, task MainTask, updatedAt time.Time) error
	// Sub reads one subtask record by key in this transaction.
	// Sub 按 Key 读取本事务内的一个子任务记录。
	Sub(string) (SubRecord, error)
	// FindSubs reads isolated subtask records in cursor order, applying status,
	// key and limit filters.
	// FindSubs 按 Cursor 升序读取隔离的子任务记录，并应用状态、Key 和数量过滤。
	FindSubs(SubQuery) ([]SubRecord, error)
	// CreateSubs inserts new subtask records and enforces unique key and cursor
	// identity within the current main.
	// CreateSubs 插入新的子任务记录，并在当前主任务内校验 Key 和 Cursor 唯一。
	CreateSubs([]SubRecord) error
	// SaveSub updates only one subtask runtime state. expectedVersion must match,
	// next.Version must be expected+1, and the original cursor must be preserved.
	// SaveSub 只更新一个子任务运行态。expectedVersion 必须匹配，
	// next.Version 必须等于 expectedVersion+1，且必须保留原始 Cursor。
	SaveSub(expectedVersion int64, next SubState) error
	// Insert-only by Command.ID and logical execution/event identity. A duplicate
	// is a no-op even after Ack. Business event keys identify immutable content;
	// storage must not require framework serialization or structural equality.
	// MainTerminated includes the committed historical Main snapshot. Terminal
	// state and this command must commit atomically; preserve it across reruns.
	// Reconstruct the historical Task's TerminationHandler and dependencies,
	// retaining original routing fields rather than the current task's fields.
	// PutCommand 按 Command.ID 和逻辑执行/事件身份做只插入写入。重复写入即使发生在
	// Ack 之后也必须是空操作。业务 Event.Key 标识不可变内容；存储层不能要求框架
	// 序列化或结构相等。MainTerminated 携带已提交的历史 Main 快照；终态和该命令
	// 必须原子提交，并在重跑中保留。重建历史 Task 的 TerminationHandler 和依赖时，
	// 应保留原始路由字段，而不是使用当前 Task 的字段。
	PutCommand(Command) error
}

type Delivery struct {
	Command    Command
	Generation int64
	ExpiresAt  time.Time
}

type CommandState struct {
	Command    Command
	Generation int64
	ExpiresAt  time.Time
	Done       bool
	// Blocked marks work quarantined by an adapter; it remains unacknowledged.
	Blocked   bool
	LastError string
}

type CommandQuery struct {
	Kinds   []CommandKind
	AfterID string
	Limit   int
}

// Store.WithTx commits all callback writes or none. Callbacks contain only
// state operations and may be retried by an adapter. Planning, policy and
// dispatcher calls must remain outside transactions.
//
// Command claims/acks are persistent leases, not process-local locks. Ack and
// release fence generation and CURRENT expiry. Adapters use a consistent
// authoritative clock owned by Store. Retry delays are relative to that clock.
// Store is permanently bound to MainID. MainRecord atomically reads business
// progress, immutable config and state; state-only writes preserve the first two.
// Repository handles for the same ID share transactions and delivery leases.
// Store 是单个逻辑 MAIN_TASK 的持久化端口。WithTx 只做状态和 Command 写入；
// Plan、策略判断和外部派发必须在事务外执行。Command 的 claim/ack/release
// 是持久租约语义，不是进程内锁。
type Store interface {
	MainID() string
	GetMain(context.Context) (MainRecord, error)
	GetSub(context.Context, string) (SubRecord, error)
	// FindSubs is read-only and follows Tx.FindSubs isolation, ordering and filters.
	FindSubs(context.Context, SubQuery) ([]SubRecord, error)
	WithTx(context.Context, func(Tx) error) error
	ClaimCommand(context.Context, time.Duration) (*Delivery, error)
	AckCommand(context.Context, Delivery) error
	ReleaseCommand(context.Context, Delivery, time.Duration, string) error
	PendingCommands(context.Context) ([]CommandState, error)
}

// CommandReader provides paginated diagnostics without loading the whole outbox.
type CommandReader interface {
	FindCommands(context.Context, CommandQuery) ([]CommandState, error)
}

// Repository locates a logical main task. Opening it does not submit a task.
// Independent handles for the same ID must share state, CAS and delivery leases.
type Repository interface {
	Open(context.Context, string) (Store, error)
}
