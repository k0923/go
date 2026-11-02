package dflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/k0923/go/lease"
)

type Options struct {
	NodeTimeout time.Duration
	// Lease configures the per-node execution lease (TTL, renewal at TTL/3, and
	// bounded release). A node whose owner stops renewing becomes claimable by any
	// instance once the TTL elapses.
	//
	// Lease 配置每个节点的执行租约，包括 TTL、按 TTL/3 续租以及有界释放。
	// 当节点 owner 停止续租后，TTL 到期即会被任意实例重新 claim。
	Lease lease.Options
	// Hooks observes node lifecycle transitions. It never affects scheduling.
	//
	// Hooks 观测节点生命周期转换。它绝不会影响调度。
	Hooks Hooks
}

// Engine drives one node per call and keeps no workflow status: Drive executes a
// single node, persists its outcome and returns the downstream nodes that became
// ready. Execution ownership is per node, so distinct nodes of one run may be
// driven concurrently from goroutines, workers or instances. The caller decides
// how to continue — recurse in-process, enqueue to a queue, or stop — giving the
// business full control over concurrency, ordering and failure handling.
//
// Engine 每次调用只推进一个节点，并且不保存工作流整体状态：Drive 执行单个节点、
// 持久化结果，并返回因此变为 ready 的下游节点。执行所有权按节点划分，所以同一
// run 的不同节点可以由 goroutine、worker 或实例并发推进。调用方决定如何继续：
// 在进程内递归、入队，或者停止，从而让业务完全控制并发、顺序和失败处理。
type Engine[R any] struct {
	definitions DefinitionProvider[R]
	infra       Infrastructure
	options     Options
}

// Executed reports a committed Drive step. Result is the original business
// result returned by Run; it is nil when the node was skipped, Run panicked,
// or a result getter panicked. Status is non-empty only after a successful
// commit. Ready contains downstream nodes that became ready after commit.
type Executed struct {
	NodeID string
	Status Status
	Result NodeResult
	Ready  []string
}

// NewEngine loads executable nodes from a business-owned immutable publication.
func NewEngine[R any](provider DefinitionProvider[R], infra Infrastructure, options Options) (*Engine[R], error) {
	if isNil(provider) || isNil(infra) {
		return nil, fmt.Errorf("dflow: provider and infrastructure required")
	}
	if options.NodeTimeout == 0 {
		options.NodeTimeout = 5 * time.Minute
	}
	if options.NodeTimeout < 0 {
		return nil, fmt.Errorf("dflow: invalid options")
	}
	// Validate lease options once; Drive builds a runner per node lease.
	//
	// 先校验一次 lease 选项；Drive 会为每个节点租约构建 runner。
	if _, err := lease.NewRunner(&nodeLease{}, options.Lease); err != nil {
		return nil, err
	}
	return &Engine[R]{provider, infra, options}, nil
}

// Drive executes exactly one node and persists its outcome. It holds only that
// node's lease — claimed, renewed while Run executes, and ended by Commit
// — so distinct nodes of one run proceed in parallel, across processes too.
// Duplicate deliveries of the same node are safe: only one claim wins.
//
// A node that is unresolved pending, terminal or owned by a live lease yields a
// zero Executed and nil error. A business node failure returns Status Failed with
// the error (fail-fast): no downstream node becomes ready. Infrastructure/lease
// errors are uncertain and should be retried by driving the same node again.
//
// Drive 精确执行一个节点并持久化其结果。它只持有该节点的租约：先 claim，
// 在 Run 执行期间续租，并在 Commit 时结束。因此同一 run 的不同节点
// 可以跨进程并行推进。重复投递同一节点是安全的：只有一个 claim 会获胜。
//
// 如果节点处于未就绪 pending、terminal，或正被有效租约持有，Drive 会返回
// 零值 Executed 和 nil error。业务节点失败会返回 Status Failed 和对应错误
// （fail-fast）：不会有下游节点变为 ready。Infrastructure/lease 错误的结果不确定，
// 应通过再次驱动同一节点来重试。
// R supplies both typed parameter resolution and business execution APIs. The
// caller prepares it using PublishedNode.GetDependencies before driving the node.
// A Suspended node is explicitly claimable; Drive never auto-polls it.
func (e *Engine[R]) Drive(ctx context.Context, nodeID string, runtime R) (executed Executed, err error) {
	if nodeID == "" {
		return Executed{}, ErrInvalidDefinition
	}
	if isNil(runtime) {
		return Executed{}, ErrInvalidRuntime
	}
	owner := &nodeLease{infra: e.infra}
	runner, err := lease.NewRunner(owner, e.options.Lease)
	if err != nil {
		return Executed{}, err
	}
	err = runner.Run(ctx, nodeID, func(ctx context.Context, token lease.Token) error {
		task := owner.task
		executed.NodeID = nodeID
		e.options.Hooks.claim(ctx, NodeEvent{NodeID: nodeID, Attempt: task.State.Generation})
		node, result, execErr := e.execute(ctx, task, runtime)
		if ctx.Err() != nil {
			// Lease lost or caller cancelled: the outcome is uncertain, not a
			// business failure. Leave the node Running for a retry/reclaim.
			//
			// 租约丢失或调用方取消时，结果是不确定的，而不是业务失败。保留节点
			// Running 状态，等待重试或重新 claim。
			return &InfrastructureError{"Execute", context.Cause(ctx)}
		}
		var status Status
		var arrivals map[string]bool
		var resultErr error
		if execErr != nil {
			var infraErr *InfrastructureError
			if errors.As(execErr, &infraErr) {
				return execErr
			}
			resultErr = execErr
			status = Failed
		} else if !result.done {
			status = Suspended
		} else {
			status = Succeeded
			if !task.State.Enabled {
				status = Skipped
			}
			arrivals = e.arrivals(node, status, result.outgoing)
		}
		ready, commitErr := owner.commit(ctx, token, NodeCommit{
			NodeID:          nodeID,
			ExpectedVersion: task.State.Version,
			Status:          status,
			Input:           result.input,
			Output:          result.output,
			Error:           resultErr,
			Arrivals:        arrivals,
		})
		if commitErr != nil {
			return commitErr
		}
		executed.Status = status
		executed.Result = result.raw
		executed.Ready = ready
		if execErr != nil {
			return &NodeError{nodeID, "execute", execErr}
		}
		return nil
	})
	if errors.Is(err, errNotDrivable) {
		return Executed{}, nil
	}
	return executed, err
}

