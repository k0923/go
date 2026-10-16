package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k0923/go/lease"
)

type EventType string

const (
	DriveStarted   EventType = "drive_started"
	DriveFinished  EventType = "drive_finished"
	ActionStarted  EventType = "action_started"
	ActionFinished EventType = "action_finished"
	ActionReplayed EventType = "action_replayed"
	WaitSuspended  EventType = "wait_suspended"
	WaitResolved   EventType = "wait_resolved"
)

// Event is a versioned, JSON-serializable observation, not a durable outbox
// message. It excludes input/output payloads. Error text comes from the caller;
// applications must avoid including secrets in their error messages.
// ExecutionID identifies ONE Drive invocation, including rejected invocations.
// ResolveSignal outside Drive has no ExecutionID; correlate it by scoped key.
// Sequence orders production within one Engine, not concurrent callback delivery.
// Event 是带版本、可 JSON 序列化的观测记录，不是持久化 outbox 消息。它不包含
// 输入/输出负载。错误文本来自调用方；应用必须避免在错误消息中包含密钥。
// ExecutionID 标识一次 Drive 调用，包括被拒绝的调用。Drive 外部的 ResolveSignal
// 没有 ExecutionID；需要通过带作用域的 key 关联。Sequence 仅对一个 Engine 内的
// 事件生产排序，不对并发回调投递排序。
type Event struct {
	SchemaVersion   int           `json:"schema_version"`
	ID              string        `json:"id"`
	EngineID        string        `json:"engine_id"`
	Sequence        uint64        `json:"sequence"`
	ExecutionID     string        `json:"execution_id,omitempty"`
	WorkflowName    string        `json:"workflow_name,omitempty"`
	WorkflowVersion string        `json:"workflow_version,omitempty"`
	Type            EventType     `json:"type"`
	Key             string        `json:"key"`
	Kind            Kind          `json:"kind,omitempty"`
	Status          Status        `json:"status"`
	At              time.Time     `json:"at"`
	StartedAt       time.Time     `json:"started_at"`
	Duration        time.Duration `json:"duration_ns"`
	Replayed        bool          `json:"replayed,omitempty"`
	RecordVersion   int64         `json:"record_version,omitempty"`
	Pending         []Pending     `json:"pending,omitempty"`
	Error           *EventError   `json:"error,omitempty"`
}

// EventError contains transportable diagnostic data. It does not retain the
// original Go error or claim that sentinel identities survive JSON encoding.
// EventError 包含可传输的诊断数据。它不保留原始 Go 错误，也不声明哨兵错误身份
// 能跨 JSON 编码保留。
type EventError struct {
	Category  string `json:"category"`
	Message   string `json:"message"`
	Operation string `json:"operation,omitempty"`
}

// EventListener must be concurrency-safe and return promptly. Delivery is
// synchronous, so events are captured before the emitting API returns.
// Panics are isolated independently for each listener.
// EventListener 必须并发安全并快速返回。投递是同步的，因此事件会在发出事件的
// API 返回前被捕获。每个 listener 的 panic 会被独立隔离。
type EventListener func(context.Context, Event)

type eventSubscription struct {
	id       uint64
	listener EventListener
	types    map[EventType]bool
}

type eventHub struct {
	mu        sync.Mutex
	next      uint64
	sequence  atomic.Uint64
	listeners []eventSubscription
}

