// Package memory is an in-process reference Infrastructure for tests, examples
// and local debugging. It is not a durable production store.
// Package memory 是进程内参考 Infrastructure，用于测试、示例和本地调试，
// 不是生产级持久化存储。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/k0923/go/workflow"
)

// Infrastructure is a concurrency-safe reference implementation.
// Use one instance per run. It is not a production durable database.
// Infrastructure 是并发安全的参考实现。每次运行使用一个实例。
// 它不是生产级持久化数据库。
type Infrastructure struct {
	mu          sync.Mutex
	records     map[string]*cflow.Record
	leases      map[string]cflow.Lease
	generations map[string]int64
	messages    map[string]Notification
	waitGroups  map[string]*memoryWaitGroup
	waitOwners  map[string]string
	now         func() time.Time
}

var (
	_ cflow.Infrastructure        = (*Infrastructure)(nil)
	_ Outbox                         = (*Infrastructure)(nil)
	_ cflow.WaitAnyInfrastructure = (*Infrastructure)(nil)
	_ cflow.RecordLister          = (*Infrastructure)(nil)
	_ cflow.Clock                 = (*Infrastructure)(nil)
)

// New returns an empty store using time.Now as its clock.
// New 返回一个以 time.Now 为时钟的空存储。
func New() *Infrastructure {
	return &Infrastructure{
		records: make(map[string]*cflow.Record), leases: make(map[string]cflow.Lease),
		generations: make(map[string]int64), messages: make(map[string]Notification),
		now:        time.Now,
		waitGroups: make(map[string]*memoryWaitGroup), waitOwners: make(map[string]string),
	}
}

// SetClock replaces the store clock (lease expiry, Sleep deadlines). Pass the
// same function as Options.Now, or leave Options.Now nil to inherit it.
// SetClock 替换存储时钟（租约过期、Sleep 截止时间）。与 Options.Now 传同一函数，
// 或让 Options.Now 为空以继承它。
func (m *Infrastructure) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Seed stores a deep copy of r as-is, bypassing every check. It is meant for
// tests that simulate records written by an older version or another worker.
// Seed 原样存入 r 的深拷贝并跳过所有校验，用于模拟旧版本或其他 worker 写入的记录的测试。
func (m *Infrastructure) Seed(r *cflow.Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[r.Key] = copyRecord(r)
}

// Stats reports live leases, records, WaitAny groups and grouped members, for
// leak assertions in tests.
// Stats 报告存活租约、记录、WaitAny 组及组成员数量，用于测试中的泄漏断言。
func (m *Infrastructure) Stats() (leases, records, groups, members int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.leases), len(m.records), len(m.waitGroups), len(m.waitOwners)
}

func copyRecord(r *cflow.Record) *cflow.Record {
	if r == nil {
		return nil
	}
	c := *r
	c.Path = append(cflow.Path(nil), r.Path...)
	c.Input = append(json.RawMessage(nil), r.Input...)
	c.Result = append(json.RawMessage(nil), r.Result...)
	return &c
}

func (m *Infrastructure) Get(ctx context.Context, key string) (*cflow.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return copyRecord(m.records[key]), nil
}

// Records lists every record of the run as deep copies, ordered by key.
// Records 以深拷贝形式按 key 顺序列出 run 的全部记录。
func (m *Infrastructure) Records(ctx context.Context) ([]*cflow.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]*cflow.Record, 0, len(m.records))
	for _, r := range m.records {
		result = append(result, copyRecord(r))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

// Now is the store clock that also decides lease expiry.
// Now 是存储时钟，同时决定租约过期。
func (m *Infrastructure) Now() time.Time { return m.now() }

func (m *Infrastructure) Claim(ctx context.Context, key string, ttl time.Duration) (*cflow.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, cflow.ErrInvalid
	}
	now := m.now()
	if l, ok := m.leases[key]; ok && l.ExpiresAt.After(now) {
		return nil, cflow.ErrConflict
	}
	m.generations[key]++
	l := cflow.Lease{Key: key, Generation: m.generations[key], ExpiresAt: now.Add(ttl)}
	m.leases[key] = l
	return &l, nil
}

func (m *Infrastructure) valid(l cflow.Lease) bool {
	current, ok := m.leases[l.Key]
	return ok && current.Generation == l.Generation && current.ExpiresAt.After(m.now())
}

func (m *Infrastructure) Renew(ctx context.Context, l cflow.Lease, ttl time.Duration) (*cflow.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, cflow.ErrInvalid
	}
	if !m.valid(l) {
		return nil, cflow.ErrConflict
	}
	l.ExpiresAt = m.now().Add(ttl)
	m.leases[l.Key] = l
	return &l, nil
}

