package flow

import (
	"context"
	"time"
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

// ExecutionLease 表示某个可恢复执行分支当前持有的租约。
type ExecutionLease struct {
	// Key 是执行分支的全局唯一标识。
	Key string
	// Generation 是租约的递增版本，用作续租和释放时的 fencing token。
	Generation int64
	// ExpiresAt 是当前租约的过期时间。
	ExpiresAt time.Time
}

// LeaseInfrastructure 定义执行分支租约的存储能力。
type LeaseInfrastructure interface {
	// ClaimLease 尝试为 key 标识的执行分支申请有效期为 ttl 的租约。
	// 分支已被其他执行者持有时返回 (nil, nil)；存储异常通过 error 返回。
	ClaimLease(ctx context.Context, key string, ttl time.Duration) (*ExecutionLease, error)
	// RenewLease 使用 lease.Generation 进行 CAS 校验，并将租约有效期延长 ttl。
	// 租约已过期或 Generation 不匹配时返回 (nil, nil)；存储异常通过 error 返回。
	RenewLease(ctx context.Context, lease ExecutionLease, ttl time.Duration) (*ExecutionLease, error)
	// ReleaseLease 使用 lease.Generation 进行 CAS 校验并释放租约。
	// 返回值表示是否成功释放；租约不存在或 Generation 不匹配时返回 false。
	ReleaseLease(ctx context.Context, lease ExecutionLease) (bool, error)
}

type Infrastructure[Input any, Output any] interface {
	AddEvent(ctx context.Context, event Event) (Event, error)
	GetEvent(ctx context.Context, key string) (Event, error)
	UpdateEvent(ctx context.Context, event Event) (Event, error)
	GetState(ctx context.Context) (State[Input, Output], error)
	SetState(ctx context.Context, state State[Input, Output]) error
	OnError(err error)
}