// errNotDrivable reports that ClaimNode found nothing to do. Drive maps it to a
// zero Executed rather than an error.
//
// errNotDrivable 表示 ClaimNode 没有找到可执行工作。Drive 会将它映射为零值
// Executed，而不是错误。
var errNotDrivable = errors.New("dflow: node not drivable")

// nodeLease adapts one Drive's node claim to lease.Runner, reusing its renewal,
// cancellation-on-loss and bounded release. Commit validates the lease and ends
// ownership in storage. Renew and Commit serialize on this Drive's local mutex;
// a renewal already in flight completes before Commit can release the lease.
// This is never a run/global lock. A committed lease is not released again.
//
// nodeLease 将一次 Drive 的节点 claim 适配到 lease.Runner，并复用它的续租、
// 丢失租约取消和有界释放能力。Commit 校验租约并在存储中结束所有权。Renew 和
// Commit 通过本次 Drive 的本地互斥锁串行化；已经在途的续租会先完成，之后
// Commit 才能释放租约。这绝不是 run/global 级别的锁。已 commit 的租约不会再次释放。
type nodeLease struct {
	infra     Infrastructure
	task      *NodeTask
	mu        sync.Mutex
	ending    bool
	committed bool
	renewErr  error
}

func (l *nodeLease) Claim(ctx context.Context, nodeID string, ttl time.Duration) (*lease.Token, error) {
	task, err := l.infra.ClaimNode(ctx, nodeID, ttl)
	if err != nil {
		return nil, &InfrastructureError{"ClaimNode", err}
	}
	if task == nil {
		return nil, errNotDrivable
	}
	if task.NodeID != nodeID || task.Lease.Key != nodeID ||
		task.Lease.Generation != task.State.Generation || task.State.Status != Running {
		return nil, &InfrastructureError{"ClaimNode", ErrInvalidDefinition}
	}
	l.task = task
	token := task.Lease
	return &token, nil
}

func (l *nodeLease) Renew(ctx context.Context, token lease.Token, ttl time.Duration) (*lease.Token, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ending {
		return &token, nil
	}
	// Bound the in-flight request so Commit cannot wait indefinitely for a
	// storage renewal that has already outlived this ownership interval.
	//
	// 限制在途请求时间，避免 Commit 无限等待一个已经超过本次所有权区间的存储续租。
	ctx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()
	renewed, err := l.infra.RenewNode(ctx, token, ttl)
	if err == nil && (renewed == nil || renewed.Key != token.Key || renewed.Generation != token.Generation) {
		err = lease.ErrConflict
	}
	l.renewErr = err
	return renewed, err
}

