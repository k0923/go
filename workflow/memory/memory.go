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
	records     map[string]*workflow.Record
	leases      map[string]workflow.Lease
	generations map[string]int64
	messages    map[string]Notification
	waitGroups  map[string]*memoryWaitGroup
	waitOwners  map[string]string
	now         func() time.Time
}

var (
	_ workflow.Infrastructure        = (*Infrastructure)(nil)
	_ Outbox                         = (*Infrastructure)(nil)
	_ workflow.WaitAnyInfrastructure = (*Infrastructure)(nil)
	_ workflow.RecordLister          = (*Infrastructure)(nil)
	_ workflow.Clock                 = (*Infrastructure)(nil)
)

// New returns an empty store using time.Now as its clock.
// New 返回一个以 time.Now 为时钟的空存储。
func New() *Infrastructure {
	return &Infrastructure{
		records: make(map[string]*workflow.Record), leases: make(map[string]workflow.Lease),
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
func (m *Infrastructure) Seed(r *workflow.Record) {
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

func copyRecord(r *workflow.Record) *workflow.Record {
	if r == nil {
		return nil
	}
	c := *r
	c.Path = append(workflow.Path(nil), r.Path...)
	c.Input = append(json.RawMessage(nil), r.Input...)
	c.Result = append(json.RawMessage(nil), r.Result...)
	return &c
}

func (m *Infrastructure) Get(ctx context.Context, key string) (*workflow.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return copyRecord(m.records[key]), nil
}

// Records lists every record of the run as deep copies, ordered by key.
// Records 以深拷贝形式按 key 顺序列出 run 的全部记录。
func (m *Infrastructure) Records(ctx context.Context) ([]*workflow.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]*workflow.Record, 0, len(m.records))
	for _, r := range m.records {
		result = append(result, copyRecord(r))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

// Now is the store clock that also decides lease expiry.
// Now 是存储时钟，同时决定租约过期。
func (m *Infrastructure) Now() time.Time { return m.now() }

func (m *Infrastructure) Claim(ctx context.Context, key string, ttl time.Duration) (*workflow.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, workflow.ErrInvalid
	}
	now := m.now()
	if l, ok := m.leases[key]; ok && l.ExpiresAt.After(now) {
		return nil, workflow.ErrConflict
	}
	m.generations[key]++
	l := workflow.Lease{Key: key, Generation: m.generations[key], ExpiresAt: now.Add(ttl)}
	m.leases[key] = l
	return &l, nil
}

func (m *Infrastructure) valid(l workflow.Lease) bool {
	current, ok := m.leases[l.Key]
	return ok && current.Generation == l.Generation && current.ExpiresAt.After(m.now())
}

func (m *Infrastructure) Renew(ctx context.Context, l workflow.Lease, ttl time.Duration) (*workflow.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, workflow.ErrInvalid
	}
	if !m.valid(l) {
		return nil, workflow.ErrConflict
	}
	l.ExpiresAt = m.now().Add(ttl)
	m.leases[l.Key] = l
	return &l, nil
}

func (m *Infrastructure) Release(ctx context.Context, l workflow.Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, ok := m.leases[l.Key]
	if !ok || current.Generation != l.Generation {
		return workflow.ErrConflict
	}
	delete(m.leases, l.Key)
	return nil
}

func (m *Infrastructure) Commit(ctx context.Context, l workflow.Lease, r *workflow.Record, expected int64) (*workflow.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || l.Key != r.Key || r.Kind != workflow.ActionKind {
		return nil, workflow.ErrInvalid
	}
	if m.waitGroups[r.Key] != nil {
		return nil, workflow.ErrChanged
	}
	if !m.valid(l) {
		return nil, workflow.ErrConflict
	}
	current := m.records[r.Key]
	if (current == nil && expected != 0) || (current != nil && current.Version != expected) {
		return nil, workflow.ErrConflict
	}
	if current != nil && (current.Kind != workflow.ActionKind || current.Status == workflow.Completed || current.Status == workflow.Failed) {
		return nil, workflow.ErrConflict
	}
	if r.Status != workflow.Running && r.Status != workflow.Completed && r.Status != workflow.Failed {
		return nil, workflow.ErrInvalid
	}
	c := copyRecord(r)
	c.Version = expected + 1
	m.records[r.Key] = c
	return copyRecord(c), nil
}

func (m *Infrastructure) Put(ctx context.Context, r *workflow.Record) (*workflow.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || (r.Kind != workflow.CheckpointKind && r.Kind != workflow.DefinitionKind && r.Kind != workflow.SignalKind && r.Kind != workflow.SleepKind) {
		return nil, workflow.ErrInvalid
	}
	if m.waitGroups[r.Key] != nil {
		return nil, workflow.ErrChanged
	}
	if existing := m.records[r.Key]; existing != nil {
		if existing.Kind != r.Kind {
			return nil, workflow.ErrChanged
		}
		return copyRecord(existing), nil
	}
	if ((r.Kind == workflow.SignalKind || r.Kind == workflow.SleepKind) && r.Status != workflow.Suspended) ||
		((r.Kind == workflow.CheckpointKind || r.Kind == workflow.DefinitionKind) && r.Status != workflow.Completed) {
		return nil, workflow.ErrInvalid
	}
	c := copyRecord(r)
	c.Version = 1
	m.records[r.Key] = c
	if c.Kind == workflow.SignalKind {
		m.enqueue(c, RequestNotification, m.now())
	}
	if c.Kind == workflow.SleepKind {
		m.enqueue(c, WakeNotification, c.Until)
	}
	return copyRecord(c), nil
}

func (m *Infrastructure) enqueue(r *workflow.Record, purpose string, at time.Time) {
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
		return false, workflow.ErrNotFound
	}
	if r.Kind != workflow.SignalKind && r.Kind != workflow.SleepKind {
		return false, workflow.ErrInvalid
	}
	if groupKey := m.waitOwners[key]; groupKey != "" {
		m.advanceWaitGroup(groupKey)
		r = m.records[key]
	}
	if r.Status == workflow.Completed || (m.waitOwners[key] != "" && (r.Status == workflow.Cancelled || r.Status == workflow.Failed)) {
		return false, nil
	}
	if r.Status != workflow.Suspended {
		return false, workflow.ErrInvalid
	}
	if r.Version != version {
		return false, workflow.ErrConflict
	}
	if r.Kind == workflow.SleepKind && m.now().Before(r.Until) {
		return false, workflow.ErrConflict
	}
	if r.Kind == workflow.SignalKind && !json.Valid(response) {
		return false, workflow.ErrInvalid
	}
	m.completeWaitRecord(r, response)
	return true, nil
}

// Caller holds m.mu; shared by external callbacks and due Sleep signals.
// 调用方持有 m.mu；外部回调和到期的 Sleep 信号共享此逻辑。
func (m *Infrastructure) completeWaitRecord(r *workflow.Record, response []byte) {
	key := r.Key
	c := copyRecord(r)
	c.Status = workflow.Completed
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