// Subscribe registers a listener, optionally filtered to event types. No types
// means all events. The returned unsubscribe function is idempotent and safe
// inside a callback. A delivery already snapshotted may finish after unsubscribe.
// No historical events are replayed. A nil listener is a harmless no-op.
// Separate registrations are separate subscriptions, even for the same function.
// Subscribe 注册一个 listener，可选按事件类型过滤。不传类型表示接收所有事件。
// 返回的取消订阅函数是幂等的，并且可在回调内部安全调用。已经截取快照的投递
// 可能会在取消订阅后完成。历史事件不会回放。nil listener 是无害的空操作。
// 即使是同一个函数，独立注册也会成为独立订阅。
func (e *Engine) Subscribe(listener EventListener, types ...EventType) func() {
	if listener == nil {
		return func() {}
	}
	filter := make(map[EventType]bool, len(types))
	for _, eventType := range types {
		filter[eventType] = true
	}
	hub := &e.events
	hub.mu.Lock()
	hub.next++
	id := hub.next
	hub.listeners = append(hub.listeners, eventSubscription{id: id, listener: listener, types: filter})
	hub.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			hub.mu.Lock()
			defer hub.mu.Unlock()
			for i, subscription := range hub.listeners {
				if subscription.id == id {
					copy(hub.listeners[i:], hub.listeners[i+1:])
					hub.listeners[len(hub.listeners)-1] = eventSubscription{}
					hub.listeners = hub.listeners[:len(hub.listeners)-1]
					break
				}
			}
		})
	}
}

type eventContextKey struct{}
type eventContext struct{ executionID, name, version string }

func (e *Engine) emit(ctx context.Context, event Event, err error) {
	// Even formatting an application-defined error must not break execution.
	// 即使格式化应用自定义错误，也不能破坏执行。
	defer func() { _ = recover() }()
	event.SchemaVersion = 1
	event.EngineID = e.eventID
	event.Sequence = e.events.sequence.Add(1)
	event.ID = fmt.Sprintf("%s:%d", e.eventID, event.Sequence)
	metadata, _ := ctx.Value(eventContextKey{}).(eventContext)
	event.ExecutionID, event.WorkflowName, event.WorkflowVersion = metadata.executionID, metadata.name, metadata.version
	event.At = e.options.Now()
	if event.StartedAt.IsZero() {
		event.StartedAt = event.At
	}
	event.Duration = event.At.Sub(event.StartedAt)
	if event.Duration < 0 {
		event.Duration = 0
	}
	if err != nil {
		event.Error = describeEventError(err)
	}
	e.events.mu.Lock()
	listeners := append([]eventSubscription(nil), e.events.listeners...)
	e.events.mu.Unlock()
	for _, subscription := range listeners {
		if len(subscription.types) > 0 && !subscription.types[event.Type] {
			continue
		}
		// Prevent one listener from corrupting another listener's snapshot.
		// 防止一个 listener 破坏另一个 listener 的快照。
		copyEvent := event
		copyEvent.Pending = append([]Pending(nil), event.Pending...)
		if event.Error != nil {
			copyError := *event.Error
			copyEvent.Error = &copyError
		}
		func() {
			defer func() { _ = recover() }()
			subscription.listener(ctx, copyEvent)
		}()
	}
}

func describeEventError(err error) (result *EventError) {
	result = &EventError{Category: "execution", Message: "error description unavailable"}
	// User-defined Error/Is/As methods may panic. Retain a diagnostic event
	// with fallback text rather than skipping delivery to every subscriber.
	// 用户定义的 Error/Is/As 方法可能 panic。应保留带兜底文本的诊断事件，
	// 而不是跳过对所有订阅者的投递。
	defer func() { _ = recover() }()
	result.Message = err.Error()
	var operation *OperationError
	var leaseErr *lease.Error
	var permanent *PermanentError
	var replay *ReplayError
	var panicErr *PanicError
	hasOperation := errors.As(err, &operation)
	hasLease := errors.As(err, &leaseErr)
	if hasOperation {
		result.Operation = operation.Op
	} else if hasLease {
		result.Operation = leaseErr.Op
	}
	switch {
	case errors.As(err, &panicErr):
		result.Category = "panic"
	case errors.Is(err, ErrConflict):
		result.Category = "conflict"
	case errors.Is(err, context.Canceled):
		result.Category = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		result.Category = "timeout"
	case hasOperation:
		result.Category = "infrastructure"
	case hasLease:
		result.Category = "lease"
	case errors.As(err, &replay):
		result.Category = "replay"
	case errors.As(err, &permanent):
		result.Category = "permanent"
	case errors.Is(err, ErrInvalid):
		result.Category = "invalid"
	case errors.Is(err, ErrChanged):
		result.Category = "changed"
	}
	return result
}
