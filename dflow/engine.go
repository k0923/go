package dflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/k0923/go/lease"
)

type Options struct {
	NodeTimeout time.Duration
	// Lease configures the per-node execution lease (TTL, renewal at TTL/3, and
	// bounded release). A node whose owner stops renewing becomes claimable by any
	// instance once the TTL elapses.
	Lease lease.Options
	// Hooks observes node lifecycle transitions. It never affects scheduling.
	Hooks Hooks
}

// Engine drives one node per call and keeps no workflow status: Drive executes a
// single node, persists its outcome and returns the downstream nodes that became
// ready. Execution ownership is per node, so distinct nodes of one run may be
// driven concurrently from goroutines, workers or instances. The caller decides
// how to continue — recurse in-process, enqueue to a queue, or stop — giving the
// business full control over concurrency, ordering and failure handling.
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
type Executed struct {
	NodeID     string
	Status     Status
	Output     any
	Suspension Suspension
	Token      string
	Ready      []string
	// Err is the business failure when Status is Failed. Drive also returns it as
	// the function error so a caller loop stops on failure (fail-fast).
	Err error
}

// NewEngine is the in-process publication convenience API.
func NewEngine(graph *Graph, infra Infrastructure, options Options) (*Engine, error) {
	if graph == nil {
		return nil, ErrInvalidDefinition
	}
	return NewEngineWithProvider(graph, infra, options)
}

// NewEngineWithProvider loads executable nodes from an immutable publication.
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
	if _, err := lease.NewRunner(&nodeLease{}, options.Lease); err != nil {
		return nil, err
	}
	return &Engine{provider, infra, options}, nil
}

// Start initializes or verifies the execution and returns the unique start node
// ID to drive. It is idempotent and takes no lease: concurrent calls from several
// instances bind the same execution once.
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
func (e *Engine) Drive(ctx context.Context, nodeID string) (executed Executed, err error) {
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
		executed.NodeID = nodeID
		e.options.Hooks.claim(ctx, NodeEvent{NodeID: nodeID, Attempt: task.State.Generation})
		node, result, execErr := e.execute(ctx, task)
		if ctx.Err() != nil {
			// Lease lost or caller cancelled: the outcome is uncertain, not a
			// business failure. Leave the node Running for a retry/reclaim.
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
		owner.ending.Store(true)
		ready, commitErr := e.infra.Commit(ctx, token, NodeCommit{nodeID, task.State.Version, next, arrivals})
		if commitErr != nil {
			owner.ending.Store(false)
			return &InfrastructureError{"Commit", commitErr}
		}
		owner.committed.Store(true)
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
var errNotDrivable = errors.New("dflow: node not drivable")

// nodeLease adapts one Drive's node claim to lease.Runner, reusing its renewal,
// cancellation-on-loss and bounded release. Commit validates the lease and ends
// ownership in storage, so renewal pauses while Commit is in flight (a renewal
// racing the commit would otherwise report a spurious loss) and a committed
// lease is not released.
type nodeLease struct {
	infra             Infrastructure
	task              *NodeTask
	ending, committed atomic.Bool
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
	if l.ending.Load() {
		return &token, nil
	}
	return l.infra.RenewNode(ctx, token, ttl)
}

func (l *nodeLease) Release(ctx context.Context, token lease.Token) error {
	if l.committed.Load() {
		return nil
	}
	return l.infra.ReleaseNode(ctx, token)
}

func (e *Engine) execute(parent context.Context, task *NodeTask) (published *PublishedNode, result Result, err error) {
	ctx, cancel := context.WithTimeout(context.WithValue(parent, nodeIDKey{}, task.NodeID), e.options.NodeTimeout)
	defer cancel()
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("panic: %v", value)
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
	} // no executable instance for skipped nodes
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
		started := time.Now()
		result, err = resumable.Resume(ctx, maps.Clone(nodeInput), ResumeInput{Token: suspension.Token, Suspension: suspension.Payload, Response: response.Value})
		e.finish(ctx, task, started, result, err)
	} else {
		e.options.Hooks.start(ctx, NodeEvent{NodeID: task.NodeID, Attempt: task.State.Generation})
		started := time.Now()
		result, err = node.Run(ctx, maps.Clone(nodeInput))
		e.finish(ctx, task, started, result, err)
	}
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

func (e *Engine) finish(ctx context.Context, task *NodeTask, started time.Time, result Result, err error) {
	if e.options.Hooks.OnFinish == nil {
		return
	}
	event := NodeEvent{
		NodeID:   task.NodeID,
		Attempt:  task.State.Generation,
		IsResume: task.State.Suspension != nil,
		Duration: time.Since(started),
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
