package dflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	graph   *Graph
	infra   Infrastructure
	leases  *lease.Runner
	options Options
}

func NewEngine(graph *Graph, infra Infrastructure, options Options) (*Engine, error) {
	if graph == nil || isNil(infra) {
		return nil, fmt.Errorf("dflow: graph and infrastructure are required")
	}
	if options.MaxConcurrency == 0 {
		options.MaxConcurrency = 4
	}
	if options.NodeTimeout == 0 {
		options.NodeTimeout = 5 * time.Minute
	}
	if options.MaxConcurrency < 1 || options.NodeTimeout < 0 {
		return nil, fmt.Errorf("dflow: invalid concurrency or timeout")
	}
	runner, err := lease.NewRunner(infra, options.Lease)
	if err != nil {
		return nil, err
	}
	return &Engine{graph, infra, runner, options}, nil
}

// Run starts or resumes a run with the same definition and input. Successful and
// skipped nodes are reused. Failed/interrupted nodes are retried on the next call.
// When no nodes can execute, Run returns their snapshot and releases the lease.
// A nil error means this drive stopped normally, not that the workflow finished.
// A run-level renewable lease prevents concurrent drivers; nodes within a run
// execute concurrently. Cancellation waits for started nodes to return, so custom
// nodes MUST honor ctx. The returned snapshot is the last confirmed durable state.
func (e *Engine) Run(ctx context.Context, input map[string]any) (state *State, err error) {
	normalized, err := cloneJSON(input)
	if err != nil {
		return nil, fmt.Errorf("dflow input: %w", err)
	}
	if normalized == nil {
		normalized = map[string]any{}
	}
	err = e.leases.Run(ctx, runLeaseKey, func(ctx context.Context, token lease.Token) error {
		loaded, err := e.infra.Load(ctx)
		if err != nil {
			return &InfrastructureError{"Load", err}
		}
		if loaded == nil {
			loaded = &State{
				DefinitionHash: e.graph.hash, Input: normalized,
				Nodes: make(map[string]NodeState, len(e.graph.nodes)),
			}
			for id := range e.graph.nodes {
				loaded.Nodes[id] = NodeState{Status: Pending}
			}
			if err := e.save(ctx, token, loaded); err != nil {
				return err
			}
		} else {
			left, _ := json.Marshal(loaded.Input)
			right, _ := json.Marshal(normalized)
			if loaded.DefinitionHash != e.graph.hash || !bytes.Equal(left, right) {
				return ErrRunMismatch
			}
			if err := e.validateState(loaded); err != nil {
				return &InfrastructureError{"Load", err}
			}
		}
		state, err = cloneJSON(loaded)
		if err != nil {
			return &InfrastructureError{"Load", err}
		}
		return e.drive(ctx, token, &state)
	})
	return state, err
}

func (e *Engine) validateState(state *State) error {
	if state.Version < 1 || len(state.Nodes) != len(e.graph.nodes) {
		return fmt.Errorf("invalid stored state")
	}
	for id := range e.graph.nodes {
		node, ok := state.Nodes[id]
		if !ok {
			return fmt.Errorf("stored node %q is missing", id)
		}
		switch node.Status {
		case Pending, Running, Failed, Suspended:
			if node.Status == Suspended && node.Suspension == nil {
				return fmt.Errorf("suspended node %q has no suspension", id)
			}
		case Succeeded, Skipped:
			if node.Suspension != nil {
				return fmt.Errorf("terminal node %q retains a suspension", id)
			}
		default:
			return fmt.Errorf("invalid stored node status %q", node.Status)
		}
		if node.Suspension != nil {
			if _, ok := e.graph.nodes[id].(ResumableNode); !ok || node.Suspension.Token == "" {
				return fmt.Errorf("node %q cannot resume persisted suspension", id)
			}
		}
	}
	return nil
}

func (e *Engine) save(ctx context.Context, token lease.Token, state *State) error {
	if err := e.infra.Save(ctx, token, state, state.Version); err != nil {
		return &InfrastructureError{"Save", err}
	}
	state.Version++
	return nil
}

type completion struct {
	id     string
	result Result
	err    error
}

