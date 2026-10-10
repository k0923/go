package workflow

import "time"

const defaultLeaseDuration = time.Minute

type RunState[Input any, Output any] struct {
	Status    RunStatus
	Input     Input
	Output    Output
	CreatedAt time.Time
	UpdatedAt time.Time
	Error     error
}

type ContinuationStatus string

const (
	ContinuationStatusRunning   ContinuationStatus = "running"
	ContinuationStatusRunnable  ContinuationStatus = "runnable"
	ContinuationStatusWaiting   ContinuationStatus = "waiting"
	ContinuationStatusSuspended ContinuationStatus = "suspended"
	ContinuationStatusCompleted ContinuationStatus = "completed"
)

type ContinuationKey struct {
	RunID     string
	BranchIDs []string
}

type ExecutionLease struct {
	Key        ContinuationKey
	OwnerID    string
	Generation uint64
	ExpiresAt  time.Time
}

type SuspensionKind string

const (
	SuspensionKindSignal SuspensionKind = "signal"
	SuspensionKindSleep  SuspensionKind = "sleep"
)

type Suspension struct {
	Kind     SuspensionKind
	Event    EventIdentity
	WakeAt   time.Time
	Metadata map[string]any
}

type ForkParallelResult struct {
	BranchLeases []ExecutionLease
	Initialized  bool
}

type CompleteContinuationResult struct {
	RemainingBranchIDs []string
	ParentLease        *ExecutionLease
}

type WorkflowStore[Input any, Output any] interface {
	// CreateRun 创建一次运行，并由 ownerID 持有根 continuation 的执行租约。
	CreateRun(runID string, input Input, ownerID string, leaseDuration time.Duration) (*RunState[Input, Output], ExecutionLease, error)
	// GetRunState 返回 runID 对应的持久化运行状态；运行不存在时返回 nil。
	GetRunState(runID string) (*RunState[Input, Output], error)
	// GetEvents 按追加顺序返回 runID 对应的持久化事件历史。
	GetEvents(runID string) ([]Event, error)

	// ClaimContinuation 抢占指定的可运行或已过期 continuation，用于继续执行。
	ClaimContinuation(key ContinuationKey, ownerID string, leaseDuration time.Duration) (ExecutionLease, error)
	// ClaimNextContinuation 抢占一次运行中的下一个可运行或已过期 continuation。
	ClaimNextContinuation(runID string, ownerID string, leaseDuration time.Duration) (ExecutionLease, error)
	// RenewLease 延长一个当前有效的执行租约。
	RenewLease(lease ExecutionLease, leaseDuration time.Duration) (ExecutionLease, error)
	// ReleaseContinuation 释放一个有效租约，并将对应 continuation 重新置为可运行。
	ReleaseContinuation(lease ExecutionLease) error

	// AppendEvent 向 lease 保护的分支追加事件。
	AppendEvent(lease ExecutionLease, event Event) error
	// SuspendContinuation 在需要时追加事件，并记录 continuation 的挂起状态。
	SuspendContinuation(lease ExecutionLease, event Event, suspension Suspension) error
	// ResolveSignal 记录已解析的信号，并抢占被该信号唤醒的挂起 continuation。
	ResolveSignal(runID string, awaited EventIdentity, resolved *SignalResolvedEvent, ownerID string, leaseDuration time.Duration) (ExecutionLease, error)

	// ForkParallel 记录并行开始事件，挂起父 continuation，并抢占子分支 continuation。
	ForkParallel(lease ExecutionLease, branchGroupID string, started *ParallelStartedEvent, branchIDs []string, leaseDuration time.Duration) (ForkParallelResult, error)
	// CompleteContinuation 完成一个分支，并在分支组全部归并后抢占父 continuation。
	CompleteContinuation(lease ExecutionLease, branchGroupID string, branchCompleted *BranchCompletedEvent, parallelCompleted *ParallelCompletedEvent, leaseDuration time.Duration) (CompleteContinuationResult, error)
	// CompleteRun 完成根 continuation，并持久化最终输出。
	CompleteRun(lease ExecutionLease, output Output) error
}
