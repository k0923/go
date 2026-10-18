// Package sqlite provides durable, run-scoped dflow storage and an outbox.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/lease"
	_ "modernc.org/sqlite"
)

var _ dflow.Infrastructure = (*Infrastructure)(nil)
var _ dflow.Outbox = (*Infrastructure)(nil)

type Infrastructure struct {
	db    *sql.DB
	scope string
}

// Open creates the schema and binds this handle to scope (e.g. tenant/run ID).
// Multiple handles/processes can share the file. The directory must exist.
// WAL + synchronous FULL protects committed checkpoints across process crashes.
func Open(ctx context.Context, path, scope string) (*Infrastructure, error) {
	if path == "" || scope == "" {
		return nil, fmt.Errorf("sqlite dflow: path and scope are required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	query := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)"}, "_txlock": {"immediate"}}
	dsn := url.URL{Scheme: "file", Path: absolute, RawQuery: query.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var legacy int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='dflow_records'`).Scan(&legacy)
	if err != nil || legacy != 0 {
		db.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("sqlite dflow: legacy operation-checkpoint schema requires an explicit migration or a new database")
	}
	_, err = db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS dflow_runs (
 scope TEXT PRIMARY KEY, state BLOB, version INTEGER NOT NULL DEFAULT 0,
 generation INTEGER NOT NULL DEFAULT 0, lease_key TEXT NOT NULL DEFAULT '',
 expires INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS dflow_waits (
 scope TEXT NOT NULL, node TEXT NOT NULL, token TEXT NOT NULL, response BLOB,
 PRIMARY KEY(scope,node,token)
);
CREATE TABLE IF NOT EXISTS dflow_nodes (
 scope TEXT NOT NULL, node TEXT NOT NULL, state BLOB NOT NULL,
 ready INTEGER NOT NULL, status TEXT NOT NULL, generation INTEGER NOT NULL,
 PRIMARY KEY(scope,node)
);
CREATE INDEX IF NOT EXISTS dflow_nodes_ready ON dflow_nodes(scope,ready,status,generation);
CREATE TABLE IF NOT EXISTS dflow_publications (
 id TEXT PRIMARY KEY, manifest BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS dflow_definitions (
 publication TEXT NOT NULL,node TEXT NOT NULL,definition BLOB NOT NULL,incoming BLOB NOT NULL,
 PRIMARY KEY(publication,node)
);
CREATE TABLE IF NOT EXISTS dflow_outbox (
 id INTEGER PRIMARY KEY AUTOINCREMENT, scope TEXT NOT NULL,
 kind TEXT NOT NULL, node TEXT NOT NULL, token TEXT NOT NULL, request BLOB
);
CREATE INDEX IF NOT EXISTS dflow_outbox_scope ON dflow_outbox(scope,id);
`)
	if err == nil {
		_, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO dflow_runs(scope) VALUES (?)`, scope)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Infrastructure{db: db, scope: scope}, nil
}

func (i *Infrastructure) Close() error { return i.db.Close() }

func (i *Infrastructure) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (i *Infrastructure) Claim(ctx context.Context, key string, ttl time.Duration) (*lease.Token, error) {
	if key != "dflow/run" || ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	now := time.Now()
	token := &lease.Token{Key: key, ExpiresAt: now.Add(ttl)}
	err := i.db.QueryRowContext(ctx, `UPDATE dflow_runs
SET generation=generation+1,lease_key=?,expires=? WHERE scope=? AND expires<=? RETURNING generation`,
		key, token.ExpiresAt.UnixNano(), i.scope, now.UnixNano()).Scan(&token.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, lease.ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return token, nil
}

func (i *Infrastructure) Renew(ctx context.Context, token lease.Token, ttl time.Duration) (*lease.Token, error) {
	if ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	now := time.Now()
	token.ExpiresAt = now.Add(ttl)
	result, err := i.db.ExecContext(ctx, `UPDATE dflow_runs SET expires=?
WHERE scope=? AND lease_key=? AND generation=? AND expires>?`,
		token.ExpiresAt.UnixNano(), i.scope, token.Key, token.Generation, now.UnixNano())
	if err := affected(result, err, lease.ErrConflict); err != nil {
		return nil, err
	}
	return &token, nil
}

func (i *Infrastructure) Release(ctx context.Context, token lease.Token) error {
	result, err := i.db.ExecContext(ctx, `UPDATE dflow_runs SET expires=0,lease_key=''
WHERE scope=? AND lease_key=? AND generation=?`, i.scope, token.Key, token.Generation)
	return affected(result, err, lease.ErrConflict)
}

func affected(result sql.Result, err, conflict error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return conflict
	}
	return nil
}

func (i *Infrastructure) checkLease(ctx context.Context, tx *sql.Tx, token lease.Token) error {
	var generation, expires int64
	var key string
	if err := tx.QueryRowContext(ctx, `SELECT generation,lease_key,expires FROM dflow_runs WHERE scope=?`,
		i.scope).Scan(&generation, &key, &expires); err != nil {
		return err
	}
	if generation != token.Generation || key != token.Key || expires <= time.Now().UnixNano() {
		return lease.ErrConflict
	}
	return nil
}

func (i *Infrastructure) Load(ctx context.Context) (*dflow.State, error) {
	state, err := i.LoadMetadata(ctx)
	if err != nil || state == nil {
		return state, err
	}
	state.Nodes = map[string]dflow.NodeState{}
	rows, err := i.db.QueryContext(ctx, `SELECT node,state FROM dflow_nodes WHERE scope=?`, i.scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var node dflow.NodeState
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, err
		}
		state.Nodes[id] = node
	}
	return state, rows.Err()
}

type reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (i *Infrastructure) getResponse(ctx context.Context, query reader, nodeID, token string) (*dflow.Response, error) {
	var data []byte
	err := query.QueryRowContext(ctx, `SELECT response FROM dflow_waits WHERE scope=? AND node=? AND token=?`,
		i.scope, nodeID, token).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, dflow.ErrWaitNotFound
	}
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	return &dflow.Response{Value: data}, nil
}

func (i *Infrastructure) GetResponse(ctx context.Context, nodeID, token string) (*dflow.Response, error) {
	return i.getResponse(ctx, i.db, nodeID, token)
}

func (i *Infrastructure) notify(ctx context.Context, tx *sql.Tx, notification dflow.Notification) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO dflow_outbox(scope,kind,node,token,request) VALUES(?,?,?,?,?)`,
		i.scope, string(notification.Kind), notification.NodeID, notification.Token, []byte(notification.Request))
	return err
}