func (e *Engine) drive(parent context.Context, token lease.Token, durable **State) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	state, err := cloneJSON(*durable)
	if err != nil {
		return err
	}
	commit := func() error {
		if err := e.save(ctx, token, state); err != nil {
			return err
		}
		copy, err := cloneJSON(state)
		if err != nil {
			return err
		}
		*durable = copy
		return nil
	}
	for id, node := range state.Nodes {
		if node.Status == Running || node.Status == Failed {
			node.Status, node.Error = Pending, ""
			state.Nodes[id] = node
		}
	}
	completed := make(chan completion, e.options.MaxConcurrency)
	active := 0
	// Never return while a started custom node can still access its resources.
	defer func() {
		cancel()
		for active > 0 {
			<-completed
			active--
		}
	}()
	fail := func(id, stage string, cause error) error {
		nodeErr := &NodeError{id, stage, cause}
		node := state.Nodes[id]
		node.Status, node.Error = Failed, nodeErr.Error()
		state.Nodes[id] = node
		return errors.Join(nodeErr, commit())
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Responses live outside the snapshot, so concurrent Wakeup cannot be
		// overwritten by a snapshot commit. No polling while the run is idle.
		responses, err := e.refreshWaits(ctx, state)
		if err != nil {
			return err
		}
		changed := false
		for _, id := range e.graph.order {
			if active >= e.options.MaxConcurrency {
				break
			}
			if state.Nodes[id].Status != Pending {
				continue
			}
			ready, enabled := e.ready(id, state)
			if !ready {
				continue
			}
			if !enabled {
				state.Nodes[id] = NodeState{Status: Skipped}
				changed = true
				continue
			}
			input, err := e.graph.specs[id].Inputs.resolve(state.Input, state.Nodes)
			if err != nil {
				return fail(id, "resolve input", err)
			}
			// An unconfigured start node passes the entire workflow input through.
			if e.graph.specs[id].Type == "start" && len(e.graph.specs[id].Inputs) == 0 {
				input, err = cloneJSON(state.Input)
				if err != nil {
					return fail(id, "resolve input", err)
				}
			}
			previous := state.Nodes[id]
			if previous.Suspension != nil {
				input = previous.Input
			}
			state.Nodes[id] = NodeState{Status: Running, Input: input, Suspension: previous.Suspension}
			if err := commit(); err != nil {
				return err
			}
			// A custom node is allowed to mutate its private input, not stored input.
			nodeInput, err := cloneJSON(input)
			if err != nil {
				return fail(id, "copy input", err)
			}
			active++
			go func() {
				result, err := e.runNode(ctx, id, nodeInput, responses[id])
				completed <- completion{id: id, result: result, err: err}
			}()
		}
		if changed {
			if err := commit(); err != nil {
				return err
			}
		}
		if active == 0 {
			// Scheduling quiescence is not a workflow lifecycle decision.
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case done := <-completed:
			active--
			if done.err != nil {
				var storageErr *InfrastructureError
				if errors.As(done.err, &storageErr) {
					return done.err
				}
				return fail(done.id, "execute", done.err)
			}
			node := state.Nodes[done.id]
			if done.result.Suspend != nil {
				node.Status = Suspended
				node.Output, node.Ports = nil, nil
				node.Suspension = &SuspensionState{Token: uuid.NewString(), Suspension: *done.result.Suspend}
			} else {
				node.Status, node.Output, node.Ports = Succeeded, done.result.Output, done.result.Ports
				node.Suspension = nil
			}
			state.Nodes[done.id] = node
			if err := commit(); err != nil {
				return err
			}
		}
	}
}

func (e *Engine) refreshWaits(ctx context.Context, state *State) (map[string]*ResumeInput, error) {
	responses := make(map[string]*ResumeInput)
	for id, node := range state.Nodes {
		if node.Suspension == nil || (node.Status != Suspended && node.Status != Pending) {
			continue
		}
		response, err := e.infra.GetResponse(ctx, id, node.Suspension.Token)
		if err != nil {
			return nil, &InfrastructureError{"GetResponse", err}
		}
		if response == nil {
			if node.Status == Pending {
				return nil, &InfrastructureError{"GetResponse", fmt.Errorf("missing response for interrupted Resume of %q", id)}
			}
			continue
		}
		var value any
		if err := json.Unmarshal(response.Value, &value); err != nil {
			return nil, &InfrastructureError{"GetResponse", err}
		}
		resume, err := cloneJSON(ResumeInput{Token: node.Suspension.Token, State: node.Suspension.State, Response: value})
		if err != nil {
			return nil, &InfrastructureError{"GetResponse", err}
		}
		responses[id] = &resume
		node.Status = Pending
		state.Nodes[id] = node
	}
	return responses, nil
}

func (e *Engine) ready(id string, state *State) (ready, enabled bool) {
	edges := e.graph.incoming[id]
	enabled = len(edges) == 0
	for _, edge := range edges {
		node := state.Nodes[edge.SourceNodeID]
		if node.Status != Succeeded && node.Status != Skipped {
			return false, false
		}
		if node.Status == Succeeded && (node.Ports == nil || slices.Contains(node.Ports, edge.SourcePortID)) {
			enabled = true
		}
	}
	return true, enabled
}

func (e *Engine) runNode(parent context.Context, id string, input map[string]any, resume *ResumeInput) (result Result, err error) {
	ctx, cancel := context.WithTimeout(context.WithValue(parent, nodeIDKey{}, id), e.options.NodeTimeout)
	defer cancel()
	// User panics become node failures rather than crashing scheduler goroutines.
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("panic: %v", value)
		}
	}()
	if resume != nil {
		result, err = e.graph.nodes[id].(ResumableNode).Resume(ctx, input, *resume)
	} else {
		result, err = e.graph.nodes[id].Run(ctx, input)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if err != nil {
		return Result{}, err
	}
	if result.Suspend != nil {
		if _, ok := e.graph.nodes[id].(ResumableNode); !ok || result.Output != nil || result.Ports != nil {
			return Result{}, ErrInvalidSuspension
		}
	}
	for _, port := range result.Ports {
		found := false
		for _, edge := range e.graph.outgoing[id] {
			if edge.SourcePortID == port {
				found = true
				break
			}
		}
		if !found {
			return Result{}, fmt.Errorf("selected unknown outgoing port %q", port)
		}
	}
	return cloneJSON(result)
}
