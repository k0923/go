package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

type memoryWaitGroup struct {
	requests []WaitRequest
	state    WaitAnyState
}

func (m *MemoryInfrastructure) WaitAny(ctx context.Context, key string, requests []WaitRequest) (*WaitAnyState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" || len(requests) == 0 {
		return nil, ErrInvalid
	}
	requests = append([]WaitRequest(nil), requests...)
	for i := range requests {
		if requests[i].Kind == "" {
			requests[i].Kind = SignalKind
		}
	}
	if existing := m.waitGroups[key]; existing != nil {
		if len(existing.requests) != len(requests) {
			return nil, ErrChanged
		}
		for i, request := range requests {
			old := existing.requests[i]
			if request.Key != old.Key || request.Kind != old.Kind ||
				request.Duration != old.Duration || !request.Until.Equal(old.Until) ||
				!bytes.Equal(request.Request, old.Request) {
				return nil, ErrChanged
			}
		}
		m.advanceWaitGroup(key)
		return copyWaitAnyState(existing.state), nil
	}
	if m.records[key] != nil {
		return nil, ErrChanged
	}
	seen := map[string]bool{key: true}
	for _, request := range requests {
		if request.Key == "" || seen[request.Key] {
			return nil, ErrInvalid
		}
		switch request.Kind {
		case SignalKind:
			if !json.Valid(request.Request) {
				return nil, ErrInvalid
			}
		case SleepKind:
		default:
			return nil, fmt.Errorf("%w: %s", ErrSignalUnsupported, request.Kind)
		}
		if m.records[request.Key] != nil || m.waitGroups[request.Key] != nil || m.waitOwners[request.Key] != "" {
			return nil, ErrChanged
		}
		seen[request.Key] = true
	}
	group := &memoryWaitGroup{state: WaitAnyState{Status: Suspended}}
	for _, request := range requests {
		raw := append(json.RawMessage(nil), request.Request...)
		storedRequest := request
		storedRequest.Request = raw
		group.requests = append(group.requests, storedRequest)
		record := &Record{Key: request.Key, Kind: request.Kind, Status: Suspended, Version: 1, Input: raw, StartedAt: m.now()}
		if request.Kind == SleepKind {
			record.Until = request.Until
			if record.Until.IsZero() {
				record.Until = m.now().Add(request.Duration)
			}
		}
		m.records[request.Key] = record
		m.waitOwners[request.Key] = key
		group.state.Pending = append(group.state.Pending, pendingRecord(record))
		if record.Kind == SleepKind {
			m.enqueue(record, WakeNotification, record.Until)
		} else {
			m.enqueue(record, RequestNotification, m.now())
		}
	}
	m.waitGroups[key] = group
	m.advanceWaitGroup(key)
	return copyWaitAnyState(group.state), nil
}

func copyWaitAnyState(state WaitAnyState) *WaitAnyState {
	state.Response = append(json.RawMessage(nil), state.Response...)
	state.Pending = append([]Pending(nil), state.Pending...)
	state.Failures = append([]WaitFailure(nil), state.Failures...)
	return &state
}

// settleWaitGroup runs under the same storage lock as Resolve. This makes
// winner selection, loser cancellation, message cleanup and wake atomic.
// settleWaitGroup 在与 Resolve 相同的存储锁下运行。这会使赢家选择、失败方取消、
// 消息清理和 wake 保持原子性。
func (m *MemoryInfrastructure) settleWaitGroup(groupKey string, winner *Record) {
	group := m.waitGroups[groupKey]
	group.state.Status = Completed
	group.state.WinnerKey = winner.Key
	group.state.WinnerKind = winner.Kind
	group.state.Response = append(json.RawMessage(nil), winner.Result...)
	group.state.Pending = nil
	for _, request := range group.requests {
		record := m.records[request.Key]
		m.removeWaitMessages(request.Key)
		if request.Key != winner.Key && record.Status == Suspended {
			cancelled := copyRecord(record)
			cancelled.Status = Cancelled
			cancelled.Version++
			cancelled.EndedAt = m.now()
			m.records[request.Key] = cancelled
			m.enqueue(cancelled, CancelNotification, m.now())
		}
	}
}

func (m *MemoryInfrastructure) removeWaitMessages(key string) {
	for id, message := range m.messages {
		if message.Key == key {
			delete(m.messages, id)
		}
	}
}

func (m *MemoryInfrastructure) FailWait(ctx context.Context, key string, version int64, message string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	groupKey := m.waitOwners[key]
	if groupKey == "" {
		return false, ErrNotFound
	}
	group := m.waitGroups[groupKey]
	m.advanceWaitGroup(groupKey)
	record := m.records[key]
	if group.state.Status != Suspended || record.Status != Suspended {
		return false, nil
	}
	if record.Version != version {
		return false, ErrConflict
	}
	failed := copyRecord(record)
	failed.Status = Failed
	failed.Version++
	failed.Error = message
	failed.EndedAt = m.now()
	m.records[key] = failed
	m.removeWaitMessages(key)
	group.state.Failures = append(group.state.Failures, WaitFailure{Key: key, Message: message})
	group.state.Pending = nil
	for _, request := range group.requests {
		if m.records[request.Key].Status == Suspended {
			group.state.Pending = append(group.state.Pending, pendingRecord(m.records[request.Key]))
		}
	}
	if len(group.state.Pending) == 0 {
		group.state.Status = Failed
		// Preserve a durable wake even when every member failed.
		// 即使每个成员都失败，也保留一个持久化 wake。
		id := fmt.Sprintf("%s|%s|failed", groupKey, WakeNotification)
		m.messages[id] = Notification{ID: id, Key: groupKey, Purpose: WakeNotification, AvailableAt: m.now()}
	}
	return true, nil
}

func pendingRecord(record *Record) Pending {
	return Pending{Key: record.Key, Kind: record.Kind, Until: record.Until}
}

// A due timer is resolved under the same lock used by callbacks. Among due
// timers choose the earliest deadline, breaking ties by declaration order.
// No resident timer/goroutine is required: its durable WAKE schedules a Drive.
// 到期计时器会在回调使用的同一把锁下解析。在已到期的计时器中选择最早截止时间，
// 并按声明顺序打破平局。不需要常驻 timer/goroutine：其持久化 WAKE 会调度一次 Drive。
func (m *MemoryInfrastructure) advanceWaitGroup(key string) {
	group := m.waitGroups[key]
	if group.state.Status != Suspended {
		return
	}
	var earliest *Record
	now := m.now()
	for _, request := range group.requests {
		record := m.records[request.Key]
		if record.Kind == SleepKind && record.Status == Suspended && !record.Until.After(now) {
			if earliest == nil || record.Until.Before(earliest.Until) {
				earliest = record
			}
		}
	}
	if earliest != nil {
		m.completeWaitRecord(earliest, nil)
	}
}
