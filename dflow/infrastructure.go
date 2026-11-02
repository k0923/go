package dflow

import (
	"context"
	"time"

	"github.com/k0923/go/lease"
)

// Status describes a node, never the entire workflow.
type Status string

const (
	Pending   Status = "pending"
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Skipped   Status = "skipped"
	Failed    Status = "failed"
	// Suspended means an async node has persisted done=false. A subsequent
	// explicit Drive may claim it again; recovery scans should not auto-poll it.
	Suspended Status = "suspended"
)

// NodeState contains persisted object-shaped input/output, including partial
// results on failure or done=false. Incoming records every predecessor's
// decision (active=true or skipped/deselected=false). All must arrive before
// readiness; any active arrival enables Run, all inactive arrivals enable skip.
type NodeState struct {
	// Version is the storage revision of this node state. Storage adapters use
	// it for compare-and-swap style writes and increment it on every mutation.
	//
	// Version 是该节点状态的存储版本。存储适配器用它做 CAS 风格写入校验，
	// 并在每次状态变更时递增。
	Version int64
	// Status is this node's lifecycle state.
	//
	// Status 是该节点的生命周期状态。
	Status Status
	// Input is the object-shaped input captured from NodeResult.GetInput.
	//
	// Input 是从 NodeResult.GetInput 捕获的对象形输入。
	Input map[string]any
	// Output is the object-shaped output captured from NodeResult.GetOutput.
	//
	// Output 是从 NodeResult.GetOutput 捕获的对象形输出。
	Output map[string]any
	// Outgoing is the active downstream node list selected by this execution.
	// It is populated only for successful propagation.
	//
	// Outgoing 是本次执行实际选择并激活的下游节点列表，只在成功传播时填充。
	Outgoing []string
	// Error stores the business or result-normalization error message when the
	// node reaches Failed.
	//
	// Error 存储节点进入 Failed 时的业务错误或结果规范化错误信息。
	Error string
	// Incoming records every predecessor decision by predecessor node ID. true
	// means that predecessor activated this node; false means skipped or
	// deselected.
	//
	// Incoming 按前驱节点 ID 记录每个前驱决策。true 表示该前驱激活了本节点；
	// false 表示该前驱被跳过，或分支未选择本节点。
	Incoming map[string]bool
	// Ready reports whether this node is claimable by Drive.
	//
	// Ready 表示该节点当前是否可被 Drive 获取并推进。
	Ready bool
	// Enabled reports whether at least one arrived predecessor activated this
	// node. A ready node with Enabled=false is skipped without calling Run.
	//
	// Enabled 表示是否至少有一个已到达前驱激活了本节点。Ready 但 Enabled=false
	// 的节点会被跳过，不调用 Run。
	Enabled bool
	// Generation identifies the current execution lease generation for this
	// node. It changes when ClaimNode acquires a new ownership attempt.
	//
	// Generation 标识该节点当前执行租约的代次。ClaimNode 获取一次新的执行所有权时更新它。
	Generation int64
}

