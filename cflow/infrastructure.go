package cflow

import (
	"context"
	"time"

	"github.com/k0923/go/lease"
)

// Infrastructure binds storage to ONE run. RunID/TenantID remain outside the
// kernel; implementations compose resolved keys with their bound scope.
//
// Every returned Record is a deep copy. Every operation checks ctx. Atomic
// methods below must be implemented by a database transaction or equivalent.
// A timeout may mean the commit succeeded; callers must retry idempotently.
// Infrastructure 将存储绑定到单个运行。RunID/TenantID 保持在内核之外；
// 实现方会将解析后的 key 与其绑定的作用域组合起来。
//
// 每个返回的 Record 都是深拷贝。每个操作都会检查 ctx。下面的原子方法必须
// 通过数据库事务或等价机制实现。超时可能意味着提交已经成功；调用方必须幂等重试。
type Infrastructure interface {
	lease.Infrastructure
	Get(context.Context, string) (*Record, error)
	// Commit checks CURRENT lease generation and expiry and record version in
	// one atomic write. expected=0 means insert-only. Success increments version.
	// Commit 在一次原子写入中检查当前租约 generation、过期时间和记录版本。
	// expected=0 表示仅插入。成功后递增版本。
	Commit(context.Context, Lease, *Record, int64) (*Record, error)
	// Put inserts a checkpoint, definition or waiting record if absent, or
	// returns the existing record. It never overwrites a resolved signal.
	// For Signal/Sleep, registration AND its delivery intent are atomic.
	// Put 在记录不存在时插入 checkpoint、definition 或 waiting 记录，否则返回已有记录。
	// 它绝不会覆盖已解析的 signal。对于 Signal/Sleep，注册和其投递意图是原子的。
	Put(context.Context, *Record) (*Record, error)
	// Resolve checks suspended status, kind and version; commits the response
	// AND a distinct wake-up intent atomically, so the caller's dispatcher can
	// Drive the run again. Already resolved is a no-op returning false. A stale
	// version returns ErrConflict. How delivery is exposed (an outbox, MQ, ...)
	// is the adapter's choice; see package memory for a reference outbox.
	// Resolve 检查 suspended 状态、kind 和版本；并原子地提交响应以及一个独立的唤醒意图，
	// 以便调用方的调度器再次 Drive。已解析时不执行操作并返回 false。过期版本返回 ErrConflict。
	// 投递如何暴露（outbox、MQ 等）由适配器决定；参考 outbox 见 memory 包。
	Resolve(context.Context, string, int64, []byte) (bool, error)
}

// RecordLister is OPTIONAL. When implemented, Drive loads the run's records in
// one call and replays terminal outcomes (finished Actions, resolved waits,
// checkpoints) without a per-step Get. Returned records must be deep copies.
// RecordLister 是可选接口。实现后，Drive 会一次性加载 run 的记录，并在不逐步 Get
// 的情况下回放终态结果（已结束的 Action、已解析的等待、检查点）。返回的记录必须是深拷贝。
type RecordLister interface {
	Records(context.Context) ([]*Record, error)
}

// Clock is OPTIONAL. When Options.Now is nil, NewEngine uses the store's clock
// so Sleep deadlines and lease expiry are judged by the same time source.
// Clock 是可选接口。Options.Now 为空时，NewEngine 使用存储时钟，使 Sleep 截止时间与
// 租约过期由同一时间源判断。
type Clock interface {
	Now() time.Time
}
