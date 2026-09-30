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
	CreateRun(runID string, input Input, ownerID string, leaseDuration time.Duration) (*RunState[Input, Output], ExecutionLease, error)
	GetRunState(runID string) (*RunState[Input, Output], error)
	GetEvents(runID string) ([]Event, error)

	ClaimContinuation(key ContinuationKey, ownerID string, leaseDuration time.Duration) (ExecutionLease, error)
	ClaimNextContinuation(runID string, ownerID string, leaseDuration time.Duration) (ExecutionLease, error)
	RenewLease(lease ExecutionLease, leaseDuration time.Duration) (ExecutionLease, error)
	ReleaseContinuation(lease ExecutionLease) error

	AppendEvent(lease ExecutionLease, event Event) error
	SuspendContinuation(lease ExecutionLease, event Event, suspension Suspension) error
	ResolveSignal(runID string, awaited EventIdentity, resolved *SignalResolvedEvent, ownerID string, leaseDuration time.Duration) (ExecutionLease, error)

	ForkParallel(lease ExecutionLease, branchGroupID string, started *ParallelStartedEvent, branchIDs []string, leaseDuration time.Duration) (ForkParallelResult, error)
	CompleteContinuation(lease ExecutionLease, branchGroupID string, branchCompleted *BranchCompletedEvent, parallelCompleted *ParallelCompletedEvent, leaseDuration time.Duration) (CompleteContinuationResult, error)
	CompleteRun(lease ExecutionLease, output Output) error
}
