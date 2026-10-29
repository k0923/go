package taskpool

import "context"

// State is a consistent instance snapshot; Active includes tasks awaiting report
// and slots awaiting replacement or dispatch. It contains no business status.
//
// State 是实例的一致快照。
// Active 包含等待完成上报、等待认领替补和等待派发的槽位，不包含业务状态。
type State struct {
	// Capacity is the configured number of execution chains and remains fixed.
	// Capacity 是配置的执行链数量，运行期间固定。
	Capacity int
	// Active counts slots not yet retired, including replacement work.
	// Active 统计尚未关闭的槽位，包括正在认领替补任务的槽位。
	Active int
}

// Completed is true after every slot has retired.
// Completed 表示所有槽位均已关闭。
func (s State) Completed() bool { return s.Active == 0 }

// OperationKind distinguishes durable dispatch from replacement claim intent.
// OperationKind 区分持久的任务派发意图与替补认领意图。
type OperationKind string

const (
	// DispatchTask delivers the current task for one slot.
	// DispatchTask 派发某个槽位中的当前任务。
	DispatchTask OperationKind = "DISPATCH_TASK"
	// ClaimNext requests a task for an initial empty slot or after completion.
	// ClaimNext 为初始空槽位或任务结束后的槽位请求一条任务。
	ClaimNext OperationKind = "CLAIM_NEXT"
)

// Delivery is an exclusively acquired infrastructure action, not a completion
// reference. Engine passes it unchanged to Store confirmation/release methods.
// Delivery 是独占领取的基础设施动作，不是完成上报引用，Engine 原样交给 Store 确认或释放。
type Delivery[T any] struct {
	// Slot identifies a chain for per-Drain exclusion, never business task identity.
	// Slot 定位每轮排除的执行链，不表示业务任务身份。
	Slot int
	// Kind selects dispatch or replacement claim.
	// Kind 区分派发或替补认领。
	Kind OperationKind
	// Task is meaningful only for DispatchTask; Factory supplies all claim data.
	// Task 只在 DispatchTask 中有意义，所有认领结果均由 Factory 提供。
	Task T
	// Token is opaque Store-owned acquisition data, such as a lease token,
	// revision or receipt supplied by the application's infrastructure.
	// Engine never compares, serializes or interprets it. It is not sent to tasks.
	// Token 是业务基础设施提供的不透明领取数据，例如租约令牌、修订或回执。
	// Engine 不比较、不序列化、不解释，
	// 也不将其发送给业务任务。
	Token any
}

