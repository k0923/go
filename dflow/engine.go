package dflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
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
type Engine struct {
	definitions DefinitionProvider
	infra       Infrastructure
	options     Options
}

// Executed reports the outcome of one Drive step. Ready lists downstream nodes
// that became ready from THIS step, for the caller to drive next. Suspension
// carries the business-defined payload when Status is Suspended; the caller
// type-asserts it to its own type and decides what to do (spawn a child, request
// approval, loop, ...), then later calls Wakeup and drives the node again.
//
// Executed 报告一次 Drive 步骤的结果。Ready 列出本步骤中新变为 ready 的下游
// 节点，供调用方继续驱动。当 Status 为 Suspended 时，Suspension 携带业务定义
// 的载荷；调用方将它断言为自己的类型并决定下一步动作（创建子流程、请求审批、
// 循环等），随后调用 Wakeup 并再次驱动该节点。
type Executed struct {
	NodeID     string
	Status     Status
	Output     map[string]any
	Suspension Suspension
	Token      string
	Ready      []string
	// Err is the business failure when Status is Failed. Drive also returns it as
	// the function error so a caller loop stops on failure (fail-fast).
	//
	// Err 是 Status 为 Failed 时的业务失败。Drive 也会将它作为函数错误返回，
	// 以便调用方循环按 fail-fast 方式停止。
	Err error
}

// NewEngine is the in-process publication convenience API.
//
// NewEngine 是进程内发布的便捷 API。
func NewEngine(graph *Graph, infra Infrastructure, options Options) (*Engine, error) {
	if graph == nil {
		return nil, ErrInvalidDefinition
	}
	return NewEngineWithProvider(graph, infra, options)
}

// NewEngineWithProvider loads executable nodes from an immutable publication.
//
// NewEngineWithProvider 从不可变发布版本加载可执行节点。
func NewEngineWithProvider(provider DefinitionProvider, infra Infrastructure, options Options) (*Engine, error) {
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
	return &Engine{provider, infra, options}, nil
}

// Start initializes or verifies the execution and returns the unique start node
// ID to drive. It is idempotent and takes no lease: concurrent calls from several
// instances bind the same execution once.
//
// Start 初始化或校验一次执行，并返回要驱动的唯一 start 节点 ID。它是幂等的，
// 且不持有租约：多个实例并发调用只会绑定同一次执行。
func (e *Engine) Start(ctx context.Context, input map[string]any) (string, error) {
	if input == nil {
		input = map[string]any{}
	}
	manifest, err := e.definitions.Manifest(ctx)
	if err != nil {
		return "", err
	}
	if err := e.infra.Initialize(ctx, manifest, input); err != nil {
		return "", &InfrastructureError{"Initialize", err}
	}
	return manifest.Start, nil
}

// Recover returns the node IDs that are currently drivable without reinitializing
// the execution: freshly ready nodes and nodes whose previous owner lost its
// lease. Nodes executing under a live lease elsewhere are not listed. It takes no
// lease. Use it after process loss to repopulate a work queue.
//
// Recover 在不重新初始化执行的情况下返回当前可驱动的节点 ID：新变为 ready 的
// 节点，以及前任 owner 丢失租约的节点。正在其他位置由有效租约执行的节点不会
// 被列出。它不持有租约。进程丢失后可用它重新填充工作队列。
func (e *Engine) Recover(ctx context.Context) ([]string, error) {
	ready, err := e.infra.ReadyNodes(ctx)
	if err != nil {
		return nil, &InfrastructureError{"ReadyNodes", err}
	}
	return ready, nil
}

// Drive executes exactly one node and persists its outcome. It holds only that
// node's lease — claimed, renewed while Run/Resume executes, and ended by Commit
// — so distinct nodes of one run proceed in parallel, across processes too.
// Duplicate deliveries of the same node are safe: only one claim wins.
//
// A node that is pending, suspended, terminal or owned by a live lease yields a
// zero Executed and nil error. A business node failure returns Status Failed with
// the error (fail-fast): no downstream node becomes ready. Infrastructure/lease
// errors are uncertain and should be retried by driving the same node again.
//
// Drive 精确执行一个节点并持久化其结果。它只持有该节点的租约：先 claim，
// 在 Run/Resume 执行期间续租，并在 Commit 时结束。因此同一 run 的不同节点
// 可以跨进程并行推进。重复投递同一节点是安全的：只有一个 claim 会获胜。
//
// 如果节点处于 pending、suspended、terminal，或正被有效租约持有，Drive 会返回
// 零值 Executed 和 nil error。业务节点失败会返回 Status Failed 和对应错误
// （fail-fast）：不会有下游节点变为 ready。Infrastructure/lease 错误的结果不确定，
// 应通过再次驱动同一节点来重试。
func (e *Engine) Drive(ctx context.Context, nodeID string) (executed Executed, err error) {
	return e.drive(ctx, nodeID, "")
}