func (l *nodeLease) commit(ctx context.Context, token lease.Token, c NodeCommit) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ending = true
	// Runner may not yet have canceled ctx after Renew returned its error.
	// Check the error under the same mutex before accepting any outcome.
	//
	// Renew 返回错误后，Runner 可能尚未取消 ctx。因此在接受任何结果前，需要在
	// 同一把互斥锁下检查该错误。
	if l.renewErr != nil {
		return nil, &InfrastructureError{"RenewNode", l.renewErr}
	}
	ready, err := l.infra.Commit(ctx, token, c)
	if err != nil {
		return nil, &InfrastructureError{"Commit", err}
	}
	l.committed = true
	return ready, nil
}

func (l *nodeLease) Release(ctx context.Context, token lease.Token) error {
	l.mu.Lock()
	committed := l.committed
	l.mu.Unlock()
	if committed {
		return nil
	}
	return l.infra.ReleaseNode(ctx, token)
}

// executionResult is normalized while the node lease is held. Result getters
// are evaluated once; arbitrary result implementations do not enter storage.
type executionResult struct {
	raw      NodeResult
	input    map[string]any
	output   map[string]any
	outgoing []string
	done     bool
}

func (e *Engine[R]) execute(parent context.Context, task *NodeTask, runtime R) (published PublishedNode[R], result executionResult, err error) {
	ctx, cancel := context.WithTimeout(context.WithValue(parent, nodeIDKey{}, task.NodeID), e.options.NodeTimeout)
	defer cancel()
	var started time.Time
	result.done = true
	defer func() {
		if value := recover(); value != nil {
			result.raw = nil
			err = fmt.Errorf("%w: panic: %v", ErrInvalidResult, value)
		}
		if !started.IsZero() {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			e.finish(ctx, task, time.Since(started), result, err)
		}
	}()
	published, err = e.definitions.LoadNode(ctx, task.NodeID)
	if err != nil {
		return nil, result, &InfrastructureError{"LoadNode", err}
	}
	if isNil(published) || published.GetID() != task.NodeID {
		return nil, result, &InfrastructureError{"LoadNode", ErrInvalidDefinition}
	}
	if !task.State.Enabled {
		return published, result, nil
	}
	node := published.GetNode()
	if isNil(node) {
		return published, result, &InfrastructureError{"LoadNode", ErrInvalidDefinition}
	}
	event := NodeEvent{NodeID: task.NodeID, Attempt: task.State.Generation}
	e.options.Hooks.start(ctx, event)
	started = time.Now()
	result.raw = node.Run(ctx, runtime)
	if isNil(result.raw) {
		result.raw = nil
		return published, result, fmt.Errorf("%w: nil result", ErrInvalidResult)
	}
	result.input = maps.Clone(result.raw.GetInput())
	result.output = maps.Clone(result.raw.GetOutput())
	err = result.raw.GetError()
	if ctx.Err() != nil {
		return published, result, ctx.Err()
	}
	if err != nil {
		return published, result, err
	}
	asyncResult, async := result.raw.(AsyncNodeResult)
	branchResult, branch := result.raw.(BranchNodeResult)
	if async && branch {
		return published, result, fmt.Errorf("%w: async and branch results are mutually exclusive", ErrInvalidResult)
	}
	if branch {
		result.outgoing = slices.Clone(branchResult.GetOutgoing())
		if err := validateIDs(result.outgoing); err != nil {
			return published, result, fmt.Errorf("%w: outgoing: %v", ErrInvalidResult, err)
		}
		for _, id := range result.outgoing {
			if !slices.Contains(published.GetOutgoing(), id) {
				return published, result, fmt.Errorf("%w: unknown outgoing node %q", ErrInvalidResult, id)
			}
		}
	} else if async {
		result.done = asyncResult.IsDone()
	}
	if !branch && result.done {
		result.outgoing = slices.Clone(published.GetOutgoing())
	}
	return published, result, nil
}

func validateIDs(ids []string) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			return ErrInvalidDefinition
		}
		seen[id] = true
	}
	return nil
}

func (e *Engine[R]) finish(ctx context.Context, task *NodeTask, duration time.Duration, result executionResult, err error) {
	if e.options.Hooks.OnFinish == nil {
		return
	}
	event := NodeEvent{
		NodeID:   task.NodeID,
		Attempt:  task.State.Generation,
		Duration: duration,
	}
	switch {
	case err != nil:
		event.Outcome, event.Err = Failed, err
	case !result.done:
		event.Outcome = Suspended
	case !task.State.Enabled:
		event.Outcome = Skipped
	default:
		event.Outcome = Succeeded
	}
	e.options.Hooks.finish(ctx, event)
}

func (e *Engine[R]) arrivals(node PublishedNode[R], status Status, outgoing []string) map[string]bool {
	result := map[string]bool{}
	for _, id := range node.GetOutgoing() {
		active := status == Succeeded && slices.Contains(outgoing, id)
		result[id] = active
	}
	return result
}