func (i *Infrastructure) Resolve(ctx context.Context, nodeID, token string, value json.RawMessage) (accepted bool, err error) {
	err = i.transaction(ctx, func(tx *sql.Tx) error {
		response, err := i.getResponse(ctx, tx, nodeID, token)
		if err != nil {
			return err
		}
		if response != nil {
			return nil
		}
		node, err := i.getNode(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if node == nil || node.Suspension == nil || node.Suspension.Token != token {
			return nil
		}
		if !json.Valid(value) {
			return dflow.ErrInvalidSuspension
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dflow_waits SET response=? WHERE scope=? AND node=? AND token=?`,
			[]byte(value), i.scope, nodeID, token); err != nil {
			return err
		}
		if err := i.notify(ctx, tx, dflow.Notification{Kind: dflow.WakeNotification, NodeID: nodeID, Token: token}); err != nil {
			return err
		}
		node.Ready = true
		node.Version++
		if err := i.putNode(ctx, tx, nodeID, *node); err != nil {
			return err
		}
		accepted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return accepted, nil
}

func (i *Infrastructure) Pending(ctx context.Context) ([]dflow.Notification, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT id,kind,node,token,request FROM dflow_outbox WHERE scope=? ORDER BY id`, i.scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []dflow.Notification
	for rows.Next() {
		var item dflow.Notification
		var request []byte
		if err := rows.Scan(&item.ID, &item.Kind, &item.NodeID, &item.Token, &request); err != nil {
			return nil, err
		}
		item.Request = request
		result = append(result, item)
	}
	return result, rows.Err()
}

func (i *Infrastructure) Ack(ctx context.Context, id int64) error {
	_, err := i.db.ExecContext(ctx, `DELETE FROM dflow_outbox WHERE scope=? AND id=?`, i.scope, id)
	return err
}
