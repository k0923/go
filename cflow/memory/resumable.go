package memory

import (
	"context"
	"encoding/json"

	"github.com/k0923/go/cflow"
)

var _ cflow.FlowInfrastructure = (*Infrastructure)(nil)

func copyFlow(item *cflow.FlowExecution) *cflow.FlowExecution {
	if item == nil {
		return nil
	}
	c := *item
	c.Path = append(cflow.Path(nil), item.Path...)
	c.Input = append(json.RawMessage(nil), item.Input...)
	c.Output = append(json.RawMessage(nil), item.Output...)
	if item.Failure != nil {
		failure := *item.Failure
		c.Failure = &failure
	}
	return &c
}

func copyLoop(loop *cflow.LoopState) *cflow.LoopState {
	if loop == nil {
		return nil
	}
	c := *loop
	c.Path = append(cflow.Path(nil), loop.Path...)
	if loop.Failure != nil {
		failure := *loop.Failure
		c.Failure = &failure
	}
	return &c
}

func (m *Infrastructure) GetLoop(ctx context.Context, key string) (*cflow.LoopState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return copyLoop(m.loops[key]), nil
}

func (m *Infrastructure) GetFlow(ctx context.Context, key string) (*cflow.FlowExecution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return copyFlow(m.flows[key]), nil
}

func (m *Infrastructure) StartFlow(ctx context.Context, spec cflow.FlowExecution) (*cflow.FlowExecution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cflow.ValidateFlow(spec); err != nil {
		return nil, err
	}
	if current := m.flows[spec.Key]; current != nil {
		if !cflow.SameFlow(spec, *current) {
			return nil, cflow.ErrChanged
		}
		return copyFlow(current), nil
	}
	if m.records[spec.Key] != nil || m.loops[spec.Key] != nil || m.waitGroups[spec.Key] != nil {
		return nil, cflow.ErrChanged
	}
	spec.Status, spec.Version, spec.Output, spec.Failure = cflow.Running, 1, nil, nil
	m.flows[spec.Key], m.owners[spec.Key] = copyFlow(&spec), spec.Key
	m.boundary(spec.ParentKey).flows[spec.Key] = true
	m.flowWake(spec.Key, spec.Key, 1)
	return copyFlow(&spec), nil
}

func (m *Infrastructure) flowWake(key, owner string, version int64) {
	id := cflow.FlowWakeID(key, version)
	m.messages[id] = Notification{ID: id, Key: key, ExecutionKey: owner, Purpose: WakeNotification, AvailableAt: m.now()}
}

func (m *Infrastructure) StartLoop(ctx context.Context, spec cflow.LoopSpec, items []cflow.FlowExecution) (*cflow.LoopState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cflow.ValidateLoopBatch(spec, items); err != nil {
		return nil, err
	}
	if current := m.loops[spec.Key]; current != nil {
		if !cflow.SameLoop(current.LoopSpec, spec) {
			return nil, cflow.ErrChanged
		}
		return copyLoop(current), nil
	}
	occupied := func(key string) bool {
		return m.records[key] != nil || m.flows[key] != nil || m.loops[key] != nil || m.waitGroups[key] != nil
	}
	if occupied(spec.Key) {
		return nil, cflow.ErrChanged
	}
	for _, item := range items {
		if occupied(item.Key) {
			return nil, cflow.ErrChanged
		}
	}
	loop := cflow.NewLoopState(spec)
	keys := make([]string, len(items))
	for i := range items {
		item := copyFlow(&items[i])
		item.Status, item.Version, item.Output, item.Failure = cflow.Queued, 1, nil, nil
		if i < loop.AdmittedCount {
			item.Status = cflow.Running
			m.flowWake(item.Key, item.Key, 1)
		}
		keys[i], m.flows[item.Key] = item.Key, item
		m.owners[item.Key] = item.Key
	}
	m.owners[spec.Key] = spec.ParentKey
	m.loopItems[spec.Key] = keys
	m.loops[spec.Key] = copyLoop(loop)
	m.boundary(spec.ParentKey).loops[spec.Key] = true
	return copyLoop(loop), nil
}

func (m *Infrastructure) CommitFlow(ctx context.Context, token cflow.Lease, result *cflow.FlowExecution, expected int64) (*cflow.FlowExecution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result == nil || token.Key != result.Key {
		return nil, cflow.ErrInvalid
	}
	if !m.valid(token) {
		return nil, cflow.ErrConflict
	}
	current := copyFlow(m.flows[result.Key])
	if current == nil {
		return nil, cflow.ErrNotFound
	}
	if current.Status == cflow.Completed || current.Status == cflow.Failed {
		return current, nil
	}
	loop := copyLoop(m.loops[current.LoopKey])
	next, err := cflow.ApplyFlowCompletion(loop, current, result, expected)
	if err != nil {
		return nil, err
	}
	m.flows[current.Key] = copyFlow(current)
	if current.LoopKey == "" {
		wakeKey := current.ParentKey
		id := cflow.FlowCompletionWakeID(current.Key, current.Version)
		m.messages[id] = Notification{ID: id, Key: wakeKey, ExecutionKey: current.ParentKey,
			Purpose: WakeNotification, AvailableAt: m.now()}
		return copyFlow(current), nil
	}
	m.loops[loop.Key] = copyLoop(loop)
	if next >= 0 {
		item := m.flows[m.loopItems[loop.Key][next]]
		item.Status, item.Version = cflow.Running, item.Version+1
		m.flowWake(item.Key, item.Key, item.Version)
	}
	if loop.Status != cflow.Suspended {
		m.flowWake(loop.Key, loop.ParentKey, loop.Version)
	}
	return copyFlow(current), nil
}

func (m *Infrastructure) LoopResults(ctx context.Context, key string) ([]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loop := m.loops[key]
	if loop == nil {
		return nil, cflow.ErrNotFound
	}
	if loop.Status != cflow.Completed && loop.Status != cflow.Failed {
		return nil, cflow.ErrConflict
	}
	return m.loopOutputs(loop), nil
}

// loopOutputs copies every output of a completed loop in index order; failed
// and DiscardOutputs loops have none. Callers hold m.mu.
// loopOutputs 按下标复制成功循环的全部输出；失败或 DiscardOutputs 循环没有输出。调用方持有 m.mu。
func (m *Infrastructure) loopOutputs(loop *cflow.LoopState) []json.RawMessage {
	if loop.Status != cflow.Completed || loop.DiscardOutputs {
		return nil
	}
	results := make([]json.RawMessage, loop.Total)
	for i := range results {
		results[i] = append(json.RawMessage(nil), m.flows[m.loopItems[loop.Key][i]].Output...)
	}
	return results
}
