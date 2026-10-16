package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryInfrastructure is a concurrency-safe reference implementation.
// Use one instance per run. It is not a production durable database.
// MemoryInfrastructure 是并发安全的参考实现。每次运行使用一个实例。
// 它不是生产级持久化数据库。
type MemoryInfrastructure struct {
	mu          sync.Mutex
	records     map[string]*Record
	leases      map[string]Lease
	generations map[string]int64
	messages    map[string]Notification
	waitGroups  map[string]*memoryWaitGroup
	waitOwners  map[string]string
	now         func() time.Time
}

func NewMemoryInfrastructure() *MemoryInfrastructure {
	return &MemoryInfrastructure{
		records: make(map[string]*Record), leases: make(map[string]Lease),
		generations: make(map[string]int64), messages: make(map[string]Notification),
		now:        time.Now,
		waitGroups: make(map[string]*memoryWaitGroup), waitOwners: make(map[string]string),
	}
}

func copyRecord(r *Record) *Record {
	if r == nil {
		return nil
	}
	c := *r
	c.Input = append(json.RawMessage(nil), r.Input...)
	c.Result = append(json.RawMessage(nil), r.Result...)
	return &c
}

func (m *MemoryInfrastructure) Get(ctx context.Context, key string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return copyRecord(m.records[key]), nil
}

func (m *MemoryInfrastructure) Claim(ctx context.Context, key string, ttl time.Duration) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, ErrInvalid
	}
	now := m.now()
	if l, ok := m.leases[key]; ok && l.ExpiresAt.After(now) {
		return nil, ErrConflict
	}
	m.generations[key]++
	l := Lease{Key: key, Generation: m.generations[key], ExpiresAt: now.Add(ttl)}
	m.leases[key] = l
	return &l, nil
}

func (m *MemoryInfrastructure) valid(l Lease) bool {
	current, ok := m.leases[l.Key]
	return ok && current.Generation == l.Generation && current.ExpiresAt.After(m.now())
}

func (m *MemoryInfrastructure) Renew(ctx context.Context, l Lease, ttl time.Duration) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, ErrInvalid
	}
	if !m.valid(l) {
		return nil, ErrConflict
	}
	l.ExpiresAt = m.now().Add(ttl)
	m.leases[l.Key] = l
	return &l, nil
}

func (m *MemoryInfrastructure) Release(ctx context.Context, l Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, ok := m.leases[l.Key]
	if !ok || current.Generation != l.Generation {
		return ErrConflict
	}
	delete(m.leases, l.Key)
	return nil
}

func (m *MemoryInfrastructure) Commit(ctx context.Context, l Lease, r *Record, expected int64) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || l.Key != r.Key || r.Kind != ActionKind {
		return nil, ErrInvalid
	}
	if m.waitGroups[r.Key] != nil {
		return nil, ErrChanged
	}
	if !m.valid(l) {
		return nil, ErrConflict
	}
	current := m.records[r.Key]
	if (current == nil && expected != 0) || (current != nil && current.Version != expected) {
		return nil, ErrConflict
	}
	if current != nil && (current.Kind != ActionKind || current.Status == Completed || current.Status == Failed) {
		return nil, ErrConflict
	}
	if r.Status != Running && r.Status != Completed && r.Status != Failed {
		return nil, ErrInvalid
	}
	c := copyRecord(r)
	c.Version = expected + 1
	m.records[r.Key] = c
	return copyRecord(c), nil
}

func (m *MemoryInfrastructure) Put(ctx context.Context, r *Record) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || (r.Kind != CheckpointKind && r.Kind != DefinitionKind && r.Kind != SignalKind && r.Kind != SleepKind) {
		return nil, ErrInvalid
	}
	if m.waitGroups[r.Key] != nil {
		return nil, ErrChanged
	}
	if existing := m.records[r.Key]; existing != nil {
		if existing.Kind != r.Kind {
			return nil, ErrChanged
		}
		return copyRecord(existing), nil
	}
	if ((r.Kind == SignalKind || r.Kind == SleepKind) && r.Status != Suspended) ||
		((r.Kind == CheckpointKind || r.Kind == DefinitionKind) && r.Status != Completed) {
		return nil, ErrInvalid
	}
	c := copyRecord(r)
	c.Version = 1
	m.records[r.Key] = c
	if c.Kind == SignalKind {
		m.enqueue(c, RequestNotification, m.now())
	}
	if c.Kind == SleepKind {
		m.enqueue(c, WakeNotification, c.Until)
	}
	return copyRecord(c), nil
}

func (m *MemoryInfrastructure) enqueue(r *Record, purpose string, at time.Time) {
	id := fmt.Sprintf("%s|%s|%d", r.Key, purpose, r.Version)
	m.messages[id] = Notification{ID: id, Key: r.Key, Purpose: purpose, AvailableAt: at}
}

func (m *MemoryInfrastructure) Resolve(ctx context.Context, key string, version int64, response []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r := m.records[key]
	if r == nil {
		return false, ErrNotFound
	}
	if r.Kind != SignalKind && r.Kind != SleepKind {
		return false, ErrInvalid
	}
	if groupKey := m.waitOwners[key]; groupKey != "" {
		m.advanceWaitGroup(groupKey)
		r = m.records[key]
	}
	if r.Status == Completed || (m.waitOwners[key] != "" && (r.Status == Cancelled || r.Status == Failed)) {
		return false, nil
	}
	if r.Status != Suspended {
		return false, ErrInvalid
	}
	if r.Version != version {
		return false, ErrConflict
	}
	if r.Kind == SleepKind && m.now().Before(r.Until) {
		return false, ErrConflict
	}
	if r.Kind == SignalKind && !json.Valid(response) {
		return false, ErrInvalid
	}
	m.completeWaitRecord(r, response)
	return true, nil
}

// Caller holds m.mu; shared by external callbacks and due Sleep signals.
// 调用方持有 m.mu；外部回调和到期的 Sleep 信号共享此逻辑。
func (m *MemoryInfrastructure) completeWaitRecord(r *Record, response []byte) {
	key := r.Key
	c := copyRecord(r)
	c.Status = Completed
	c.Version++
	c.EndedAt = m.now()
	c.Result = append(json.RawMessage(nil), response...)
	m.records[key] = c
	if groupKey := m.waitOwners[key]; groupKey != "" {
		m.settleWaitGroup(groupKey, c)
	}
	m.enqueue(c, WakeNotification, m.now())
}

func (m *MemoryInfrastructure) Pending(ctx context.Context, now time.Time) ([]Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []Notification
	for _, message := range m.messages {
		if !message.AvailableAt.After(now) {
			result = append(result, message)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AvailableAt.Equal(result[j].AvailableAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].AvailableAt.Before(result[j].AvailableAt)
	})
	return result, nil
}

func (m *MemoryInfrastructure) Ack(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(m.messages, id)
	return nil
}
