package debugger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id TEXT PRIMARY KEY,
	workflow TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	input BLOB,
	output BLOB,
	status TEXT NOT NULL DEFAULT 'CREATED',
	error TEXT NOT NULL DEFAULT '',
	pending TEXT NOT NULL DEFAULT '[]',
	drives INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	last_seen INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS deleted_runs (
	id TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS engine_event_ids (
	run_id TEXT NOT NULL,
	event_id TEXT NOT NULL,
	PRIMARY KEY (run_id, event_id)
);
CREATE TABLE IF NOT EXISTS records (
	run_id TEXT NOT NULL,
	key TEXT NOT NULL,
	kind TEXT NOT NULL,
	status TEXT NOT NULL,
	version INTEGER NOT NULL,
	input BLOB,
	result BLOB,
	error TEXT NOT NULL DEFAULT '',
	until INTEGER NOT NULL DEFAULT 0,
	started_at INTEGER NOT NULL DEFAULT 0,
	ended_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0,
	group_key TEXT NOT NULL DEFAULT '',
	type_path TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (run_id, key)
);
CREATE TABLE IF NOT EXISTS leases (
	run_id TEXT NOT NULL,
	key TEXT NOT NULL,
	generation INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	held INTEGER NOT NULL,
	PRIMARY KEY (run_id, key)
);
CREATE TABLE IF NOT EXISTS messages (
	run_id TEXT NOT NULL,
	id TEXT NOT NULL,
	key TEXT NOT NULL,
	purpose TEXT NOT NULL,
	state TEXT NOT NULL,
	available_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	done_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, id)
);
CREATE TABLE IF NOT EXISTS wait_groups (
	run_id TEXT NOT NULL,
	key TEXT NOT NULL,
	data TEXT NOT NULL,
	PRIMARY KEY (run_id, key)
);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	source TEXT NOT NULL,
	at INTEGER NOT NULL,
	payload TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_run ON events (run_id, id);
CREATE INDEX IF NOT EXISTS messages_due ON messages (run_id, state, available_at);
`

// Message delivery states. REMOVED means storage withdrew it (a resolved or
// cancelled wait); DONE means a request was answered through the debugger.
// 消息投递状态。REMOVED 表示存储已撤回消息（等待已解析或已取消）；
// DONE 表示请求已通过 debugger 被回答。
const (
	msgPending = "PENDING"
	msgAcked   = "ACKED"
	msgRemoved = "REMOVED"
	msgDone    = "DONE"
)

// Store is the SQLite state behind the server. All mutations run in one
// serialized transaction, matching the atomicity memory.Infrastructure gets
// from its single mutex.
// Store 是服务端背后的 SQLite 状态。所有变更都在一个串行事务中执行，
// 与 memory.Infrastructure 通过单个互斥锁获得的原子性保持一致。
type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	now func() time.Time
	hub *hub
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has one writer anyway, and in-memory databases
	// would otherwise be distinct per connection.
	// 单连接：SQLite 本来也只有一个 writer；否则内存数据库会按连接彼此隔离。
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA synchronous=NORMAL"} {
		if _, err = db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// The schema is pre-release: an older database without the path column is
	// not migrated. Delete it (or pass a new -db) to start fresh.
	// schema 尚未发布：缺少 path 列的旧数据库不做迁移，删除它（或换一个 -db）重新开始。
	var hasPath int
	if err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('records') WHERE name = 'path'`).Scan(&hasPath); err == nil && hasPath == 0 {
		err = fmt.Errorf("debugger database %s predates Record.Path; delete it to start fresh", path)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now, hub: newHub()}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func tm(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// blob scans a nullable BLOB into a JSON payload. json.RawMessage is not a
// []byte for database/sql in this toolchain, so it cannot be scanned directly.
// blob 将可空 BLOB 扫描为 JSON 负载。在此工具链中，json.RawMessage 对
// database/sql 而言不是 []byte，因此不能直接扫描。
type blob struct{ dst *json.RawMessage }

func (b blob) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*b.dst = nil
	case []byte:
		*b.dst = clone(v)
	case string:
		*b.dst = clone([]byte(v))
	default:
		return fmt.Errorf("unsupported blob %T", value)
	}
	return nil
}

type txn struct {
	tx     *sql.Tx
	s      *Store
	run    string
	now    time.Time
	events []map[string]any
	dirty  bool
}

// exec runs a write and marks the transaction dirty when it changed rows, so
// read-only transactions (e.g. the wake long poll) never notify subscribers.
// exec 执行一次写入，并在改动行时将事务标记为 dirty，因此只读事务
// （例如 wake 长轮询）不会通知订阅者。
func (t *txn) exec(query string, args ...any) (sql.Result, error) {
	res, err := t.tx.Exec(query, args...)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			t.dirty = true
		}
	}
	return res, err
}