func (m *Infrastructure) Release(ctx context.Context, l cflow.Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, ok := m.leases[l.Key]
	if !ok || current.Generation != l.Generation {
		return cflow.ErrConflict
	}
	delete(m.leases, l.Key)
	return nil
}

func (m *Infrastructure) Commit(ctx context.Context, l cflow.Lease, r *cflow.Record, expected int64) (*cflow.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || l.Key != r.Key || r.Kind != cflow.ActionKind {
		return nil, cflow.ErrInvalid
	}
	if m.waitGroups[r.Key] != nil {
		return nil, cflow.ErrChanged
	}
	if !m.valid(l) {
		return nil, cflow.ErrConflict
	}
	current := m.records[r.Key]
	if (current == nil && expected != 0) || (current != nil && current.Version != expected) {
		return nil, cflow.ErrConflict
	}
	if current != nil && (current.Kind != cflow.ActionKind || current.Status == cflow.Completed || current.Status == cflow.Failed) {
		return nil, cflow.ErrConflict
	}
	if r.Status != cflow.Running && r.Status != cflow.Completed && r.Status != cflow.Failed {
		return nil, cflow.ErrInvalid
	}
	c := copyRecord(r)
	c.Version = expected + 1
	m.records[r.Key] = c
	return copyRecord(c), nil
}

func (m *Infrastructure) Put(ctx context.Context, r *cflow.Record) (*cflow.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || (r.Kind != cflow.CheckpointKind && r.Kind != cflow.DefinitionKind && r.Kind != cflow.SignalKind && r.Kind != cflow.SleepKind) {
		return nil, cflow.ErrInvalid
	}
	if m.waitGroups[r.Key] != nil {
		return nil, cflow.ErrChanged
	}
	if existing := m.records[r.Key]; existing != nil {
		if existing.Kind != r.Kind {
			return nil, cflow.ErrChanged
		}
		return copyRecord(existing), nil
	}
	if ((r.Kind == cflow.SignalKind || r.Kind == cflow.SleepKind) && r.Status != cflow.Suspended) ||
		((r.Kind == cflow.CheckpointKind || r.Kind == cflow.DefinitionKind) && r.Status != cflow.Completed) {
		return nil, cflow.ErrInvalid
	}
	c := copyRecord(r)
	c.Version = 1
	m.records[r.Key] = c
	if c.Kind == cflow.SignalKind {
		m.enqueue(c, RequestNotification, m.now())
	}
	if c.Kind == cflow.SleepKind {
		m.enqueue(c, WakeNotification, c.Until)
	}
	return copyRecord(c), nil
}

func (m *Infrastructure) enqueue(r *cflow.Record, purpose string, at time.Time) {
	id := fmt.Sprintf("%s|%s|%d", r.Key, purpose, r.Version)
	m.messages[id] = Notification{ID: id, Key: r.Key, Purpose: purpose, AvailableAt: at}
}

func (m *Infrastructure) Resolve(ctx context.Context, key string, version int64, response []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r := m.records[key]
	if r == nil {
		return false, cflow.ErrNotFound
	}
	if r.Kind != cflow.SignalKind && r.Kind != cflow.SleepKind {
		return false, cflow.ErrInvalid
	}
	if groupKey := m.waitOwners[key]; groupKey != "" {
		m.advanceWaitGroup(groupKey)
		r = m.records[key]
	}
	if r.Status == cflow.Completed || (m.waitOwners[key] != "" && (r.Status == cflow.Cancelled || r.Status == cflow.Failed)) {
		return false, nil
	}
	if r.Status != cflow.Suspended {
		return false, cflow.ErrInvalid
	}
	if r.Version != version {
		return false, cflow.ErrConflict
	}
	if r.Kind == cflow.SleepKind && m.now().Before(r.Until) {
		return false, cflow.ErrConflict
	}
	if r.Kind == cflow.SignalKind && !json.Valid(response) {
		return false, cflow.ErrInvalid
	}
	m.completeWaitRecord(r, response)
	return true, nil
}

// Caller holds m.mu; shared by external callbacks and due Sleep signals.
// 调用方持有 m.mu；外部回调和到期的 Sleep 信号共享此逻辑。
func (m *Infrastructure) completeWaitRecord(r *cflow.Record, response []byte) {
	key := r.Key
	c := copyRecord(r)
	c.Status = cflow.Completed
	c.Version++
	c.EndedAt = m.now()
	c.Result = append(json.RawMessage(nil), response...)
	m.records[key] = c
	if groupKey := m.waitOwners[key]; groupKey != "" {
		m.settleWaitGroup(groupKey, c)
	}
	m.enqueue(c, WakeNotification, m.now())
}

func (m *Infrastructure) Pending(ctx context.Context, now time.Time) ([]Notification, error) {
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

func (m *Infrastructure) Ack(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(m.messages, id)
	return nil
}