// Store is the application-provided infrastructure contract for one bound
// distributed pool instance. It owns persistence and coordination across workers.
// Every transition is atomic, and Acquire must exclusively reserve a slot until
// confirmation, release or infrastructure-controlled expiry. Report during an
// acquired dispatch records completion but must not make its replacement
// claimable until that ownership ends. This also coordinates concurrent or
// reentrant Drain without any Engine-local lock.
// Adapters own association, task isolation/encoding, ownership fencing, clocks,
// retry eligibility and recovery. Tasks require no Key or comparability. Slots
// and tokens are infrastructure coordinates, never business run identities.
// Store 是业务为单个分布式任务池提供的基础设施契约，负责持久化与跨 worker 协调。
// 每个转换原子执行，Acquire 独占槽位直到确认、释放或基础设施控制的到期。
// 在派发仍被持有时 Report 可记录完成，但所有权结束前不能使替补可领取，
// 因此并发或重入 Drain 不依赖 Engine 本地锁。关联、任务隔离及编码、所有权校验、
// 时钟、重试时机和恢复由适配器负责，Task 不要求 Key 或可比较；槽位和令牌只用于
// 基础设施，不表示业务运行身份。
// Applications create and initialize the execution chains and initial ClaimNext
// intents before binding Store to Engine. Initialization, duplicate creation and
// restoration are adapter responsibilities, not part of this interface. A prepared
// instance with zero chains is complete.
// 业务在绑定 Engine 前创建并初始化执行链和初次 ClaimNext 意图，初始化、重复创建
// 及恢复由适配器负责，不属于此接口；已准备好的零执行链实例表示完成。
type Store[T any] interface {
	// State reads capacity and active slots from one consistent snapshot.
	// State 从同一个一致快照中读取容量与活跃槽位数。
	State(context.Context) (State, error)
	// Report associates the business Task with its issued record and atomically
	// creates one ClaimNext intent. The adapter owns task association, duplicate
	// detection and stale reports. Factory does not consume the reported Task.
	// Task has no Key/comparable requirement. Duplicate completion never creates
	// additional intent; old tasks cannot complete a current replacement. Report
	// preserves in-flight slot ownership and never invokes external callbacks.
	// Report 将业务 Task 关联到已发出记录，原子创建一次 ClaimNext 意图；
	// 关联、重复检测及旧上报由适配器处理，Factory 不读取上报 Task。
	// Task 不要求 Key 或可比较，重复完成不得多创建意图，旧任务不得完成当前替补。
	// 上报保持在途槽位独占，不调用外部工厂或派发器。
	Report(context.Context, T) error
	// Acquire claims an available dispatch/claim action or returns nil. Pending
	// actions remain recoverable according to adapter policy. Waiting accepted
	// tasks are not claimed again until their runtime reports completion.
	// excludedSlots is an immutable per-Drain list: none of these slots may be
	// returned, irrespective of task or Kind. This prevents same-call refill and
	// retry monopolization. Selection should fairly visit other eligible slots.
	//
	// Acquire 领取可处理的派发或认领动作，没有可领取动作时返回 nil。
	// 错误或所有权到期后由适配器保证恢复，已接管任务在上报后才继续推进。
	// excludedSlots 是本次 Drain 的只读排除列表，无论 Task 或 Kind 都不能返回
	// 这些槽位，防止本轮自动补充或失败动作独占；应公平选择其他可处理槽位。
	Acquire(ctx context.Context, excludedSlots []int) (*Delivery[T], error)
	// ConfirmDispatch ends acquisition after reliable handoff. If Report arrived
	// during dispatch, preserve its completion and make the claim eligible.
	// Repeated committed confirmations are no-ops; stale holders cannot modify
	// the current action. An invalid holder returns ErrOwnershipLost.
	// ConfirmDispatch 在可靠接管后结束领取；派发期间已有 Report 时，保留完成并
	// 使认领可处理。已提交确认重复调用为空操作，旧持有者不能修改当前动作，
	// 无效持有者返回 ErrOwnershipLost。
	ConfirmDispatch(context.Context, Delivery[T]) error
	// Claim coordinates Factory.Claim and saving a task with its dispatch intent,
	// or retiring the chain for ok=false, ending ownership atomically. First
	// claims and replenishment use the same protocol without synthetic tasks.
	// Repeated committed deliveries are no-ops and do not consume the source again.
	// Durable adapters must couple source consumption with slot persistence using
	// a shared transaction or infrastructure-owned idempotent claim protocol. A
	// plain dequeue followed by a separate write cannot guarantee crash recovery.
	// The callback may receive an adapter transaction context. Return FactoryError
	// only if the source callback failed and rollback/recovery succeeded; storage
	// failures must not be classified as FactoryError. A stale holder cannot
	// overwrite a current slot or commit a second task.
	// Claim 协调 Factory.Claim 与任务及派发意图保存，ok=false 则结束执行链，
	// 同时原子结束所有权；初次和补充认领走同一协议，不使用虚构 Task。
	// 已提交动作重复为空操作，不再次消费来源。持久适配器应通过共享事务或
	// 基础设施幂等认领协议关联来源消费与槽位保存，普通出队后单独写入无法保证
	// 宕机恢复。回调可接收适配器事务上下文；仅来源回调失败且回滚或恢复成功时
	// 返回 FactoryError，存储失败不得归入此类型。旧持有者不能覆盖槽位或多提交任务。
	Claim(context.Context, Delivery[T], Factory[T]) error
	// Release ends the current acquisition without discarding pending work.
	// Store determines retry timing. If completion arrived during failed
	// dispatch, retain its claim instead of replaying that completed task.
	// Release 结束当前领取并保留待处理动作，Store 决定重试时间；失败派发期间
	// 已上报完成时，保留认领意图，不重新派发已完成任务。
	Release(context.Context, Delivery[T]) error
}
