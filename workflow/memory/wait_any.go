package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/k0923/go/workflow"
)

type memoryWaitGroup struct {
	requests []workflow.WaitRequest
	state    workflow.WaitAnyState
}

func (m *Infrastructure) WaitAny(ctx context.Context, key string, requests []workflow.WaitRequest) (*workflow.WaitAnyState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" || len(requests) == 0 {
		return nil, workflow.ErrInvalid
	}
	requests = append([]workflow.WaitRequest(nil), requests...)
	for i := range requests {
		if requests[i].Kind == "" {
			requests[i].Kind = workflow.SignalKind
		}
	}
	if existing := m.waitGroups[key]; existing != nil {
		if len(existing.requests) != len(requests) {
			return nil, workflow.ErrChanged
		}
		for i, request := range requests {
			if !workflow.SameWaitMember(existing.requests[i], request) {
				return nil, workflow.ErrChanged
			}
		}
		m.advanceWaitGroup(key)
		return copyWaitAnyState(existing.state), nil
	}
	if m.records[key] != nil {
		return nil, workflow.ErrChanged
	}
	seen := map[string]bool{key: true}
	for _, request := range requests {
		if request.Key == "" || seen[request.Key] {
			return nil, workflow.ErrInvalid
		}
		switch request.Kind {
		case workflow.SignalKind:
			if request.Error == "" && !json.Valid(request.Request) {
				return nil, workflow.ErrInvalid
			}
		case workflow.SleepKind:
		default:
			return nil, fmt.Errorf("%w: %s", workflow.ErrSignalUnsupported, request.Kind)
		}
		if m.records[request.Key] != nil || m.waitGroups[request.Key] != nil || m.waitOwners[request.Key] != "" {
			return nil, workflow.ErrChanged
		}
		seen[request.Key] = true
	}
	group := &memoryWaitGroup{state: workflow.WaitAnyState{Status: workflow.Suspended}}
	for _, request := range requests {
		raw := append(json.RawMessage(nil), request.Request...)
		storedRequest := request
		storedRequest.Request = raw
		storedRequest.Path = append(workflow.Path(nil), request.Path...)
		group.requests = append(group.requests, storedRequest)
		record := &workflow.Record{Key: request.Key, Path: append(workflow.Path(nil), request.Path...), Kind: request.Kind, Status: workflow.Suspended, Version: 1, Type: request.Type,
			Input: raw, StartedAt: m.now()}
		if request.Error != "" {
			record.Status, record.Error, record.EndedAt = workflow.Failed, request.Error, m.now()
		}
		if request.Kind == workflow.SleepKind {
			record.Until = request.Until
			if record.Until.IsZero() {
				record.Until = m.now().Add(request.Duration)
			}
		}
		m.records[request.Key] = record
		m.waitOwners[request.Key] = key
		if record.Status == workflow.Failed {
			group.state.Failures = append(group.state.Failures, workflow.WaitFailure{Key: record.Key, Message: record.Error})
			continue
		}
		group.state.Pending = append(group.state.Pending, pendingRecord(record))
		if record.Kind == workflow.SleepKind {
			m.enqueue(record, WakeNotification, record.Until)
		} else {
			m.enqueue(record, RequestNotification, m.now())
		}
	}
	m.waitGroups[key] = group
	if len(group.state.Pending) == 0 {
		group.state.Status = workflow.Failed
		id := fmt.Sprintf("%s|%s|failed", key, WakeNotification)
		m.messages[id] = Notification{ID: id, Key: key, Purpose: WakeNotification, AvailableAt: m.now()}
	}
	m.advanceWaitGroup(key)
	return copyWaitAnyState(group.state), nil
}

func copyWaitAnyState(state workflow.WaitAnyState) *workflow.WaitAnyState {
	state.Response = append(json.RawMessage(nil), state.Response...)
	state.Pending = append([]workflow.Pending(nil), state.Pending...)
	for i := range state.Pending {
		state.Pending[i].Path = append(workflow.Path(nil), state.Pending[i].Path...)
	}
	state.Failures = append([]workflow.WaitFailure(nil), state.Failures...)
	return &state
}