// tx runs fn atomically for one run. The run row is created lazily so a
// client may use the Infrastructure without registering first.
// tx 为单个运行原子执行 fn。run 行会被惰性创建，因此客户端可以在注册前
// 使用 Infrastructure。
func (s *Store) tx(ctx context.Context, run string, fn func(*txn) error) error {
	if run == "" {
		return cflow.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Acquire the SQLite writer lock before sampling the fencing clock.
	// 在读取 fencing 时钟之前获取 SQLite 写锁。
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET id = id WHERE id = ?`, run); err != nil {
		tx.Rollback()
		return err
	}
	var deleted string
	err = tx.QueryRowContext(ctx, `SELECT id FROM deleted_runs WHERE id = ?`, run).Scan(&deleted)
	if err == nil {
		tx.Rollback()
		return cflow.ErrNotFound
	}
	if !errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return err
	}
	t := &txn{tx: tx, s: s, run: run, now: s.now()}
	if _, err = t.exec(`INSERT OR IGNORE INTO runs (id, created_at, updated_at) VALUES (?, ?, ?)`, run, ts(t.now), ts(t.now)); err != nil {
		tx.Rollback()
		return err
	}
	if err = fn(t); err != nil {
		tx.Rollback()
		return err
	}
	for _, event := range t.events {
		event["at"] = t.now
		payload, _ := json.Marshal(event)
		if _, err = tx.Exec(`INSERT INTO events (run_id, source, at, payload) VALUES (?, 'store', ?, ?)`, run, ts(t.now), string(payload)); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if t.dirty || len(t.events) > 0 {
		s.hub.publish(run)
	}
	return nil
}

func (t *txn) log(eventType, key string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["type"], fields["key"] = eventType, key
	t.events = append(t.events, fields)
}

func (t *txn) record(key string) (*cflow.Record, string, error) {
	var r cflow.Record
	var group string
	var until, started, ended int64
	err := t.tx.QueryRow(`SELECT key, path, kind, status, version, type_path, input, result, error, until, started_at, ended_at, group_key
		FROM records WHERE run_id = ? AND key = ?`, t.run, key).
		Scan(&r.Key, pathColumn{&r.Path}, &r.Kind, &r.Status, &r.Version, &r.Type, blob{&r.Input}, blob{&r.Result}, &r.Error, &until, &started, &ended, &group)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	r.Input, r.Result = clone(r.Input), clone(r.Result)
	r.Until, r.StartedAt, r.EndedAt = tm(until), tm(started), tm(ended)
	return &r, group, nil
}

func (t *txn) saveRecord(r *cflow.Record, group string) error {
	_, err := t.exec(`INSERT OR REPLACE INTO records
		(run_id, key, path, kind, status, version, type_path, input, result, error, until, started_at, ended_at, updated_at, group_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.run, r.Key, encodePath(r.Path), r.Kind, r.Status, r.Version, r.Type, []byte(r.Input), []byte(r.Result), r.Error,
		ts(r.Until), ts(r.StartedAt), ts(r.EndedAt), ts(t.now), group)
	return err
}

func (t *txn) enqueueID(id, key, purpose string, at time.Time) error {
	_, err := t.exec(`INSERT OR REPLACE INTO messages (run_id, id, key, purpose, state, available_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, t.run, id, key, purpose, msgPending, ts(at), ts(t.now))
	return err
}

func (t *txn) enqueue(r *cflow.Record, purpose string, at time.Time) error {
	return t.enqueueID(fmt.Sprintf("%s|%s|%d", r.Key, purpose, r.Version), r.Key, purpose, at)
}

func (t *txn) closeMessages(key, purpose, state string) error {
	query := `UPDATE messages SET state = ?, done_at = ? WHERE run_id = ? AND key = ? AND state = ?`
	args := []any{state, ts(t.now), t.run, key, msgPending}
	if purpose != "" {
		query += ` AND purpose = ?`
		args = append(args, purpose)
	}
	_, err := t.exec(query, args...)
	return err
}

type storedGroup struct {
	Requests []cflow.WaitRequest
	State    cflow.WaitAnyState
}

func (t *txn) group(key string) (*storedGroup, error) {
	var data string
	err := t.tx.QueryRow(`SELECT data FROM wait_groups WHERE run_id = ? AND key = ?`, t.run, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var g storedGroup
	if err = json.Unmarshal([]byte(data), &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func (t *txn) saveGroup(key string, g *storedGroup) error {
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT OR REPLACE INTO wait_groups (run_id, key, data) VALUES (?, ?, ?)`, t.run, key, string(data))
	return err
}

// ---- lease.Infrastructure ----
// ---- lease.Infrastructure 接口实现 ----

func (s *Store) Claim(ctx context.Context, run, key string, ttl time.Duration) (lease *cflow.Lease, err error) {
	if ttl <= 0 {
		return nil, cflow.ErrInvalid
	}
	err = s.tx(ctx, run, func(t *txn) error {
		var generation, expires, held int64
		err := t.tx.QueryRow(`SELECT generation, expires_at, held FROM leases WHERE run_id = ? AND key = ?`, run, key).
			Scan(&generation, &expires, &held)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if held == 1 && tm(expires).After(t.now) {
			return cflow.ErrConflict
		}
		generation++
		lease = &cflow.Lease{Key: key, Generation: generation, ExpiresAt: t.now.Add(ttl)}
		_, err = t.exec(`INSERT OR REPLACE INTO leases (run_id, key, generation, expires_at, held) VALUES (?, ?, ?, ?, 1)`,
			run, key, generation, ts(lease.ExpiresAt))
		return err
	})
	return lease, err
}

func (t *txn) validLease(l cflow.Lease) (bool, error) {
	var generation, expires, held int64
	err := t.tx.QueryRow(`SELECT generation, expires_at, held FROM leases WHERE run_id = ? AND key = ?`, t.run, l.Key).
		Scan(&generation, &expires, &held)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && held == 1 && generation == l.Generation && tm(expires).After(t.now), err
}

func (s *Store) Renew(ctx context.Context, run string, l cflow.Lease, ttl time.Duration) (renewed *cflow.Lease, err error) {
	if ttl <= 0 {
		return nil, cflow.ErrInvalid
	}
	err = s.tx(ctx, run, func(t *txn) error {
		valid, err := t.validLease(l)
		if err != nil {
			return err
		}
		if !valid {
			return cflow.ErrConflict
		}
		l.ExpiresAt = t.now.Add(ttl)
		renewed = &l
		_, err = t.exec(`UPDATE leases SET expires_at = ? WHERE run_id = ? AND key = ?`, ts(l.ExpiresAt), run, l.Key)
		return err
	})
	return renewed, err
}

func (s *Store) Release(ctx context.Context, run string, l cflow.Lease) error {
	return s.tx(ctx, run, func(t *txn) error {
		result, err := t.exec(`UPDATE leases SET held = 0 WHERE run_id = ? AND key = ? AND generation = ? AND held = 1`,
			run, l.Key, l.Generation)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return cflow.ErrConflict
		}
		return nil
	})
}