func (e *Engine) drive(ctx context.Context, nodeID, expectedToken string) (executed Executed, err error) {
	if nodeID == "" {
		return Executed{}, ErrInvalidDefinition
	}
	owner := &nodeLease{infra: e.infra}
	runner, err := lease.NewRunner(owner, e.options.Lease)
	if err != nil {
		return Executed{}, err
	}
	err = runner.Run(ctx, nodeID, func(ctx context.Context, token lease.Token) error {
		task := owner.task
		// ClaimNode's returned snapshot is protected by this lease. A stale
		// resume may briefly claim and release the current node, but it can never
		// invoke Resume for a different suspension round.
		//
		// ClaimNode 返回的快照受此租约保护。过期 resume 可能短暂 claim 并释放
		// 当前节点，但绝不会调用另一个挂起轮次的 Resume。
		if expectedToken != "" && (task.State.Suspension == nil || task.State.Suspension.Token != expectedToken) {
			return nil
		}
		executed.NodeID = nodeID
		e.options.Hooks.claim(ctx, NodeEvent{NodeID: nodeID, Attempt: task.State.Generation})
		node, result, execErr := e.execute(ctx, task)
		if ctx.Err() != nil {
			// Lease lost or caller cancelled: the outcome is uncertain, not a
			// business failure. Leave the node Running for a retry/reclaim.
			//
			// 租约丢失或调用方取消时，结果是不确定的，而不是业务失败。保留节点
			// Running 状态，等待重试或重新 claim。
			return &InfrastructureError{"Execute", context.Cause(ctx)}
		}
		next := task.State
		var arrivals []Arrival
		if execErr != nil {
			var infraErr *InfrastructureError
			if errors.As(execErr, &infraErr) {
				return execErr
			}
			next.Status, next.Error = Failed, execErr.Error()
		} else if result.Suspend != nil {
			next.Status = Suspended
			next.Suspension = &SuspensionState{Token: uuid.NewString(), Payload: result.Suspend}
		} else {
			next.Status = Succeeded
			if !task.State.Enabled {
				next.Status = Skipped
			}
			next.Output, next.Ports, next.Suspension = result.Output, result.Ports, nil
			arrivals, err = e.arrivals(ctx, nodeID, node, next)
			if err != nil {
				return err
			}
		}
		next.Ready = false
		ready, commitErr := owner.commit(ctx, token, NodeCommit{nodeID, task.State.Version, next, arrivals})
		if commitErr != nil {
			return commitErr
		}
		executed.Status = next.Status
		executed.Output = next.Output
		executed.Ready = ready
		if next.Suspension != nil {
			executed.Suspension = next.Suspension.Payload
			executed.Token = next.Suspension.Token
		}
		if execErr != nil {
			executed.Err = execErr
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

func (e *Engine) execute(parent context.Context, task *NodeTask) (published *PublishedNode, result Result, err error) {
	ctx, cancel := context.WithTimeout(context.WithValue(parent, nodeIDKey{}, task.NodeID), e.options.NodeTimeout)
	defer cancel()
	var started time.Time
	var duration time.Duration
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("panic: %v", value)
		}
		if !started.IsZero() {
			if ctx.Err() != nil {
				result, err = Result{}, ctx.Err()
			}
			if duration == 0 {
				// duration includes a panicking call.
				//
				// duration 包含发生 panic 的调用耗时。
				duration = time.Since(started)
			}
			e.finish(ctx, task, duration, result, err)
		}
	}()
	published, err = e.definitions.LoadNode(ctx, task.NodeID)
	if err != nil {
		return nil, result, &InfrastructureError{"LoadNode", err}
	}
	if published == nil || published.Spec.ID != task.NodeID {
		return nil, result, &InfrastructureError{"LoadNode", ErrInvalidDefinition}
	}
	if !task.State.Enabled {
		return published, result, nil
	}
	// Skipped nodes have no executable instance.
	//
	// 被跳过的节点没有可执行实例。
	node := published.Spec.Node
	if isNil(node) {
		return published, result, &InfrastructureError{"LoadNode", ErrInvalidDefinition}
	}
	nodeInput := task.State.Input
	if task.State.Suspension == nil {
		upstream := map[string]NodeState{}
		for _, parameter := range published.Spec.Inputs {
			if isNil(parameter) {
				return published, result, ErrInvalidReference
			}
			refs, refErr := parameter.References()
			if refErr != nil {
				return published, result, refErr
			}
			for _, path := range refs {
				if err := validatePath(path); err != nil {
					return published, result, err
				}
				if path[0] == InputSource {
					continue
				}
				if _, loaded := upstream[path[0]]; loaded {
					continue
				}
				state, readErr := e.infra.GetNode(ctx, path[0])
				if readErr != nil {
					return published, result, &InfrastructureError{"GetNode", readErr}
				}
				if state != nil {
					upstream[path[0]] = *state
				}
			}
		}
		nodeInput, err = published.Spec.Inputs.resolveWithInput(e.infra, ctx, upstream)
		if err != nil {
			return published, result, err
		}
		if _, start := node.(*StartNode); start && len(published.Spec.Inputs) == 0 {
			value, readErr := e.infra.GetInput(ctx)
			if readErr != nil {
				return published, result, &InfrastructureError{"GetInput", readErr}
			}
			var ok bool
			nodeInput, ok = value.(map[string]any)
			if !ok {
				return published, result, ErrInvalidDefinition
			}
		}
		task.State.Input = nodeInput
	}
	if suspension := task.State.Suspension; suspension != nil {
		response, readErr := e.infra.GetResponse(ctx, task.NodeID, suspension.Token)
		if readErr != nil {
			return published, result, &InfrastructureError{"GetResponse", readErr}
		}
		if response == nil {
			return published, result, &InfrastructureError{"GetResponse", ErrWaitNotFound}
		}
		resumable, ok := node.(ResumableNode)
		if !ok {
			return published, result, ErrInvalidSuspension
		}
		e.options.Hooks.start(ctx, NodeEvent{NodeID: task.NodeID, Attempt: task.State.Generation, IsResume: true, Token: suspension.Token})
		started = time.Now()
		result, err = resumable.Resume(ctx, maps.Clone(nodeInput), ResumeInput{Token: suspension.Token, Suspension: suspension.Payload, Response: response.Value})
	} else {
		e.options.Hooks.start(ctx, NodeEvent{NodeID: task.NodeID, Attempt: task.State.Generation})
		started = time.Now()
		result, err = node.Run(ctx, maps.Clone(nodeInput))
	}
	duration = time.Since(started)
	if ctx.Err() != nil {
		return published, Result{}, ctx.Err()
	}
	if err != nil {
		return published, result, err
	}
	if result.Suspend != nil {
		if _, ok := node.(ResumableNode); !ok || isNil(result.Suspend) || result.Suspend.Type() == "" || result.Output != nil || result.Ports != nil {
			return published, result, ErrInvalidSuspension
		}
	}
	for _, port := range result.Ports {
		found := false
		for _, edge := range published.Outgoing {
			if edge.SourcePortID == port {
				found = true
			}
		}
		if !found {
			return published, result, fmt.Errorf("unknown outgoing port %q", port)
		}
	}
	return published, result, err
}

func (e *Engine) finish(ctx context.Context, task *NodeTask, duration time.Duration, result Result, err error) {
	if e.options.Hooks.OnFinish == nil {
		return
	}
	event := NodeEvent{
		NodeID:   task.NodeID,
		Attempt:  task.State.Generation,
		IsResume: task.State.Suspension != nil,
		Duration: duration,
	}
	if task.State.Suspension != nil {
		event.Token = task.State.Suspension.Token
	}
	switch {
	case err != nil:
		event.Outcome, event.Err = Failed, err
	case result.Suspend != nil:
		event.Outcome = Suspended
	case !task.State.Enabled:
		event.Outcome = Skipped
	default:
		event.Outcome = Succeeded
	}
	e.options.Hooks.finish(ctx, event)
}

func (e *Engine) arrivals(ctx context.Context, source string, node *PublishedNode, state NodeState) ([]Arrival, error) {
	targets := map[string]bool{}
	var order []string
	for _, edge := range node.Outgoing {
		if _, exists := targets[edge.TargetNodeID]; !exists {
			order = append(order, edge.TargetNodeID)
		}
		active := state.Status == Succeeded && (state.Ports == nil || slices.Contains(state.Ports, edge.SourcePortID))
		targets[edge.TargetNodeID] = targets[edge.TargetNodeID] || active
	}
	var result []Arrival
	for _, id := range order {
		incoming, err := e.definitions.Incoming(ctx, id)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(incoming, source) {
			return nil, ErrInvalidDefinition
		}
		result = append(result, Arrival{TargetNodeID: id, Incoming: incoming, Active: targets[id]})
	}
	return result, nil
}