// settleWaitGroup runs under the same storage lock as Resolve. This makes
// winner selection, loser cancellation, message cleanup and wake atomic.
// settleWaitGroup 在与 Resolve 相同的存储锁下运行。这会使赢家选择、失败方取消、
// 消息清理和 wake 保持原子性。
func (m *Infrastructure) settleWaitGroup(groupKey string, winner *workflow.Record) {
	group := m.waitGroups[groupKey]
	group.state.Status = workflow.Completed
	group.state.WinnerKey = winner.Key
	group.state.WinnerKind = winner.Kind
	group.state.Response = append(json.RawMessage(nil), winner.Result...)
	group.state.Pending = nil
	for _, request := range group.requests {
		record := m.records[request.Key]
		m.removeWaitMessages(request.Key)
		if request.Key != winner.Key && record.Status == workflow.Suspended {
			cancelled := copyRecord(record)
			cancelled.Status = workflow.Cancelled
			cancelled.Version++
			cancelled.EndedAt = m.now()
			m.records[request.Key] = cancelled
			m.enqueue(cancelled, CancelNotification, m.now())
		}
	}
}

func (m *Infrastructure) removeWaitMessages(key string) {
	for id, message := range m.messages {
		if message.Key == key {
			delete(m.messages, id)
		}
	}
}

func (m *Infrastructure) FailWait(ctx context.Context, key string, version int64, message string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	groupKey := m.waitOwners[key]
	if groupKey == "" {
		return false, workflow.ErrNotFound
	}
	group := m.waitGroups[groupKey]
	m.advanceWaitGroup(groupKey)
	record := m.records[key]
	if group.state.Status != workflow.Suspended || record.Status != workflow.Suspended {
		return false, nil
	}
	if record.Version != version {
		return false, workflow.ErrConflict
	}
	failed := copyRecord(record)
	failed.Status = workflow.Failed
	failed.Version++
	failed.Error = message
	failed.EndedAt = m.now()
	m.records[key] = failed
	m.removeWaitMessages(key)
	group.state.Failures = append(group.state.Failures, workflow.WaitFailure{Key: key, Message: message})
	group.state.Pending = nil
	for _, request := range group.requests {
		if m.records[request.Key].Status == workflow.Suspended {
			group.state.Pending = append(group.state.Pending, pendingRecord(m.records[request.Key]))
		}
	}
	if len(group.state.Pending) == 0 {
		group.state.Status = workflow.Failed
		// Preserve a durable wake even when every member failed.
		// 即使每个成员都失败，也保留一个持久化 wake。
		id := fmt.Sprintf("%s|%s|failed", groupKey, WakeNotification)
		m.messages[id] = Notification{ID: id, Key: groupKey, Purpose: WakeNotification, AvailableAt: m.now()}
	}
	return true, nil
}

func pendingRecord(record *workflow.Record) workflow.Pending {
	return workflow.Pending{Key: record.Key, Path: append(workflow.Path(nil), record.Path...), Kind: record.Kind, Until: record.Until}
}

// A due timer is resolved under the same lock used by callbacks. Among due
// timers choose the earliest deadline, breaking ties by declaration order.
// No resident timer/goroutine is required: its durable WAKE schedules a Drive.
// 到期计时器会在回调使用的同一把锁下解析。在已到期的计时器中选择最早截止时间，
// 并按声明顺序打破平局。不需要常驻 timer/goroutine：其持久化 WAKE 会调度一次 Drive。
func (m *Infrastructure) advanceWaitGroup(key string) {
	group := m.waitGroups[key]
	if group.state.Status != workflow.Suspended {
		return
	}
	var earliest *workflow.Record
	now := m.now()
	for _, request := range group.requests {
		record := m.records[request.Key]
		if record.Kind == workflow.SleepKind && record.Status == workflow.Suspended && !record.Until.After(now) {
			if earliest == nil || record.Until.Before(earliest.Until) {
				earliest = record
			}
		}
	}
	if earliest != nil {
		m.completeWaitRecord(earliest, nil)
	}
}
