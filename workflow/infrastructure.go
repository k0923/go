package workflow

import "context"

type FlowStatus string

const (
	FlowStatusRunning   FlowStatus = "RUNNING"
	FlowStatusCompleted FlowStatus = "COMPLETED"
	FlowStatusFailed    FlowStatus = "FAILED"
	FlowStatusSuspended FlowStatus = "SUSPENDED"
)

type State[Input any, Output any] struct {
	Input  Input
	Output Output
	Status FlowStatus
	Errors []string
}

// Infrastructure provides the runtime persistence and lease primitives.
// The workflow runtime does not own workflow-instance isolation. It passes the
// resolved event key to Infrastructure and expects the implementation to decide
// how to scope that key. Production implementations can include a run ID,
// tenant ID, namespace, or other isolation dimension in their storage keys or
// query conditions. The runtime focuses on deterministic execution, replay,
// leases, and event consistency for the keys it is given.
//
// Infrastructure 提供运行时持久化和租约原语。
// workflow 运行时不负责流程实例隔离。运行时只会把解析后的事件 key 传给
// Infrastructure，具体如何限定这个 key 的作用域由实现方决定。生产实现可以在
// 存储 key 或查询条件中加入 run ID、tenant ID、namespace 或其他隔离维度。
// 框架本身专注于在给定 key 下保证确定性执行、回放、租约和事件一致性。
type Infrastructure interface {
	LeaseInfrastructure
	AddEvent(ctx context.Context, event Event) (Event, error)
	GetEvent(ctx context.Context, key string) (Event, error)
	UpdateEvent(ctx context.Context, event Event) (Event, error)
	OnError(err error)
}