// ---- cflow.Infrastructure ----
// ---- cflow.Infrastructure 接口实现 ----

func (s *Store) Get(ctx context.Context, run, key string) (record *cflow.Record, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		record, _, err = t.record(key)
		return err
	})
	return record, err
}

func (s *Store) Commit(ctx context.Context, run string, l cflow.Lease, r *cflow.Record, expected int64) (committed *cflow.Record, err error) {
	if r == nil || l.Key != r.Key || r.Kind != cflow.ActionKind {
		return nil, cflow.ErrInvalid
	}
	err = s.tx(ctx, run, func(t *txn) error {
		if g, err := t.group(r.Key); err != nil || g != nil {
			return errors.Join(err, cflow.ErrChanged)
		}
		valid, err := t.validLease(l)
		if err != nil {
			return err
		}
		if !valid {
			return cflow.ErrConflict
		}
		current, _, err := t.record(r.Key)
		if err != nil {
			return err
		}
		if (current == nil && expected != 0) || (current != nil && current.Version != expected) {
			return cflow.ErrConflict
		}
		if current != nil && (current.Kind != cflow.ActionKind || current.Status == cflow.Completed || current.Status == cflow.Failed) {
			return cflow.ErrConflict
		}
		if r.Status != cflow.Running && r.Status != cflow.Completed && r.Status != cflow.Failed {
			return cflow.ErrInvalid
		}
		c := *r
		c.Input, c.Result = clone(r.Input), clone(r.Result)
		c.Version = expected + 1
		if err = t.saveRecord(&c, ""); err != nil {
			return err
		}
		t.log("store.commit", c.Key, map[string]any{"kind": c.Kind, "status": c.Status, "record_version": c.Version, "generation": l.Generation})
		committed = &c
		return nil
	})
	return committed, err
}

func (s *Store) Put(ctx context.Context, run string, r *cflow.Record) (stored *cflow.Record, err error) {
	if r == nil || (r.Kind != cflow.CheckpointKind && r.Kind != cflow.DefinitionKind &&
		r.Kind != cflow.SignalKind && r.Kind != cflow.SleepKind) {
		return nil, cflow.ErrInvalid
	}
	err = s.tx(ctx, run, func(t *txn) error {
		if g, err := t.group(r.Key); err != nil || g != nil {
			return errors.Join(err, cflow.ErrChanged)
		}
		existing, _, err := t.record(r.Key)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Kind != r.Kind {
				return cflow.ErrChanged
			}
			stored = existing
			return nil
		}
		if ((r.Kind == cflow.SignalKind || r.Kind == cflow.SleepKind) && r.Status != cflow.Suspended) ||
			((r.Kind == cflow.CheckpointKind || r.Kind == cflow.DefinitionKind) && r.Status != cflow.Completed) {
			return cflow.ErrInvalid
		}
		c := *r
		c.Input, c.Result = clone(r.Input), clone(r.Result)
		c.Version = 1
		if err = t.saveRecord(&c, ""); err != nil {
			return err
		}
		switch c.Kind {
		case cflow.SignalKind:
			err = t.enqueue(&c, memory.RequestNotification, t.now)
		case cflow.SleepKind:
			err = t.enqueue(&c, memory.WakeNotification, c.Until)
		}
		fields := map[string]any{"kind": c.Kind, "status": c.Status, "record_version": c.Version}
		if !c.Until.IsZero() {
			fields["until"] = c.Until
		}
		t.log("store.put", c.Key, fields)
		stored = &c
		return err
	})
	return stored, err
}

