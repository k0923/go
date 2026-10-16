package workflow

import (
	"context"

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
	// For Signal/Sleep, registration AND notification insertion are atomic.
	// Put 在记录不存在时插入 checkpoint、definition 或 waiting 记录，否则返回已有记录。
	// 它绝不会覆盖已解析的 signal。对于 Signal/Sleep，注册和通知插入是原子的。
	Put(context.Context, *Record) (*Record, error)
	// Resolve checks suspended status, kind and version; commits the response
	// AND a distinct Wake notification atomically. Already resolved is a no-op
	// returning false. A stale version returns ErrConflict.
	// Resolve 检查 suspended 状态、kind 和版本；并原子地提交响应以及一个独立的 Wake 通知。
	// 已解析时不执行操作并返回 false。过期版本返回 ErrConflict。
	Resolve(context.Context, string, int64, []byte) (bool, error)
}
