package dflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/k0923/go/lease"
)

const runLeaseKey = "dflow/run"

type Options struct {
	MaxConcurrency int
	NodeTimeout    time.Duration
	Lease          lease.Options
}

type Engine struct {
	definitions DefinitionProvider
	infra       Infrastructure
	leases      *lease.Runner
	options     Options
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
	if options.MaxConcurrency == 0 {
		options.MaxConcurrency = 4
	}
	if options.NodeTimeout == 0 {
		options.NodeTimeout = 5 * time.Minute
	}
	if options.MaxConcurrency < 1 || options.NodeTimeout < 0 {
		return nil, fmt.Errorf("dflow: invalid options")
	}
	runner, err := lease.NewRunner(infra, options.Lease)
	if err != nil {
		return nil, err
	}
	return &Engine{provider, infra, runner, options}, nil
}

// Run drains persistent ready tasks in memory. Returned Nodes contains only
// nodes touched by this drive. Business observation uses GetNode/SnapshotReader.
// Nil error means quiescence, not workflow completion.
func (e *Engine) Run(ctx context.Context, input map[string]any) (state *State, err error) {
	if input == nil {
		input = map[string]any{}
	}
	err = e.leases.Run(ctx, runLeaseKey, func(ctx context.Context, token lease.Token) error {
		manifest, err := e.definitions.Manifest(ctx)
		if err != nil {
			return err
		}
		if err := e.infra.Initialize(ctx, token, manifest, input); err != nil {
			return &InfrastructureError{"Initialize", err}
		}
		state = &State{DefinitionHash: manifest.Hash, Input: input, Nodes: map[string]NodeState{}}
		return e.drive(ctx, token, state, nil, false)
	})
	return state, err
}

type completion struct {
	task   *NodeTask
	node   *PublishedNode
	result Result
	err    error
}

func (e *Engine) drive(parent context.Context, token lease.Token, observed *State, first *NodeTask, scoped bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan completion, e.options.MaxConcurrency)
	active := 0
	queue := []string{}
	queued := map[string]bool{}
	defer func() {
		cancel()
		for active > 0 {
			<-done
			active--
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		for active < e.options.MaxConcurrency {
			var task *NodeTask
			var err error
			if first != nil {
				task = first
				first = nil
			} else if scoped {
				for len(queue) > 0 && task == nil {
					id := queue[0]
					queue = queue[1:]
					delete(queued, id)
					task, err = e.infra.ClaimNode(ctx, token, id)
					if err != nil {
						break
					}
				}
			} else {
				task, err = e.infra.ClaimReady(ctx, token)
			}
			if err != nil {
				return &InfrastructureError{"ClaimReady", err}
			}
			if task == nil {
				break
			}
			observed.Nodes[task.NodeID] = task.State
			active++
			go func() {
				node, result, err := e.execute(ctx, task)
				done <- completion{task, node, result, err}
			}()
		}
		if active == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-done:
			active--
			if err := ctx.Err(); err != nil {
				return err
			}
			task := result.task
			next := task.State
			var arrivals []Arrival
			if result.err != nil {
				var infraErr *InfrastructureError
				if errors.As(result.err, &infraErr) {
					return result.err
				}
				next.Status, next.Error = Failed, result.err.Error()
			} else if result.result.Suspend != nil {
				next.Status = Suspended
				next.Suspension = &SuspensionState{Token: uuid.NewString(), Payload: result.result.Suspend}
			} else {
				next.Status = Succeeded
				if !task.State.Enabled {
					next.Status = Skipped
				}
				next.Output, next.Ports, next.Suspension = result.result.Output, result.result.Ports, nil
				var err error
				arrivals, err = e.arrivals(ctx, task.NodeID, result.node, next)
				if err != nil {
					return err
				}
			}
			next.Ready = false
			if err := e.infra.Commit(ctx, token, NodeCommit{task.NodeID, task.State.Version, next, arrivals}); err != nil {
				return &InfrastructureError{"Commit", err}
			}
			next.Version++
			observed.Nodes[task.NodeID] = next
			if scoped {
				for _, arrival := range arrivals {
					if !queued[arrival.TargetNodeID] {
						queue = append(queue, arrival.TargetNodeID)
						queued[arrival.TargetNodeID] = true
					}
				}
			}
			if result.err != nil {
				return &NodeError{task.NodeID, "execute", result.err}
			}
		}
	}
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
		if err != nil {
			return published, result, err
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
		result, err = resumable.Resume(ctx, maps.Clone(nodeInput), ResumeInput{Token: suspension.Token, Suspension: suspension.Payload, Response: response.Value})
	} else {
		result, err = node.Run(ctx, maps.Clone(nodeInput))
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