func (s *Store) Resolve(ctx context.Context, run, key string, version int64, response []byte) (resolved bool, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		resolved, err = t.resolve(key, version, response)
		return err
	})
	return resolved, err
}

func (t *txn) resolve(key string, version int64, response []byte) (bool, error) {
	r, group, err := t.record(key)
	if err != nil {
		return false, err
	}
	if r == nil {
		return false, cflow.ErrNotFound
	}
	if r.Kind != cflow.SignalKind && r.Kind != cflow.SleepKind {
		return false, cflow.ErrInvalid
	}
	if group != "" {
		if err = t.advanceGroup(group); err != nil {
			return false, err
		}
		if r, _, err = t.record(key); err != nil {
			return false, err
		}
	}
	if r.Status == cflow.Completed || (group != "" && (r.Status == cflow.Cancelled || r.Status == cflow.Failed)) {
		return false, nil
	}
	if r.Status != cflow.Suspended {
		return false, cflow.ErrInvalid
	}
	if r.Version != version {
		return false, cflow.ErrConflict
	}
	if r.Kind == cflow.SleepKind && t.now.Before(r.Until) {
		return false, cflow.ErrConflict
	}
	if r.Kind == cflow.SignalKind && !json.Valid(response) {
		return false, cflow.ErrInvalid
	}
	return true, t.complete(r, group, response)
}

func (t *txn) complete(r *cflow.Record, group string, response []byte) error {
	c := *r
	c.Status = cflow.Completed
	c.Version++
	c.EndedAt = t.now
	c.Result = clone(response)
	if err := t.saveRecord(&c, group); err != nil {
		return err
	}
	// The request was answered; keep the delivery view honest.
	// 请求已被回答；保持投递视图准确。
	if err := t.closeMessages(c.Key, memory.RequestNotification, msgDone); err != nil {
		return err
	}
	if c.Kind == cflow.SleepKind {
		if err := t.closeMessages(c.Key, memory.WakeNotification, msgRemoved); err != nil {
			return err
		}
	}
	t.log("store.resolve", c.Key, map[string]any{"kind": c.Kind, "status": c.Status, "record_version": c.Version, "group": group})
	if group != "" {
		if err := t.settleGroup(group, &c); err != nil {
			return err
		}
	}
	return t.enqueue(&c, memory.WakeNotification, t.now)
}

// ---- memory.Outbox ----
// ---- memory.Outbox 接口实现 ----

func (s *Store) Pending(ctx context.Context, run string, now time.Time) (result []memory.Notification, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		result, err = t.pending(now, "")
		return err
	})
	return result, err
}

