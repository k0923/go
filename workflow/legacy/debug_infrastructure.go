package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

var _ Infrastructure = (*DebugInfrastructure)(nil)
var _ OutboxInfrastructure = (*DebugInfrastructure)(nil)
var _ Infrastructure = (*debugInfrastructureTx)(nil)

// OutboxMessage 描述一条待投递的信号，是 DebugInfrastructure 的投递契约。
// 消息不冗余保存负载，投递方按 SignalKey 调用 GetEvent 读取信号事件本身
// （Request / Until）。生产实现可替换为自己的投递载体（例如 MQ + 存储）。
type OutboxMessage struct {
	// Key 是幂等键，等于信号事件的 key。
	Key string
	// SignalKey 是待投递信号事件的 key。
	SignalKey string
	// AvailableAt 是消息可见时间：WaitFor 为提交时刻，Sleep 为 Until。
	AvailableAt time.Time
}

// OutboxInfrastructure 是 DebugInfrastructure 额外提供的投递侧契约，
// 不属于框架核心 Infrastructure。生产实现如需投递能力，可自定义等价接口。
type OutboxInfrastructure interface {
	// PendingOutbox 返回 AvailableAt 不晚于 now 且尚未确认的消息，按可见时间升序。
	PendingOutbox(ctx context.Context, now time.Time) ([]OutboxMessage, error)
	// AckOutbox 以幂等键确认投递；未确认则置为已确认并返回 true，
	// 已确认返回 true，键不存在返回 false。
	AckOutbox(ctx context.Context, key string) (bool, error)
}

type outboxRecord struct {
	message OutboxMessage
	ackedAt time.Time
}

type DebugInfrastructure struct {
	lock             sync.Mutex
	events           map[string]Event
	leases           map[string]ExecutionLease
	leaseGenerations map[string]int64
	outbox           map[string]outboxRecord
	errors           []error
}

func NewDebugInfrastructure() *DebugInfrastructure {
	return &DebugInfrastructure{
		events:           make(map[string]Event),
		leases:           make(map[string]ExecutionLease),
		leaseGenerations: make(map[string]int64),
		outbox:           make(map[string]outboxRecord),
	}
}

func (infrastructure *DebugInfrastructure) ClaimLease(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (*ExecutionLease, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.claimLease(ctx, key, ttl)
}

func (infrastructure *DebugInfrastructure) RenewLease(
	ctx context.Context,
	lease ExecutionLease,
	ttl time.Duration,
) (*ExecutionLease, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.renewLease(ctx, lease, ttl)
}

func (infrastructure *DebugInfrastructure) ReleaseLease(
	ctx context.Context,
	lease ExecutionLease,
) (bool, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.releaseLease(ctx, lease)
}

func (infrastructure *DebugInfrastructure) AddEvent(ctx context.Context, event Event) (Event, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.addEvent(ctx, event)
}

func (infrastructure *DebugInfrastructure) GetEvent(ctx context.Context, key string) (Event, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.getEvent(ctx, key)
}

func (infrastructure *DebugInfrastructure) UpdateEvent(ctx context.Context, event Event) (Event, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.updateEvent(ctx, event)
}

func (infrastructure *DebugInfrastructure) Suspend(ctx context.Context, signal Signal) error {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.suspend(ctx, signal)
}

func (infrastructure *DebugInfrastructure) OnError(err error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.onError(err)
}

func (infrastructure *DebugInfrastructure) PendingOutbox(
	ctx context.Context,
	now time.Time,
) ([]OutboxMessage, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.pendingOutbox(ctx, now)
}

func (infrastructure *DebugInfrastructure) AckOutbox(ctx context.Context, key string) (bool, error) {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()
	return infrastructure.ackOutbox(ctx, key)
}

func (infrastructure *DebugInfrastructure) Errors() []error {
	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	return append([]error(nil), infrastructure.errors...)
}

// Transaction 在持锁状态下执行 handler：handler 通过传入的 api 读到自己的写入，
// 返回非 nil error 或 panic 时回滚到事务开始前的快照，否则提交。
func (infrastructure *DebugInfrastructure) Transaction(
	ctx context.Context,
	handler func(api Infrastructure) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	infrastructure.lock.Lock()
	defer infrastructure.lock.Unlock()
	infrastructure.initialize()

	snapshot, err := infrastructure.snapshot()
	if err != nil {
		return err
	}

	api := &debugInfrastructureTx{infrastructure: infrastructure}
	var handlerErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				infrastructure.restore(snapshot)
				panic(recovered)
			}
		}()
		handlerErr = handler(api)
	}()

	if handlerErr != nil {
		infrastructure.restore(snapshot)
		return handlerErr
	}
	return nil
}

// debugInfrastructureTx 是事务内视图：它与 DebugInfrastructure 共享同一批结构，
// 但不再加锁，因为外层 Transaction 已持有锁。否则会自死锁。
type debugInfrastructureTx struct {
	infrastructure *DebugInfrastructure
}

