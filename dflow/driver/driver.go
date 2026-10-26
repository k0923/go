// Package driver is the reference in-process driver for dflow. The engine itself
// only advances one node per Drive call; this package shows the simplest way to
// drive a whole DAG to quiescence by recursing on the downstream nodes each step
// returns. Production systems may replace it with a queue: enqueue each ready
// node ID and let workers call engine.Drive independently.
//
// Package driver 是 dflow 的参考进程内驱动。引擎本身每次 Drive 只推进一个节点；
// 本包展示了最简单的整图驱动到静止方式：递归驱动每一步返回的下游节点。
// 生产系统可以用队列替代它：把每个 ready 节点 ID 入队，并让 worker 独立调用
// engine.Drive。
package driver

import (
	"context"

	"github.com/k0923/go/dflow"
)

// Run starts (or verifies) the execution and drives from the unique start node to
// quiescence, serially and in-process. The returned State contains only the
// nodes this call touched, mirroring the engine's per-step reporting.
//
// Run 启动（或校验）执行，并在进程内串行地从唯一 start 节点驱动到静止。返回的
// State 只包含本次调用触碰过的节点，与引擎的单步报告模型保持一致。
func Run(ctx context.Context, e *dflow.Engine, input map[string]any) (*dflow.State, error) {
	start, err := e.Start(ctx, input)
	if err != nil {
		return nil, err
	}
	state := &dflow.State{Nodes: map[string]dflow.NodeState{}}
	return state, driveToQuiescence(ctx, e, state, []string{start})
}

// Drain recovers drivable work after process loss and drives it to quiescence.
//
// Drain 在进程丢失后恢复可驱动工作，并将其驱动到静止。
func Drain(ctx context.Context, e *dflow.Engine) (*dflow.State, error) {
	state := &dflow.State{Nodes: map[string]dflow.NodeState{}}
	return state, driveToQuiescence(ctx, e, state, nil)
}

// Resume optionally delivers a callback (pass one response, including nil), then
// drives the named node and its reachable successors. Without a response it
// consumes a persisted Wake. A stale token or terminal node drives nothing. It
// is deliberately targeted: it does not sweep unrelated ready nodes.
//
// Resume 可选地投递回调（传入一个响应，包括 nil），然后驱动指定节点及其可达后继。
// 不传响应时，它会消费已持久化的 Wake。过期 token 或 terminal 节点不会驱动任何
// 内容。它是刻意定向的：不会扫描无关的 ready 节点。
func Resume(ctx context.Context, e *dflow.Engine, nodeID, token string, response ...any) (*dflow.State, error) {
	if nodeID == "" || token == "" || len(response) > 1 {
		return nil, dflow.ErrWaitNotFound
	}
	if len(response) == 1 {
		if _, err := e.Wakeup(ctx, nodeID, token, response[0]); err != nil {
			return nil, err
		}
	}
	state := &dflow.State{Nodes: map[string]dflow.NodeState{}}
	executed, err := e.Resume(ctx, nodeID, token)
	if executed.Status != "" {
		state.Nodes[executed.NodeID] = nodeState(executed)
	}
	if err != nil {
		return state, err
	}
	return state, drive(ctx, e, state, executed.Ready)
}

// driveToQuiescence drains the given ready set, then repeatedly asks the engine
// which nodes remain drivable until none do. The second phase catches nodes that
// became ready out of band, such as a suspension resolved during its own commit.
//
// driveToQuiescence 先清空给定 ready 集合，然后反复询问引擎还有哪些节点可驱动，
// 直到没有为止。第二阶段会捕获带外变为 ready 的节点，例如在自身 commit 期间被
// 解析的 suspension。
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
//
// drive 执行串行广度优先扫描：先执行一个节点，再跟随它置为 ready 的下游节点。
// 它会在第一个节点失败处停止（fail-fast）。
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