func (t *txn) pending(now time.Time, purpose string) ([]memory.Notification, error) {
	query := `SELECT id, key, purpose, available_at FROM messages WHERE run_id = ? AND state = ? AND available_at <= ?`
	args := []any{t.run, msgPending, ts(now)}
	if purpose != "" {
		query += ` AND purpose = ?`
		args = append(args, purpose)
	}
	rows, err := t.tx.Query(query+` ORDER BY available_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []memory.Notification
	for rows.Next() {
		var n memory.Notification
		var at int64
		if err = rows.Scan(&n.ID, &n.Key, &n.Purpose, &at); err != nil {
			return nil, err
		}
		n.AvailableAt = tm(at)
		result = append(result, n)
	}
	return result, rows.Err()
}

func (s *Store) Ack(ctx context.Context, run, id string) error {
	return s.tx(ctx, run, func(t *txn) error {
		_, err := t.exec(`UPDATE messages SET state = ?, done_at = ? WHERE run_id = ? AND id = ? AND state = ?`,
			msgAcked, ts(t.now), run, id, msgPending)
		return err
	})
}

// nextWake returns due WAKE notifications, or the time the earliest future
// one becomes due (zero when none is scheduled).
// nextWake 返回已到期的 WAKE 通知，或最早的未来通知到期时间
// （没有计划通知时为零值）。
func (s *Store) nextWake(ctx context.Context, run string) (due []memory.Notification, next time.Time, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		if due, err = t.pending(t.now, memory.WakeNotification); err != nil || len(due) > 0 {
			return err
		}
		var at sql.NullInt64
		err = t.tx.QueryRow(`SELECT MIN(available_at) FROM messages WHERE run_id = ? AND state = ? AND purpose = ?`,
			run, msgPending, memory.WakeNotification).Scan(&at)
		if at.Valid {
			next = tm(at.Int64)
		}
		return err
	})
	return due, next, err
}

// ---- cflow.WaitAnyInfrastructure ----
// ---- cflow.WaitAnyInfrastructure 接口实现 ----

func (s *Store) WaitAny(ctx context.Context, run, key string, requests []cflow.WaitRequest) (state *cflow.WaitAnyState, err error) {
	if key == "" || len(requests) == 0 {
		return nil, cflow.ErrInvalid
	}
	requests = append([]cflow.WaitRequest(nil), requests...)
	for i := range requests {
		if requests[i].Kind == "" {
			requests[i].Kind = cflow.SignalKind
		}
	}
	err = s.tx(ctx, run, func(t *txn) error {
		existing, err := t.group(key)
		if err != nil {
			return err
		}
		if existing != nil {
			if len(existing.Requests) != len(requests) {
				return cflow.ErrChanged
			}
			for i, request := range requests {
				if !cflow.SameWaitMember(existing.Requests[i], request) {
					return cflow.ErrChanged
				}
			}
			if err = t.advanceGroup(key); err != nil {
				return err
			}
			g, err := t.group(key)
			if err == nil {
				state = &g.State
			}
			return err
		}
		if r, _, err := t.record(key); err != nil || r != nil {
			return errors.Join(err, cflow.ErrChanged)
		}
		seen := map[string]bool{key: true}
		for _, request := range requests {
			if request.Key == "" || seen[request.Key] {
				return cflow.ErrInvalid
			}
			switch request.Kind {
			case cflow.SignalKind:
				if request.Error == "" && !json.Valid(request.Request) {
					return cflow.ErrInvalid
				}
			case cflow.SleepKind:
			default:
				return fmt.Errorf("%w: %s", cflow.ErrSignalUnsupported, request.Kind)
			}
			r, owner, err := t.record(request.Key)
			if err != nil {
				return err
			}
			g, err := t.group(request.Key)
			if err != nil {
				return err
			}
			if r != nil || g != nil || owner != "" {
				return cflow.ErrChanged
			}
			seen[request.Key] = true
		}
		g := &storedGroup{State: cflow.WaitAnyState{Status: cflow.Suspended}}
		for _, request := range requests {
			request.Request = clone(request.Request)
			g.Requests = append(g.Requests, request)
			record := &cflow.Record{Key: request.Key, Path: request.Path, Kind: request.Kind, Status: cflow.Suspended,
				Version: 1, Type: request.Type, Input: clone(request.Request), StartedAt: t.now}
			if request.Error != "" {
				record.Status, record.Error, record.EndedAt = cflow.Failed, request.Error, t.now
			}
			if request.Kind == cflow.SleepKind {
				record.Until = request.Until
				if record.Until.IsZero() {
					record.Until = t.now.Add(request.Duration)
				}
			}
			if err = t.saveRecord(record, key); err != nil {
				return err
			}
			if record.Status == cflow.Failed {
				g.State.Failures = append(g.State.Failures, cflow.WaitFailure{Key: record.Key, Message: record.Error})
				continue
			}
			g.State.Pending = append(g.State.Pending, pendingOf(record))
			if record.Kind == cflow.SleepKind {
				err = t.enqueue(record, memory.WakeNotification, record.Until)
			} else {
				err = t.enqueue(record, memory.RequestNotification, t.now)
			}
			if err != nil {
				return err
			}
		}
		if len(g.State.Pending) == 0 {
			g.State.Status = cflow.Failed
			if err = t.enqueueID(key+"|"+memory.WakeNotification+"|failed", key, memory.WakeNotification, t.now); err != nil {
				return err
			}
		}
		if err = t.saveGroup(key, g); err != nil {
			return err
		}
		t.log("store.wait_any", key, map[string]any{"status": g.State.Status, "pending": g.State.Pending})
		if err = t.advanceGroup(key); err != nil {
			return err
		}
		if g, err = t.group(key); err == nil {
			state = &g.State
		}
		return err
	})
	return state, err
}

func pendingOf(r *cflow.Record) cflow.Pending {
	return cflow.Pending{Key: r.Key, Path: r.Path, Kind: r.Kind, Until: r.Until}
}

// The path column stores Record.Path as JSON, so queries and the UI get the
// structured address instead of splitting the key.
// path 列以 JSON 存储 Record.Path，查询与 UI 拿到结构化地址，而不必拆分 key。
func encodePath(p cflow.Path) string {
	if len(p) == 0 {
		return ""
	}
	raw, _ := json.Marshal(p)
	return string(raw)
}

type pathColumn struct{ dst *cflow.Path }

func (c pathColumn) Scan(value any) error {
	var raw []byte
	switch v := value.(type) {
	case nil:
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return fmt.Errorf("unsupported path %T", value)
	}
	*c.dst = nil
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, c.dst)
}

// settleGroup selects the winner, cancels losers and withdraws their
// notifications in the resolving transaction.
// settleGroup 在解析事务中选择赢家、取消失败方并撤回它们的通知。
func (t *txn) settleGroup(key string, winner *cflow.Record) error {
	g, err := t.group(key)
	if err != nil || g == nil {
		return err
	}
	g.State.Status = cflow.Completed
	g.State.WinnerKey, g.State.WinnerKind = winner.Key, winner.Kind
	g.State.Response = clone(winner.Result)
	g.State.Pending = nil
	for _, request := range g.Requests {
		if err = t.closeMessages(request.Key, "", msgRemoved); err != nil {
			return err
		}
		record, _, err := t.record(request.Key)
		if err != nil {
			return err
		}
		if request.Key != winner.Key && record.Status == cflow.Suspended {
			record.Status = cflow.Cancelled
			record.Version++
			record.EndedAt = t.now
			if err = t.saveRecord(record, key); err != nil {
				return err
			}
			if err = t.enqueue(record, memory.CancelNotification, t.now); err != nil {
				return err
			}
			t.log("store.cancel", record.Key, map[string]any{"kind": record.Kind, "status": record.Status, "group": key})
		}
	}
	t.log("store.wait_any_settled", key, map[string]any{"status": cflow.Completed, "winner": winner.Key})
	return t.saveGroup(key, g)
}

// advanceGroup resolves the earliest due timer of a suspended group.
// advanceGroup 解析挂起组中最早到期的计时器。
func (t *txn) advanceGroup(key string) error {
	g, err := t.group(key)
	if err != nil || g == nil || g.State.Status != cflow.Suspended {
		return err
	}
	var earliest *cflow.Record
	for _, request := range g.Requests {
		record, _, err := t.record(request.Key)
		if err != nil {
			return err
		}
		if record.Kind == cflow.SleepKind && record.Status == cflow.Suspended && !record.Until.After(t.now) {
			if earliest == nil || record.Until.Before(earliest.Until) {
				earliest = record
			}
		}
	}
	if earliest == nil {
		return nil
	}
	return t.complete(earliest, key, nil)
}

func (s *Store) FailWait(ctx context.Context, run, key string, version int64, message string) (failed bool, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		failed, err = t.failWait(key, version, message)
		return err
	})
	return failed, err
}

func (t *txn) failWait(key string, version int64, message string) (bool, error) {
	record, groupKey, err := t.record(key)
	if err != nil {
		return false, err
	}
	if record == nil || groupKey == "" {
		return false, cflow.ErrNotFound
	}
	if err = t.advanceGroup(groupKey); err != nil {
		return false, err
	}
	g, err := t.group(groupKey)
	if err != nil {
		return false, err
	}
	if record, _, err = t.record(key); err != nil {
		return false, err
	}
	if g.State.Status != cflow.Suspended || record.Status != cflow.Suspended {
		return false, nil
	}
	if record.Version != version {
		return false, cflow.ErrConflict
	}
	record.Status = cflow.Failed
	record.Version++
	record.Error = message
	record.EndedAt = t.now
	if err = t.saveRecord(record, groupKey); err != nil {
		return false, err
	}
	if err = t.closeMessages(key, "", msgRemoved); err != nil {
		return false, err
	}
	t.log("store.fail_wait", key, map[string]any{"kind": record.Kind, "status": record.Status, "group": groupKey, "error": map[string]string{"message": message}})
	g.State.Failures = append(g.State.Failures, cflow.WaitFailure{Key: key, Message: message})
	g.State.Pending = nil
	for _, request := range g.Requests {
		member, _, err := t.record(request.Key)
		if err != nil {
			return false, err
		}
		if member.Status == cflow.Suspended {
			g.State.Pending = append(g.State.Pending, pendingOf(member))
		}
	}
	if len(g.State.Pending) == 0 {
		g.State.Status = cflow.Failed
		id := fmt.Sprintf("%s|%s|failed", groupKey, memory.WakeNotification)
		if err = t.enqueueID(id, groupKey, memory.WakeNotification, t.now); err != nil {
			return false, err
		}
	}
	return true, t.saveGroup(groupKey, g)
}

// ---- debugger operations used by the UI and the worker ----
// ---- UI 和 worker 使用的 debugger 操作 ----

func (s *Store) Register(ctx context.Context, spec RunSpec) (RunInfo, error) {
	if spec.ID == "" {
		spec.ID = spec.Workflow + "-" + uuid.NewString()
	}
	err := s.tx(ctx, spec.ID, func(t *txn) error {
		var status string
		if err := t.tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, spec.ID).Scan(&status); err != nil {
			return err
		}
		_, err := t.exec(`UPDATE runs SET workflow = ?, version = ?, input = ?, updated_at = ?, last_seen = ? WHERE id = ?`,
			spec.Workflow, spec.Version, []byte(spec.Input), ts(t.now), ts(t.now), spec.ID)
		t.log("run.attached", spec.Workflow, map[string]any{"status": status})
		return err
	})
	if err != nil {
		return RunInfo{}, err
	}
	return s.runInfo(ctx, spec.ID)
}

func (s *Store) Report(ctx context.Context, run string, report Report) error {
	pending, _ := json.Marshal(report.Pending)
	if report.Pending == nil {
		pending = []byte("[]")
	}
	return s.tx(ctx, run, func(t *txn) error {
		_, err := t.exec(`UPDATE runs SET status = ?, output = ?, error = ?, pending = ?, drives = drives + 1,
			updated_at = ?, last_seen = ? WHERE id = ?`,
			report.Status, []byte(report.Output), report.Error, string(pending), ts(t.now), ts(t.now), run)
		return err
	})
}

// Touch marks the run's worker as alive without creating a store event.
// Touch 将运行的 worker 标记为存活，但不会创建 store 事件。
func (s *Store) Touch(run string) {
	s.mu.Lock()
	_, _ = s.db.Exec(`UPDATE runs SET last_seen = ? WHERE id = ?`, ts(s.now()), run)
	s.mu.Unlock()
}

func (s *Store) AppendEvents(ctx context.Context, run string, events []json.RawMessage) error {
	if len(events) == 0 {
		return nil
	}
	return s.tx(ctx, run, func(t *txn) error {
		for _, event := range events {
			if !json.Valid(event) {
				return cflow.ErrInvalid
			}
			var identity struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(event, &identity); err != nil {
				return cflow.ErrInvalid
			}
			if identity.ID != "" {
				res, err := t.exec(`INSERT OR IGNORE INTO engine_event_ids (run_id, event_id) VALUES (?, ?)`, run, identity.ID)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					continue
				}
			}
			if _, err := t.exec(`INSERT INTO events (run_id, source, at, payload) VALUES (?, 'engine', ?, ?)`,
				run, ts(t.now), string(event)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Submit answers a suspended WaitFor (or WaitAny member) with a JSON payload.
// Submit 使用 JSON 负载回答一个挂起的 WaitFor（或 WaitAny 成员）。
func (s *Store) Submit(ctx context.Context, run, key string, response json.RawMessage) (accepted bool, err error) {
	if !json.Valid(response) {
		return false, fmt.Errorf("%w: response is not valid JSON", cflow.ErrInvalid)
	}
	err = s.tx(ctx, run, func(t *txn) error {
		r, _, err := t.record(key)
		if err != nil {
			return err
		}
		if r == nil {
			return cflow.ErrNotFound
		}
		if r.Kind != cflow.SignalKind {
			return fmt.Errorf("%w: %s is a %s, not a signal", cflow.ErrInvalid, key, r.Kind)
		}
		t.log("debugger.submit", key, map[string]any{"kind": r.Kind, "status": r.Status})
		accepted, err = t.resolve(key, r.Version, response)
		return err
	})
	return accepted, err
}

// FireSleep moves a pending timer's deadline to now and resolves it.
// FireSleep 将 pending 计时器的截止时间移动到当前时间并解析它。
func (s *Store) FireSleep(ctx context.Context, run, key string) error {
	return s.tx(ctx, run, func(t *txn) error {
		r, group, err := t.record(key)
		if err != nil {
			return err
		}
		if r == nil {
			return cflow.ErrNotFound
		}
		if r.Kind != cflow.SleepKind || r.Status != cflow.Suspended {
			return fmt.Errorf("%w: %s is not a pending sleep", cflow.ErrInvalid, key)
		}
		r.Until = t.now
		if err = t.saveRecord(r, group); err != nil {
			return err
		}
		t.log("debugger.fire_sleep", key, map[string]any{"kind": r.Kind, "status": r.Status})
		if group != "" {
			return t.advanceGroup(group)
		}
		return t.complete(r, "", nil)
	})
}

func (s *Store) FailMember(ctx context.Context, run, key, message string) (bool, error) {
	var failed bool
	err := s.tx(ctx, run, func(t *txn) error {
		r, _, err := t.record(key)
		if err != nil {
			return err
		}
		if r == nil {
			return cflow.ErrNotFound
		}
		failed, err = t.failWait(key, r.Version, message)
		return err
	})
	return failed, err
}

// Redrive enqueues a manual WAKE so the attached worker drives again.
// Redrive 入队一个手动 WAKE，使已连接的 worker 再次 drive。
func (s *Store) Redrive(ctx context.Context, run string) error {
	return s.tx(ctx, run, func(t *txn) error {
		t.log("debugger.redrive", run, nil)
		return t.enqueueID(fmt.Sprintf("$manual|%s|%d", memory.WakeNotification, t.now.UnixNano()),
			"$run", memory.WakeNotification, t.now)
	})
}

func (s *Store) Delete(ctx context.Context, run string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO deleted_runs (id) VALUES (?)`, run); err != nil {
		tx.Rollback()
		return err
	}
	for _, table := range []string{"records", "leases", "messages", "wait_groups", "events", "engine_event_ids"} {
		if _, err = tx.Exec(`DELETE FROM `+table+` WHERE run_id = ?`, run); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err = tx.Exec(`DELETE FROM runs WHERE id = ?`, run); err != nil {
		tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.hub.publish(run)
	return nil
}

// ---- read models ----
// ---- 读取模型 ----

const onlineWindow = 10 * time.Second

func scanRun(scan func(...any) error, now time.Time) (RunInfo, error) {
	var info RunInfo
	var pending string
	var created, updated, seen int64
	err := scan(&info.ID, &info.Workflow, &info.Version, blob{&info.Input}, blob{&info.Output}, &info.Status, &info.Error,
		&pending, &info.Drives, &created, &updated, &seen)
	if err != nil {
		return info, err
	}
	info.Input, info.Output = clone(info.Input), clone(info.Output)
	_ = json.Unmarshal([]byte(pending), &info.Pending)
	if info.Pending == nil {
		info.Pending = []cflow.Pending{}
	}
	info.CreatedAt, info.UpdatedAt, info.LastSeen = tm(created), tm(updated), tm(seen)
	info.Online = seen != 0 && now.Sub(info.LastSeen) < onlineWindow
	return info, nil
}

const runColumns = `id, workflow, version, input, output, status, error, pending, drives, created_at, updated_at, last_seen`

func (s *Store) progress() (map[string]Progress, error) {
	rows, err := s.db.Query(`SELECT run_id, status, COUNT(*) FROM records
		WHERE kind NOT IN (?, ?) GROUP BY run_id, status`, cflow.DefinitionKind, cflow.CheckpointKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Progress{}
	for rows.Next() {
		var run string
		var status cflow.Status
		var n int
		if err = rows.Scan(&run, &status, &n); err != nil {
			return nil, err
		}
		p := result[run]
		p.Total += n
		switch status {
		case cflow.Completed:
			p.Completed += n
		case cflow.Running:
			p.Running += n
		case cflow.Suspended:
			p.Suspended += n
		case cflow.Failed:
			p.Failed += n
		case cflow.Cancelled:
			p.Cancelled += n
		}
		result[run] = p
	}
	return result, rows.Err()
}

func (s *Store) ListRuns(ctx context.Context) ([]RunInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	progress, err := s.progress()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.now()
	runs := []RunInfo{}
	for rows.Next() {
		info, err := scanRun(rows.Scan, now)
		if err != nil {
			return nil, err
		}
		info.Progress = progress[info.ID]
		runs = append(runs, info)
	}
	return runs, rows.Err()
}

func (s *Store) runInfo(ctx context.Context, run string) (RunInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runInfoLocked(ctx, run)
}

func (s *Store) runInfoLocked(ctx context.Context, run string) (RunInfo, error) {
	info, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, run).Scan, s.now())
	if errors.Is(err, sql.ErrNoRows) {
		return info, cflow.ErrNotFound
	}
	if err != nil {
		return info, err
	}
	progress, err := s.progress()
	info.Progress = progress[run]
	return info, err
}

func (s *Store) Detail(ctx context.Context, run string) (*RunDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.runInfoLocked(ctx, run)
	if err != nil {
		return nil, err
	}
	detail := &RunDetail{Run: info, Records: []RecordView{}, Groups: []GroupView{}, Messages: []MessageView{}, Now: s.now()}
	rows, err := s.db.QueryContext(ctx, `SELECT key, path, kind, status, version, type_path, input, result, error, until, started_at, ended_at, updated_at, group_key
		FROM records WHERE run_id = ? ORDER BY started_at, rowid`, run)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v RecordView
		var until, started, ended, updated int64
		if err = rows.Scan(&v.Key, pathColumn{&v.Path}, &v.Kind, &v.Status, &v.Version, &v.Type, blob{&v.Input}, blob{&v.Result}, &v.Error,
			&until, &started, &ended, &updated, &v.Group); err != nil {
			rows.Close()
			return nil, err
		}
		v.Input, v.Result = clone(v.Input), clone(v.Result)
		v.Until, v.StartedAt, v.EndedAt, v.UpdatedAt = tm(until), tm(started), tm(ended), tm(updated)
		detail.Records = append(detail.Records, v)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT key, data FROM wait_groups WHERE run_id = ? ORDER BY key`, run)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key, data string
		if err = rows.Scan(&key, &data); err != nil {
			rows.Close()
			return nil, err
		}
		var g storedGroup
		_ = json.Unmarshal([]byte(data), &g)
		detail.Groups = append(detail.Groups, GroupView{Key: key, Requests: g.Requests, State: g.State})
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT id, key, purpose, state, available_at, created_at, done_at
		FROM messages WHERE run_id = ? ORDER BY created_at DESC, id LIMIT 300`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MessageView
		var available, created, done int64
		if err = rows.Scan(&m.ID, &m.Key, &m.Purpose, &m.State, &available, &created, &done); err != nil {
			return nil, err
		}
		m.AvailableAt, m.CreatedAt, m.DoneAt = tm(available), tm(created), tm(done)
		detail.Messages = append(detail.Messages, m)
	}
	return detail, rows.Err()
}

func (s *Store) Events(ctx context.Context, run string, after int64, limit int) ([]EventView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, source, at, payload FROM events
		WHERE run_id = ? AND id > ? ORDER BY id LIMIT ?`, run, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []EventView{}
	for rows.Next() {
		var e EventView
		var at int64
		var payload string
		if err = rows.Scan(&e.ID, &e.Source, &at, &payload); err != nil {
			return nil, err
		}
		e.At, e.Event = tm(at), json.RawMessage(payload)
		events = append(events, e)
	}
	return events, rows.Err()
}

// hub fans out "run changed" notifications to SSE clients and long polls.
// hub 将 "run changed" 通知扇出给 SSE 客户端和长轮询。
type hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func newHub() *hub { return &hub{subs: map[chan string]struct{}{}} }

func (h *hub) subscribe() (chan string, func()) {
	ch := make(chan string, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *hub) publish(run string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- run:
		default: // a slow consumer refetches on its next notification
			// 慢消费者会在下一次通知时重新拉取。
		}
	}
}

func sortPending(p []cflow.Pending) {
	sort.Slice(p, func(i, j int) bool { return p[i].Key < p[j].Key })
}
