package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/k0923/go/tflow"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS tflow_schema (version INTEGER NOT NULL);
INSERT INTO tflow_schema SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM tflow_schema);
CREATE TABLE IF NOT EXISTS tflow_mains (
 id TEXT PRIMARY KEY, version INTEGER NOT NULL, data BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS tflow_subs (
 main_id TEXT NOT NULL REFERENCES tflow_mains(id), key TEXT NOT NULL,
 cursor INTEGER NOT NULL, version INTEGER NOT NULL, status TEXT NOT NULL,
 data BLOB NOT NULL, PRIMARY KEY(main_id,key), UNIQUE(main_id,cursor)
);
CREATE INDEX IF NOT EXISTS tflow_sub_status ON tflow_subs(main_id,status,cursor);
CREATE TABLE IF NOT EXISTS tflow_commands (
 main_id TEXT NOT NULL REFERENCES tflow_mains(id), id TEXT NOT NULL,
 kind TEXT NOT NULL, identity TEXT NOT NULL, data BLOB,
 generation INTEGER NOT NULL DEFAULT 0, expires_at INTEGER NOT NULL DEFAULT 0,
 not_before INTEGER NOT NULL DEFAULT 0, done INTEGER NOT NULL DEFAULT 0,
 done_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 blocked INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(main_id,id)
);
CREATE INDEX IF NOT EXISTS tflow_command_due ON tflow_commands(done,not_before,expires_at,main_id);
`

type Options struct {
	Codec Codec
	// CommitPlan maps business pagination into a new immutable task. Nil preserves
	// the old task. It runs after CAS validation, inside the database transaction.
	CommitPlan func(tflow.MainTask, tflow.Plan) (tflow.MainTask, error)
}

type Repository struct {
	db         *sql.DB
	codec      Codec
	commitPlan func(tflow.MainTask, tflow.Plan) (tflow.MainTask, error)
}

type Store struct {
	repository *Repository
	id         string
}

var _ tflow.Store = (*Store)(nil)
var _ tflow.CommandReader = (*Store)(nil)
var _ tflow.Repository = (*Repository)(nil)

// New opens a file database using WAL, foreign keys, busy timeout, and immediate
// write transactions. Independent repositories/processes share database locks.
func New(ctx context.Context, path string, opts Options) (*Repository, error) {
	if path == "" || tflow.IsNil(opts.Codec) {
		return nil, tflow.ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	q := url.Values{}
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	r := &Repository{db: db, codec: opts.Codec, commitPlan: opts.CommitPlan}
	fail := func(cause error) (*Repository, error) {
		return nil, errors.Join(cause, db.Close())
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return fail(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	if _, err = tx.ExecContext(ctx, schema); err == nil {
		var version int
		err = tx.QueryRowContext(ctx, "SELECT version FROM tflow_schema").Scan(&version)
		if err == nil && version != 1 {
			err = fmt.Errorf("unsupported tflow sqlite schema version %d", version)
		}
	}
	if err != nil {
		return fail(errors.Join(err, tx.Rollback()))
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return r, nil
}

func (r *Repository) Close() error { return r.db.Close() }

func (r *Repository) Open(ctx context.Context, id string) (tflow.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, tflow.ErrInvalid
	}
	return &Store{repository: r, id: id}, nil
}

func (s *Store) MainID() string { return s.id }

type reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readMain(ctx context.Context, db reader, s *Store) (tflow.MainRecord, error) {
	var data []byte
	var summary tflow.SubSummary
	query := `SELECT data,
(SELECT COUNT(*) FROM tflow_subs WHERE main_id=m.id AND status='PENDING'),
(SELECT COUNT(*) FROM tflow_subs WHERE main_id=m.id AND status='RUNNING'),
(SELECT COUNT(*) FROM tflow_subs WHERE main_id=m.id AND status='SUCCEEDED'),
(SELECT COUNT(*) FROM tflow_subs WHERE main_id=m.id AND status='FAILED'),
(SELECT COUNT(*) FROM tflow_subs WHERE main_id=m.id AND status='CANCELED')
FROM tflow_mains m WHERE id=?`
	if err := db.QueryRowContext(ctx, query, s.id).Scan(&data, &summary.Pending, &summary.Running, &summary.Succeeded, &summary.Failed, &summary.Canceled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tflow.MainRecord{}, tflow.ErrNotFound
		}
		return tflow.MainRecord{}, err
	}
	m, err := s.repository.decodeMain(data)
	if err != nil {
		return m, err
	}
	m.Subs = summary
	return m, nil
}

// One SQL statement reads the main and summary from the same SQLite snapshot.
func (s *Store) GetMain(ctx context.Context) (tflow.MainRecord, error) {
	return readMain(ctx, s.repository.db, s)
}

func readSub(ctx context.Context, db reader, s *Store, key string) (tflow.SubRecord, error) {
	var data []byte
	err := db.QueryRowContext(ctx, "SELECT data FROM tflow_subs WHERE main_id=? AND key=?", s.id, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return tflow.SubRecord{}, tflow.ErrNotFound
	}
	if err != nil {
		return tflow.SubRecord{}, err
	}
	return s.repository.decodeSub(data)
}

func (s *Store) GetSub(ctx context.Context, key string) (tflow.SubRecord, error) {
	return readSub(ctx, s.repository.db, s, key)
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func findSubs(ctx context.Context, db reader, s *Store, q tflow.SubQuery) ([]tflow.SubRecord, error) {
	if q.Limit < 0 {
		return nil, tflow.ErrInvalid
	}
	// A query against a missing aggregate must return ErrNotFound, not an empty page.
	var present int
	if err := db.QueryRowContext(ctx, "SELECT 1 FROM tflow_mains WHERE id=?", s.id).Scan(&present); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, tflow.ErrNotFound
		}
		return nil, err
	}
	query := "SELECT data FROM tflow_subs WHERE main_id=?"
	args := []any{s.id}
	if q.AfterCursor != nil {
		query += " AND cursor>?"
		args = append(args, *q.AfterCursor)
	}
	if len(q.Statuses) > 0 {
		query += " AND status IN (" + placeholders(len(q.Statuses)) + ")"
		for _, status := range q.Statuses {
			args = append(args, status)
		}
	}
	if len(q.Keys) > 0 {
		query += " AND key IN (" + placeholders(len(q.Keys)) + ")"
		for _, key := range q.Keys {
			args = append(args, key)
		}
	}
	query += " ORDER BY cursor"
	if q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]tflow.SubRecord, 0)
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		sub, err := s.repository.decodeSub(data)
		if err != nil {
			return nil, err
		}
		result = append(result, sub)
	}
	return result, rows.Err()
}

func (s *Store) FindSubs(ctx context.Context, q tflow.SubQuery) ([]tflow.SubRecord, error) {
	return findSubs(ctx, s.repository.db, s, q)
}

type transaction struct {
	db     *sql.Tx
	store  *Store
	ctx    context.Context
	active bool
}

func (s *Store) WithTx(ctx context.Context, fn func(tflow.Tx) error) (err error) {
	if fn == nil {
		return tflow.ErrInvalid
	}
	db, err := s.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tx := &transaction{db: db, store: s, ctx: ctx, active: true}
	defer func() {
		tx.active = false
		rollbackErr := db.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	before, beforeErr := tx.Main()
	if beforeErr != nil && !errors.Is(beforeErr, tflow.ErrNotFound) {
		return beforeErr
	}
	if err := fn(tx); err != nil {
		return err
	}
	m, err := tx.Main()
	if err != nil {
		return err
	}
	if err := tflow.ValidateMain(m); err != nil {
		return err
	}
	if beforeErr == nil && before.Subs != m.Subs && m.Version <= before.Version {
		return fmt.Errorf("%w: summary changed without main version", tflow.ErrInvalid)
	}
	return db.Commit()
}

func (tx *transaction) check() error {
	if !tx.active {
		return tflow.ErrInvalid
	}
	return tx.ctx.Err()
}
func (tx *transaction) Main() (tflow.MainRecord, error) {
	if err := tx.check(); err != nil {
		return tflow.MainRecord{}, err
	}
	return readMain(tx.ctx, tx.db, tx.store)
}
func (tx *transaction) Sub(key string) (tflow.SubRecord, error) {
	if err := tx.check(); err != nil {
		return tflow.SubRecord{}, err
	}
	return readSub(tx.ctx, tx.db, tx.store, key)
}
func (tx *transaction) FindSubs(q tflow.SubQuery) ([]tflow.SubRecord, error) {
	if err := tx.check(); err != nil {
		return nil, err
	}
	return findSubs(tx.ctx, tx.db, tx.store, q)
}

func (tx *transaction) CreateMain(m tflow.MainRecord) error {
	if err := tx.check(); err != nil {
		return err
	}
	_, err := tx.Main()
	if err == nil {
		return tflow.ErrAlreadyExists
	}
	if !errors.Is(err, tflow.ErrNotFound) {
		return err
	}
	if m.Version != 1 || tflow.IsNil(m.Task) {
		return tflow.ErrInvalid
	}
	if err := tflow.ValidateMainState(m.MainConfig, m.MainState); err != nil {
		return err
	}
	data, err := tx.store.repository.encodeMain(m)
	if err != nil {
		return err
	}
	_, err = tx.db.ExecContext(tx.ctx, "INSERT INTO tflow_mains(id,version,data) VALUES(?,?,?)", tx.store.id, m.Version, data)
	return err
}

func (tx *transaction) saveRecord(expected int64, m tflow.MainRecord) error {
	if m.Version != expected+1 || tflow.IsNil(m.Task) {
		return tflow.ErrInvalid
	}
	if err := tflow.ValidateMainState(m.MainConfig, m.MainState); err != nil {
		return err
	}
	data, err := tx.store.repository.encodeMain(m)
	if err != nil {
		return err
	}
	result, err := tx.db.ExecContext(tx.ctx, "UPDATE tflow_mains SET version=?,data=? WHERE id=? AND version=?", m.Version, data, tx.store.id, expected)
	return requireChange(result, err, tflow.ErrConflict)
}

func (tx *transaction) CommitPlan(expected int64, next tflow.MainRecord, plan tflow.Plan) error {
	if err := tx.check(); err != nil {
		return err
	}
	if tflow.IsNil(plan) {
		return tflow.ErrInvalid
	}
	if expected == 0 {
		if _, err := tx.Main(); err == nil {
			return tflow.ErrAlreadyExists
		} else if !errors.Is(err, tflow.ErrNotFound) {
			return err
		}
		if next.Version != 1 {
			return tflow.ErrInvalid
		}
	} else {
		current, err := tx.Main()
		if err != nil {
			return err
		}
		if current.Version != expected {
			return tflow.ErrConflict
		}
		if next.Version != expected+1 {
			return tflow.ErrInvalid
		}
		next.Task, next.MainConfig = current.Task, current.MainConfig
	}
	if tx.store.repository.commitPlan != nil {
		task, err := tx.store.repository.commitPlan(next.Task, plan)
		if err != nil {
			return err
		}
		next.Task = task
	}
	if expected == 0 {
		return tx.CreateMain(next)
	}
	return tx.saveRecord(expected, next)
}

func (tx *transaction) SaveMain(expected int64, state tflow.MainState) error {
	m, err := tx.Main()
	if err != nil {
		return err
	}
	if m.Version != expected {
		return tflow.ErrConflict
	}
	m.MainState = state
	return tx.saveRecord(expected, m)
}

func (tx *transaction) SaveTask(expected int64, task tflow.MainTask, now time.Time) error {
	m, err := tx.Main()
	if err != nil {
		return err
	}
	if m.Version != expected {
		return tflow.ErrConflict
	}
	if m.Status.Terminal() || tflow.IsNil(task) {
		return tflow.ErrInvalid
	}
	m.Task, m.Version, m.UpdatedAt = task, m.Version+1, now
	return tx.saveRecord(expected, m)
}

func (tx *transaction) CreateSubs(subs []tflow.SubRecord) error {
	if err := tx.check(); err != nil {
		return err
	}
	for _, sub := range subs {
		if sub.Version != 1 {
			return tflow.ErrInvalid
		}
		if err := tflow.ValidateSub(sub); err != nil {
			return err
		}
		var exists int
		err := tx.db.QueryRowContext(tx.ctx, "SELECT 1 FROM tflow_subs WHERE main_id=? AND (key=? OR cursor=?) LIMIT 1", tx.store.id, sub.Key, sub.Cursor).Scan(&exists)
		if err == nil {
			return &tflow.PlanError{MainID: tx.store.id, Field: "identity", Index: -1, Cause: tflow.ErrInvalidPlan, Reason: "duplicate subtask key or cursor"}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		data, err := tx.store.repository.encodeSub(sub)
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(tx.ctx, "INSERT INTO tflow_subs(main_id,key,cursor,version,status,data) VALUES(?,?,?,?,?,?)", tx.store.id, sub.Key, sub.Cursor, sub.Version, sub.Status, data); err != nil {
			return err
		}
	}
	return nil
}

func (tx *transaction) SaveSub(expected int64, state tflow.SubState) error {
	old, err := tx.Sub(state.Key)
	if err != nil {
		return err
	}
	if old.Version != expected {
		return tflow.ErrConflict
	}
	if state.Version != expected+1 || state.Cursor != old.Cursor {
		return tflow.ErrInvalid
	}
	old.SubState = state
	if err := tflow.ValidateSub(old); err != nil {
		return err
	}
	data, err := tx.store.repository.encodeSub(old)
	if err != nil {
		return err
	}
	result, err := tx.db.ExecContext(tx.ctx, "UPDATE tflow_subs SET version=?,status=?,data=? WHERE main_id=? AND key=? AND version=?", state.Version, state.Status, data, tx.store.id, state.Key, expected)
	return requireChange(result, err, tflow.ErrConflict)
}

// Only scheduling identity is compared; opaque payloads need not be equal.
func commandIdentity(c tflow.Command) (string, error) {
	data, err := jsonIdentity(c)
	return string(data), err
}

func (tx *transaction) PutCommand(c tflow.Command) error {
	if err := tx.check(); err != nil {
		return err
	}
	if c.MainID != tx.store.id || c.ID == "" {
		return tflow.ErrInvalid
	}
	identity, err := commandIdentity(c)
	if err != nil {
		return err
	}
	var old string
	err = tx.db.QueryRowContext(tx.ctx, "SELECT identity FROM tflow_commands WHERE main_id=? AND id=?", tx.store.id, c.ID).Scan(&old)
	if err == nil {
		if old != identity {
			return tflow.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	data, err := tx.store.repository.encodeCommand(c)
	if err != nil {
		return err
	}
	_, err = tx.db.ExecContext(tx.ctx, "INSERT INTO tflow_commands(main_id,id,kind,identity,data,not_before) VALUES(?,?,?,?,?,?)", tx.store.id, c.ID, c.Kind, identity, data, millis(c.NotBefore))
	return err
}

func requireChange(result sql.Result, err error, missing error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return missing
	}
	return nil
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
func timestamp(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.UnixMilli(n).UTC()
}

func databaseNow(ctx context.Context, db reader) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, "SELECT CAST(unixepoch('subsec')*1000 AS INTEGER)").Scan(&n)
	return n, err
}

func (s *Store) ClaimCommand(ctx context.Context, ttl time.Duration) (delivery *tflow.Delivery, err error) {
	if ttl < time.Millisecond {
		return nil, tflow.ErrInvalid
	}
	db, err := s.repository.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := db.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, e)
		}
	}()
	now, err := databaseNow(ctx, db)
	if err != nil {
		return nil, err
	}
	var id string
	var data []byte
	var generation, notBefore int64
	err = db.QueryRowContext(ctx, `SELECT id,data,generation,not_before FROM tflow_commands
WHERE main_id=? AND done=0 AND blocked=0 AND not_before<=? AND expires_at<=? ORDER BY not_before,id LIMIT 1`, s.id, now, now).Scan(&id, &data, &generation, &notBefore)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if e := db.QueryRowContext(ctx, "SELECT 1 FROM tflow_mains WHERE id=?", s.id).Scan(&exists); errors.Is(e, sql.ErrNoRows) {
			return nil, tflow.ErrNotFound
		} else if e != nil {
			return nil, e
		}
		return nil, db.Commit()
	}
	if err != nil {
		return nil, err
	}
	c, err := s.repository.decodeCommand(data)
	if err != nil {
		return nil, err
	}
	expires := now + ttl.Milliseconds()
	if _, err := db.ExecContext(ctx, "UPDATE tflow_commands SET generation=generation+1,expires_at=? WHERE main_id=? AND id=?", expires, s.id, id); err != nil {
		return nil, err
	}
	if err := db.Commit(); err != nil {
		return nil, err
	}
	c.NotBefore = timestamp(notBefore)
	return &tflow.Delivery{Command: c, Generation: generation + 1, ExpiresAt: timestamp(expires)}, nil
}

const sqlNow = "CAST(unixepoch('subsec')*1000 AS INTEGER)"

func (s *Store) AckCommand(ctx context.Context, d tflow.Delivery) error {
	if d.Command.MainID != s.id {
		return tflow.ErrDeliveryLeaseLost
	}
	result, err := s.repository.db.ExecContext(ctx, "UPDATE tflow_commands SET done=1,done_at="+sqlNow+",expires_at=0 WHERE main_id=? AND id=? AND generation=? AND done=0 AND blocked=0 AND expires_at>"+sqlNow, s.id, d.Command.ID, d.Generation)
	return requireChange(result, err, tflow.ErrDeliveryLeaseLost)
}

func (s *Store) ReleaseCommand(ctx context.Context, d tflow.Delivery, delay time.Duration, lastError string) error {
	if delay < 0 {
		return tflow.ErrInvalid
	}
	if d.Command.MainID != s.id {
		return tflow.ErrDeliveryLeaseLost
	}
	ms := delay.Milliseconds()
	if delay%time.Millisecond != 0 {
		ms++
	}
	result, err := s.repository.db.ExecContext(ctx, "UPDATE tflow_commands SET expires_at=0,not_before="+sqlNow+"+?,last_error=? WHERE main_id=? AND id=? AND generation=? AND done=0 AND blocked=0 AND expires_at>"+sqlNow, ms, lastError, s.id, d.Command.ID, d.Generation)
	return requireChange(result, err, tflow.ErrDeliveryLeaseLost)
}

func (s *Store) PendingCommands(ctx context.Context) ([]tflow.CommandState, error) {
	return s.FindCommands(ctx, tflow.CommandQuery{})
}

func (s *Store) FindCommands(ctx context.Context, q tflow.CommandQuery) ([]tflow.CommandState, error) {
	return s.findCommands(ctx, q, false)
}

func (s *Store) findCommands(ctx context.Context, q tflow.CommandQuery, blockedOnly bool) ([]tflow.CommandState, error) {
	if q.Limit < 0 {
		return nil, tflow.ErrInvalid
	}
	var exists int
	if err := s.repository.db.QueryRowContext(ctx, "SELECT 1 FROM tflow_mains WHERE id=?", s.id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, tflow.ErrNotFound
		}
		return nil, err
	}
	query := "SELECT data,generation,expires_at,not_before,last_error,blocked FROM tflow_commands WHERE main_id=? AND done=0 AND id>?"
	args := []any{s.id, q.AfterID}
	if blockedOnly {
		query += " AND blocked=1"
	}
	if len(q.Kinds) > 0 {
		query += " AND kind IN (" + placeholders(len(q.Kinds)) + ")"
		for _, kind := range q.Kinds {
			args = append(args, kind)
		}
	}
	query += " ORDER BY id"
	if q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := s.repository.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]tflow.CommandState, 0)
	for rows.Next() {
		var data []byte
		var state tflow.CommandState
		var expiry, notBefore int64
		if err := rows.Scan(&data, &state.Generation, &expiry, &notBefore, &state.LastError, &state.Blocked); err != nil {
			return nil, err
		}
		c, err := s.repository.decodeCommand(data)
		if err != nil {
			return nil, err
		}
		c.NotBefore = timestamp(notBefore)
		state.Command, state.ExpiresAt = c, timestamp(expiry)
		result = append(result, state)
	}
	return result, rows.Err()
}

// DueMains includes terminal mains with outstanding cancel/notification work.
func (r *Repository) DueMains(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, tflow.ErrInvalid
	}
	rows, err := r.db.QueryContext(ctx, "SELECT main_id FROM tflow_commands WHERE done=0 AND blocked=0 AND not_before<="+sqlNow+" AND expires_at<="+sqlNow+" GROUP BY main_id ORDER BY MIN(not_before),main_id LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// QuarantineCommand durably isolates an owned delivery without discarding it.
// Workers choose their escalation policy; no business failure is inferred here.
func (s *Store) QuarantineCommand(ctx context.Context, d tflow.Delivery, reason string) error {
	if d.Command.MainID != s.id {
		return tflow.ErrDeliveryLeaseLost
	}
	result, err := s.repository.db.ExecContext(ctx, "UPDATE tflow_commands SET blocked=1,expires_at=0,last_error=? WHERE main_id=? AND id=? AND generation=? AND done=0 AND blocked=0 AND expires_at>"+sqlNow, reason, s.id, d.Command.ID, d.Generation)
	return requireChange(result, err, tflow.ErrDeliveryLeaseLost)
}

func (s *Store) DeadLetters(ctx context.Context, q tflow.CommandQuery) ([]tflow.CommandState, error) {
	return s.findCommands(ctx, q, true)
}

// RequeueCommand explicitly reactivates a quarantined command, preserving its
// identity and payload. Increment generation to fence any old delivery owner.
func (s *Store) RequeueCommand(ctx context.Context, id string) error {
	result, err := s.repository.db.ExecContext(ctx, "UPDATE tflow_commands SET blocked=0,generation=generation+1,not_before="+sqlNow+",expires_at=0 WHERE main_id=? AND id=? AND done=0 AND blocked=1", s.id, id)
	return requireChange(result, err, tflow.ErrNotFound)
}

// CompactAcknowledged removes payloads but preserves identity tombstones, so an
// old PutCommand cannot resurrect acknowledged work. Retention is caller-owned.
func (r *Repository) CompactAcknowledged(ctx context.Context, before time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx, "UPDATE tflow_commands SET data=NULL,last_error='' WHERE done=1 AND done_at<? AND data IS NOT NULL", millis(before))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
