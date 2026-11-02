// Package driver supplies optional serial, in-process scheduling for dflow.
// Business startup initializes the execution; this package only drives nodes.
package driver

import (
	"context"

	"github.com/k0923/go/dflow"
)

// State is the in-process driver observation returned by one Run or Drain call.
// It contains only nodes actually driven by this driver call.
//
// State 是一次 Run 或 Drain 调用返回的进程内 driver 观测结果，只包含本次调用实际推进过的节点。
type State struct {
	Nodes map[string]dflow.NodeState
}

// RecoveryReader lists nodes that are ready to be driven without initializing a
// new execution: freshly ready nodes, lost Running attempts and Failed retries.
// It must exclude terminal nodes, unresolved Pending nodes, live leases and
// Suspended nodes awaiting business scheduling.
//
// RecoveryReader 列出不需要初始化新执行即可推进的节点：新 ready 的节点、丢失租约的
// Running attempt，以及可重试的 Failed 节点。它必须排除终态节点、未就绪 Pending
// 节点、仍持有有效租约的节点，以及等待业务调度的 Suspended 节点。
type RecoveryReader interface {
	ReadyNodes(context.Context) ([]string, error)
}

// Run drives a supplied starting node and newly ready successors. Business must
// seed that node before calling Run. R provides both parameter resolution and
// business APIs; its resolver must read current committed outputs as this sweep
// advances. For per-node preloading, call Engine.Drive from business scheduling.
//
// An unfinished async node is recorded but never automatically polled again.
// The caller later calls Run with that node ID after scheduling a continuation.
func Run[R any](ctx context.Context, e *dflow.Engine[R], nodeID string, runtime R) (*State, error) {
	if nodeID == "" {
		return nil, dflow.ErrInvalidDefinition
	}
	state := &State{Nodes: map[string]dflow.NodeState{}}
	return state, drive(ctx, e, state, []string{nodeID}, runtime)
}

// Drain performs one recovery sweep using reader and drives the returned nodes.
// Suspended nodes await explicit business scheduling.
func Drain[R any](ctx context.Context, e *dflow.Engine[R], reader RecoveryReader, runtime R) (*State, error) {
	if reader == nil {
		return nil, dflow.ErrInvalidRuntime
	}
	ready, err := reader.ReadyNodes(ctx)
	if err != nil {
		return nil, &dflow.InfrastructureError{Op: "ReadyNodes", Cause: err}
	}
	state := &State{Nodes: map[string]dflow.NodeState{}}
	return state, drive(ctx, e, state, ready, runtime)
}

func drive[R any](ctx context.Context, e *dflow.Engine[R], state *State, ready []string, runtime R) error {
	queue := append([]string(nil), ready...)
	seen := map[string]bool{}
	for _, id := range queue {
		seen[id] = true
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		executed, err := e.Drive(ctx, id, runtime)
		if executed.Status != "" {
			node := dflow.NodeState{
				Status: executed.Status,
			}
			if executed.Result != nil {
				node.Input = executed.Result.GetInput()
				node.Output = executed.Result.GetOutput()
				if resultErr := executed.Result.GetError(); resultErr != nil {
					node.Error = resultErr.Error()
				}
			}
			state.Nodes[executed.NodeID] = node
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
