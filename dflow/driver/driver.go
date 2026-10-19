// Package driver is the reference in-process driver for dflow. The engine itself
// only advances one node per Drive call; this package shows the simplest way to
// drive a whole DAG to quiescence by recursing on the downstream nodes each step
// returns. Production systems may replace it with a queue: enqueue each ready
// node ID and let workers call engine.Drive independently.
package driver

import (
	"context"

	"github.com/k0923/go/dflow"
)

// Run starts (or verifies) the execution and drives every ready node to
// quiescence, serially and in-process. The returned State contains only the
// nodes this call touched, mirroring the engine's per-step reporting.
func Run(ctx context.Context, e *dflow.Engine, input map[string]any) (*dflow.State, error) {
	ready, err := e.Start(ctx, input)
	if err != nil {
		return nil, err
	}
	state := &dflow.State{Nodes: map[string]dflow.NodeState{}}
	return state, driveToQuiescence(ctx, e, state, ready)
}

// Drain recovers drivable work after process loss and drives it to quiescence.
func Drain(ctx context.Context, e *dflow.Engine) (*dflow.State, error) {
	state := &dflow.State{Nodes: map[string]dflow.NodeState{}}
	return state, driveToQuiescence(ctx, e, state, nil)
}

// Resume optionally delivers a callback (pass one response, including nil), then
// drives the named node and its reachable successors. Without a response it
// consumes a persisted Wake. A stale token or terminal node drives nothing. It
// is deliberately targeted: it does not sweep unrelated ready nodes.
func Resume(ctx context.Context, e *dflow.Engine, nodeID, token string, response ...any) (*dflow.State, error) {
	if len(response) > 1 {
		return nil, dflow.ErrWaitNotFound
	}
	if len(response) == 1 {
		if _, err := e.Wakeup(ctx, nodeID, token, response[0]); err != nil {
			return nil, err
		}
	}
	state := &dflow.State{Nodes: map[string]dflow.NodeState{}}
	return state, drive(ctx, e, state, []string{nodeID})
}

// driveToQuiescence drains the given ready set, then repeatedly asks the engine
// which nodes remain drivable until none do. The second phase catches nodes that
// became ready out of band, such as a suspension resolved during its own commit.
func driveToQuiescence(ctx context.Context, e *dflow.Engine, state *dflow.State, ready []string) error {
	for {
		if err := drive(ctx, e, state, ready); err != nil {
			return err
		}
		recovered, err := e.Recover(ctx)
		if err != nil {
			return err
		}
		ready = recovered[:0:0]
		for _, id := range recovered {
			if node, seen := state.Nodes[id]; !seen || node.Status == dflow.Suspended {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			return nil
		}
	}
}

// drive runs a serial breadth-first sweep: execute a node, then follow the
// downstream nodes it made ready. It stops at the first node failure (fail-fast).
func drive(ctx context.Context, e *dflow.Engine, state *dflow.State, ready []string) error {
	queue := append([]string(nil), ready...)
	seen := map[string]bool{}
	for _, id := range queue {
		seen[id] = true
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		executed, err := e.Drive(ctx, id)
		if executed.Status != "" {
			state.Nodes[executed.NodeID] = nodeState(executed)
		}
		if err != nil {
			return err
		}
		for _, next := range executed.Ready {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return nil
}

func nodeState(executed dflow.Executed) dflow.NodeState {
	state := dflow.NodeState{Status: executed.Status, Output: executed.Output}
	if executed.Token != "" {
		state.Suspension = &dflow.SuspensionState{Token: executed.Token, Payload: executed.Suspension}
	}
	if executed.Err != nil {
		state.Error = executed.Err.Error()
	}
	return state
}
