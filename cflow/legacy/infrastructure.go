package cflow

import (
	"context"
)

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
	// Suspend 持久化一个信号事件，并按信号 key 幂等：
	// 同 key 同类型已存在时保留既有事件，不覆盖；同 key 类型冲突返回 error。
	// 同时让该信号可被外部调度器发现与投递，具体投递机制由实现决定。
	Suspend(ctx context.Context, signal Signal) error
	// Transaction 提供一个事务边界：handler 内的所有写入要么一起提交，要么一起回滚。
	// handler 返回非 nil error 即回滚；写入必须对 handler 自身可见（read-your-writes）。
	Transaction(ctx context.Context, handler func(api Infrastructure) error) error
	AddEvent(ctx context.Context, event Event) (Event, error)
	GetEvent(ctx context.Context, key string) (Event, error)
	UpdateEvent(ctx context.Context, event Event) (Event, error)
	OnError(err error)
}