func (tx *debugInfrastructureTx) ClaimLease(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (*ExecutionLease, error) {
	return tx.infrastructure.claimLease(ctx, key, ttl)
}

func (tx *debugInfrastructureTx) RenewLease(
	ctx context.Context,
	lease ExecutionLease,
	ttl time.Duration,
) (*ExecutionLease, error) {
	return tx.infrastructure.renewLease(ctx, lease, ttl)
}

func (tx *debugInfrastructureTx) ReleaseLease(
	ctx context.Context,
	lease ExecutionLease,
) (bool, error) {
	return tx.infrastructure.releaseLease(ctx, lease)
}

func (tx *debugInfrastructureTx) Suspend(ctx context.Context, signal Signal) error {
	return tx.infrastructure.suspend(ctx, signal)
}

func (tx *debugInfrastructureTx) Transaction(
	ctx context.Context,
	handler func(api Infrastructure) error,
) error {
	return fmt.Errorf("nested transaction is not supported")
}

func (tx *debugInfrastructureTx) AddEvent(ctx context.Context, event Event) (Event, error) {
	return tx.infrastructure.addEvent(ctx, event)
}

func (tx *debugInfrastructureTx) GetEvent(ctx context.Context, key string) (Event, error) {
	return tx.infrastructure.getEvent(ctx, key)
}

func (tx *debugInfrastructureTx) UpdateEvent(ctx context.Context, event Event) (Event, error) {
	return tx.infrastructure.updateEvent(ctx, event)
}

func (tx *debugInfrastructureTx) OnError(err error) {
	tx.infrastructure.onError(err)
}

func (infrastructure *DebugInfrastructure) claimLease(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (*ExecutionLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("lease ttl must be positive")
	}

	now := time.Now()
	if lease, exists := infrastructure.leases[key]; exists && lease.ExpiresAt.After(now) {
		return nil, nil
	}

	generation := infrastructure.leaseGenerations[key] + 1
	lease := ExecutionLease{
		Key:        key,
		Generation: generation,
		ExpiresAt:  now.Add(ttl),
	}
	infrastructure.leases[key] = lease
	infrastructure.leaseGenerations[key] = generation
	return cloneLease(lease), nil
}

func (infrastructure *DebugInfrastructure) renewLease(
	ctx context.Context,
	lease ExecutionLease,
	ttl time.Duration,
) (*ExecutionLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("lease ttl must be positive")
	}

	now := time.Now()
	current, exists := infrastructure.leases[lease.Key]
	if !exists ||
		current.Generation != lease.Generation ||
		!current.ExpiresAt.After(now) {
		return nil, nil
	}
	current.ExpiresAt = now.Add(ttl)
	infrastructure.leases[lease.Key] = current
	return cloneLease(current), nil
}

func (infrastructure *DebugInfrastructure) releaseLease(
	ctx context.Context,
	lease ExecutionLease,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	current, exists := infrastructure.leases[lease.Key]
	if !exists || current.Generation != lease.Generation {
		return false, nil
	}
	delete(infrastructure.leases, lease.Key)
	return true, nil
}

func (infrastructure *DebugInfrastructure) addEvent(ctx context.Context, event Event) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stored, err := cloneEvent(event)
	if err != nil {
		return nil, err
	}
	setEventVersion(stored, 1)
	key := event.GetKey()
	if infrastructure.events[key] != nil {
		return nil, nil
	}
	infrastructure.events[key] = stored
	return cloneEvent(stored)
}

func (infrastructure *DebugInfrastructure) getEvent(ctx context.Context, key string) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if infrastructure.events[key] == nil {
		return nil, nil
	}
	return cloneEvent(infrastructure.events[key])
}

func (infrastructure *DebugInfrastructure) updateEvent(ctx context.Context, event Event) (Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := event.GetKey()
	current := infrastructure.events[key]
	if current == nil || current.GetVersion() != event.GetVersion() {
		return nil, nil
	}
	stored, err := cloneEvent(event)
	if err != nil {
		return nil, err
	}
	setEventVersion(stored, event.GetVersion()+1)
	infrastructure.events[key] = stored
	return cloneEvent(stored)
}

// suspend 按 key 幂等持久化信号，并登记其投递（outbox）。
// 同 key 同类型已存在时保留既有事件，不覆盖版本；类型冲突返回 error。
func (infrastructure *DebugInfrastructure) suspend(ctx context.Context, signal Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	event, ok := signal.(Event)
	if !ok {
		return fmt.Errorf("signal %T does not implement Event", signal)
	}
	key := event.GetKey()

	stored, err := cloneEvent(event)
	if err != nil {
		return err
	}
	setEventVersion(stored, 1)

	if existing := infrastructure.events[key]; existing != nil {
		if existing.GetEventType() != stored.GetEventType() {
			return fmt.Errorf(
				"signal %s already exists as %s, not %s",
				key,
				existing.GetEventType(),
				stored.GetEventType(),
			)
		}
	} else {
		infrastructure.events[key] = stored
	}

	infrastructure.enqueueOutbox(stored)
	return nil
}

