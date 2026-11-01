package cflow

import (
	"context"
	"encoding/json"
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
	// The lease is either the Action's own (Lease.Key == Record.Key, root run)
	// or its owner's (Lease.Key == Record.ExecutionKey, resumed child); an existing
	// record must keep its ExecutionKey.
	// Commit 在一次原子写入中检查当前租约 generation、过期时间和记录版本。
	// expected=0 表示仅插入。成功后递增版本。租约可以是 Action 自身的（Key 等于记录 key，根运行），
	// 也可以是所属执行单元的（Key 等于 Record.ExecutionKey，恢复子级时）；已有记录的 ExecutionKey 不得改变。
	Commit(context.Context, Lease, *Record, int64) (*Record, error)
	// Put inserts a checkpoint, definition or waiting record if absent, or
	// returns the existing record. It never overwrites a resolved signal.
	// For Signal/Sleep, registration AND its delivery intent are atomic.
	// Put 在记录不存在时插入 checkpoint、definition 或 waiting 记录，否则返回已有记录。
	// 它绝不会覆盖已解析的 signal。对于 Signal/Sleep，注册和其投递意图是原子的。
	Put(context.Context, *Record) (*Record, error)
	// Resolve checks suspended status, kind and version; commits the response
	// AND a distinct wake-up intent atomically, so the caller's dispatcher can
	// Resume the run again. Already resolved is a no-op returning false. A stale
	// version returns ErrConflict. How delivery is exposed (an outbox, MQ, ...)
	// is the adapter's choice; see package memory for a reference outbox.
	// Resolve 检查 suspended 状态、kind 和版本；并原子地提交响应以及一个独立的唤醒意图，
	// 以便调用方的调度器再次 Drive。已解析时不执行操作并返回 false。过期版本返回 ErrConflict。
	// 投递如何暴露（outbox、MQ 等）由适配器决定；参考 outbox 见 memory 包。
	Resolve(context.Context, string, int64, []byte) (bool, error)
}

// RecordLister is an optional observation interface. Drive and DriveFlow read
// only the steps in their own execution boundary. Records must be deep copies.
// RecordLister 是可选观测接口；Drive 与 DriveFlow 按需读取各自执行边界中的步骤。
// 返回的记录必须是深拷贝。
type RecordLister interface {
	Records(context.Context) ([]*Record, error)
}

// ExecutionLoader is OPTIONAL. When implemented, Drive and DriveFlow load
// everything one boundary replays from in one call: the records it owns
// (Record.ExecutionKey; "" is the root), and the standalone children and loops
// it started (ParentKey). Terminal entries replay without further reads.
// Nothing of other boundaries may be returned. Values must be deep copies.
// It runs before every Start/Resume of every boundary, so index ownership:
// the cost must depend on that boundary's own data, not on the whole run.
// ExecutionLoader 是可选接口。实现后，Drive 与 DriveFlow 一次性加载某个执行边界回放所需的全部数据：
// 它拥有的记录（Record.ExecutionKey，根为空），以及它启动的独立子级和循环（ParentKey）。
// 终态项回放时不再读取存储。不得返回其他边界的数据；返回值必须是深拷贝。
// 每个执行边界每次 Start/Resume 前都会调用它，因此需按归属建立索引：成本只应取决于该边界自身的数据，
// 而不是整个运行。
type ExecutionLoader interface {
	LoadExecution(context.Context, string) (*ExecutionSnapshot, error)
}

// ExecutionSnapshot is the replay state of one boundary. Flows holds standalone
// children (LoopKey == ""); each loop carries its successful outputs in index
// order once terminal.
// ExecutionSnapshot 是一个执行边界的回放状态。Flows 为独立子级（LoopKey 为空）；
// 循环终态后按下标携带成功前缀的输出。
type ExecutionSnapshot struct {
	Records []*Record
	Flows   []*FlowExecution
	Loops   []*LoopSnapshot
}

type LoopSnapshot struct {
	LoopState
	Outputs []json.RawMessage `json:",omitempty"`
}

// Clock is OPTIONAL. When Options.Now is nil, NewEngine uses the store's clock
// so Sleep deadlines and lease expiry are judged by the same time source.
// Clock 是可选接口。Options.Now 为空时，NewEngine 使用存储时钟，使 Sleep 截止时间与
// 租约过期由同一时间源判断。
type Clock interface {
	Now() time.Time
}