// Infrastructure is business-scoped to one execution of an immutable
// publication. Business startup binds the definition/input and seeds the root
// as Pending, Ready=true, Enabled=true. Value loading, external task progress,
// callbacks and execution initialization are business responsibilities.
//
// Implementations own serialization, type reconstruction and snapshot
// isolation. All methods must honor ctx. Protected writes validate the lease's
// generation and storage-side current expiry atomically.
type Infrastructure interface {
	// ClaimNode atomically acquires a free/expired node lease and returns an
	// independent state snapshot. Claim ready Pending nodes, lost Running
	// attempts, Failed retries and explicitly driven Suspended nodes. Do not
	// claim unresolved Pending nodes, terminal nodes or nodes under a live
	// lease. Nil means nothing to drive.
	//
	// Increment Version and Generation, set Running and clear Ready. Preserve
	// Input/Output and Incoming/Enabled. The lease key equals nodeID and its
	// generation equals the state's new Generation. ttl must be positive.
	ClaimNode(ctx context.Context, nodeID string, ttl time.Duration) (*NodeTask, error)
	// RenewNode checks generation and storage-side current expiry. Stale or
	// expired ownership returns lease.ErrConflict.
	RenewNode(context.Context, lease.Token, time.Duration) (*lease.Token, error)
	// ReleaseNode frees ownership; a stale generation returns lease.ErrConflict.
	ReleaseNode(context.Context, lease.Token) error
	// Commit validates the lease and ExpectedVersion in the same transaction
	// that saves the result, merges all target arrivals and releases ownership.
	// Preserve Incoming/Enabled/Generation; increment Version in storage.
	//
	// Only Succeeded/Skipped propagate. Failed/Suspended save Input/Output/Error
	// without downstream arrivals. Use ValidateCommit and ApplyArrival inside
	// the transaction. Duplicate predecessor decisions must never overwrite a
	// different value, and readiness must be created only once.
	//
	// Return only nodes newly ready from this commit. Persist readiness
	// independently of the returned list so business recovery can repair a
	// queue delivery lost after commit. Roll back all changes on error.
	Commit(context.Context, lease.Token, NodeCommit) ([]string, error)
}

type NodeTask struct {
	NodeID string
	State  NodeState
	Lease  lease.Token
}

// NodeCommit is the result mutation produced by one successful Drive attempt.
// Infrastructure.Commit must persist these fields into the current NodeState,
// merge Arrivals, validate the lease and ExpectedVersion, and release ownership
// atomically.
//
// NodeCommit 表示一次 Drive 尝试产生的结果变更。Infrastructure.Commit 必须将
// 这些字段持久化到当前 NodeState、合并 Arrivals、校验租约和 ExpectedVersion，
// 并原子释放所有权。
type NodeCommit struct {
	// NodeID is the node whose execution result is being committed.
	//
	// NodeID 是本次正在提交执行结果的节点 ID。
	NodeID string
	// ExpectedVersion is the node state version observed when the lease was
	// claimed. Commit must fail on mismatch to prevent stale writes.
	//
	// ExpectedVersion 是获取租约时观察到的节点状态版本。提交时若不匹配，
	// 必须失败，以避免过期写入。
	ExpectedVersion int64
	// Status is the terminal or suspended status produced by this Drive.
	//
	// Status 是本次 Drive 产生的终态或挂起状态。
	Status Status
	// Input is the captured input returned by NodeResult.GetInput. nil means
	// keep the currently persisted input; non-nil replaces it completely.
	//
	// Input 是从 NodeResult.GetInput 捕获的输入。nil 表示保留当前已持久化输入；
	// 非 nil 表示全量替换。
	Input map[string]any
	// Output is the captured output returned by NodeResult.GetOutput. nil means
	// keep the currently persisted output; non-nil replaces it completely.
	//
	// Output 是从 NodeResult.GetOutput 捕获的输出。nil 表示保留当前已持久化输出；
	// 非 nil 表示全量替换。
	Output map[string]any
	// Error is the failure value when Status is Failed. Storage adapters decide
	// how to persist it, for example by saving Error().
	//
	// Error 是 Status 为 Failed 时的错误值。存储适配器自行决定如何持久化，
	// 例如保存 Error() 字符串。
	Error error
	// Arrivals maps each direct downstream node ID to whether this predecessor
	// activated that edge. true entries become the persisted outgoing list. false
	// means skipped or branch-deselected. It is empty for failed or suspended
	// executions.
	//
	// Arrivals 将每个直接下游节点 ID 映射为当前前驱是否激活该边。值为 true 的
	// 条目会成为持久化的 outgoing 列表；false 表示被跳过或分支未选择。失败或挂起
	// 的执行不会产生该映射。
	Arrivals map[string]bool
}