func (infrastructure *DebugInfrastructure) enqueueOutbox(event Event) {
	message := outboxMessageFor(event)
	if _, exists := infrastructure.outbox[message.Key]; exists {
		return
	}
	infrastructure.outbox[message.Key] = outboxRecord{message: message}
}

func (infrastructure *DebugInfrastructure) pendingOutbox(
	ctx context.Context,
	now time.Time,
) ([]OutboxMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	messages := make([]OutboxMessage, 0, len(infrastructure.outbox))
	for _, record := range infrastructure.outbox {
		if !record.ackedAt.IsZero() || record.message.AvailableAt.After(now) {
			continue
		}
		messages = append(messages, record.message)
	}
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].AvailableAt.Equal(messages[j].AvailableAt) {
			return messages[i].Key < messages[j].Key
		}
		return messages[i].AvailableAt.Before(messages[j].AvailableAt)
	})
	return messages, nil
}

func (infrastructure *DebugInfrastructure) ackOutbox(
	ctx context.Context,
	key string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	record, exists := infrastructure.outbox[key]
	if !exists {
		return false, nil
	}
	if record.ackedAt.IsZero() {
		record.ackedAt = time.Now()
		infrastructure.outbox[key] = record
	}
	return true, nil
}

func (infrastructure *DebugInfrastructure) onError(err error) {
	if err == nil {
		return
	}
	infrastructure.errors = append(infrastructure.errors, err)
}

func (infrastructure *DebugInfrastructure) initialize() {
	if infrastructure.events == nil {
		infrastructure.events = make(map[string]Event)
	}
	if infrastructure.leases == nil {
		infrastructure.leases = make(map[string]ExecutionLease)
	}
	if infrastructure.leaseGenerations == nil {
		infrastructure.leaseGenerations = make(map[string]int64)
	}
	if infrastructure.outbox == nil {
		infrastructure.outbox = make(map[string]outboxRecord)
	}
}

type debugSnapshot struct {
	events           map[string]Event
	leases           map[string]ExecutionLease
	leaseGenerations map[string]int64
	outbox           map[string]outboxRecord
}

func (infrastructure *DebugInfrastructure) snapshot() (*debugSnapshot, error) {
	events := make(map[string]Event, len(infrastructure.events))
	for key, event := range infrastructure.events {
		cloned, err := cloneEvent(event)
		if err != nil {
			return nil, err
		}
		events[key] = cloned
	}

	leases := make(map[string]ExecutionLease, len(infrastructure.leases))
	for key, lease := range infrastructure.leases {
		leases[key] = lease
	}

	generations := make(map[string]int64, len(infrastructure.leaseGenerations))
	for key, generation := range infrastructure.leaseGenerations {
		generations[key] = generation
	}

	outbox := make(map[string]outboxRecord, len(infrastructure.outbox))
	for key, record := range infrastructure.outbox {
		outbox[key] = cloneOutboxRecord(record)
	}

	return &debugSnapshot{
		events:           events,
		leases:           leases,
		leaseGenerations: generations,
		outbox:           outbox,
	}, nil
}

func (infrastructure *DebugInfrastructure) restore(snapshot *debugSnapshot) {
	infrastructure.events = snapshot.events
	infrastructure.leases = snapshot.leases
	infrastructure.leaseGenerations = snapshot.leaseGenerations
	infrastructure.outbox = snapshot.outbox
}

func outboxMessageFor(event Event) OutboxMessage {
	message := OutboxMessage{
		Key:         event.GetKey(),
		SignalKey:   event.GetKey(),
		AvailableAt: time.Now(),
	}
	if sleep, ok := event.(*SleepSignalEvent); ok {
		message.AvailableAt = sleep.Until
	}
	return message
}

func cloneOutboxRecord(record outboxRecord) outboxRecord {
	return outboxRecord{
		message: record.message,
		ackedAt: record.ackedAt,
	}
}

func cloneLease(lease ExecutionLease) *ExecutionLease {
	result := lease
	return &result
}

func cloneEvent(event Event) (Event, error) {
	switch value := event.(type) {
	case *StepResultEvent:
		result := *value
		result.Result = cloneRawMessage(value.Result)
		return &result, nil
	case *WaitForSignalEvent:
		result := *value
		result.Request = cloneRawMessage(value.Request)
		result.Response = cloneRawMessage(value.Response)
		return &result, nil
	case *CheckpointEvent:
		result := *value
		result.Data = cloneRawMessage(value.Data)
		return &result, nil
	case *SleepSignalEvent:
		result := *value
		return &result, nil
	case *LoopEvent:
		result := *value
		result.Result = cloneRawMessage(value.Result)
		return &result, nil
	default:
		return nil, fmt.Errorf("unsupported event type %T", event)
	}
}

func setEventVersion(event Event, version int) {
	switch value := event.(type) {
	case *StepResultEvent:
		value.Version = version
	case *WaitForSignalEvent:
		value.Version = version
	case *CheckpointEvent:
		value.Version = version
	case *SleepSignalEvent:
		value.Version = version
	case *LoopEvent:
		value.Version = version
	}
}

func cloneRawMessage(message json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), message...)
}
