// Package sqlite implements a durable, run-scoped dflow Infrastructure.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/lease"
	_ "modernc.org/sqlite"
)

var _ dflow.Infrastructure = (*Infrastructure)(nil)

func init() {
	gob.Register(map[string]any{})
	gob.Register([]any{})
}

type Publication[R any] struct {
	records map[string]dflow.PublishedNode[R]
	order   []string
}

func Publish[R any](records []dflow.PublishedNode[R]) (*Publication[R], error) {
	if len(records) == 0 {
		return nil, dflow.ErrInvalidDefinition
	}
	p := &Publication[R]{records: map[string]dflow.PublishedNode[R]{}, order: make([]string, 0, len(records))}
	for _, record := range records {
		if record == nil || record.GetID() == "" {
			return nil, dflow.ErrInvalidDefinition
		}
		id := record.GetID()
		if _, exists := p.records[id]; exists {
			return nil, dflow.ErrInvalidDefinition
		}
		p.records[id] = record
		p.order = append(p.order, id)
	}
	return p, nil
}

func (p *Publication[R]) LoadNode(ctx context.Context, id string) (dflow.PublishedNode[R], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, ok := p.records[id]
	if !ok {
		return nil, dflow.ErrInvalidDefinition
	}
	return record, nil
}

func (p *Publication[R]) IDs() []string { return slices.Clone(p.order) }

type Infrastructure struct {
	db    *sql.DB
	scope string
}

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
	_, err = db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS dflow_nodes (
 scope TEXT NOT NULL,
 node TEXT NOT NULL,
 state BLOB NOT NULL,
 incoming BLOB NOT NULL,
 ready INTEGER NOT NULL,
 status TEXT NOT NULL,
 generation INTEGER NOT NULL,
 lease_expires INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(scope,node)
);
CREATE INDEX IF NOT EXISTS dflow_nodes_ready ON dflow_nodes(scope,ready,status,lease_expires);
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Infrastructure{db: db, scope: scope}, nil
}

func NewRun[R any](ctx context.Context, path, scope string, records []dflow.PublishedNode[R], root string) (*Infrastructure, *Publication[R], error) {
	publication, err := Publish(records)
	if err != nil {
		return nil, nil, err
	}
	infra, err := Open(ctx, path, scope)
	if err != nil {
		return nil, nil, err
	}
	if err := infra.Initialize(ctx, publication, root); err != nil {
		infra.Close()
		return nil, nil, err
	}
	return infra, publication, nil
}

func (i *Infrastructure) Close() error { return i.db.Close() }

func (i *Infrastructure) Initialize[R any](ctx context.Context, publication *Publication[R], root string) error {
	if publication == nil || root == "" {
		return dflow.ErrInvalidDefinition
	}
	return i.transaction(ctx, func(tx *sql.Tx) error {
		for _, id := range publication.order {
			record := publication.records[id]
			state := dflow.NodeState{Status: dflow.Pending}
			if id == root {
				state.Version, state.Ready, state.Enabled = 1, true, true
			}
			if err := putNode(ctx, tx, i.scope, id, state, record.GetIncoming(), 0); err != nil {
				return err
			}
		}
		return nil
	})
}

func (i *Infrastructure) ReadyNodes(ctx context.Context) ([]string, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT node FROM dflow_nodes WHERE scope=?
AND (ready=1 OR status IN ('running','failed')) AND lease_expires<=? ORDER BY node`, i.scope, time.Now().UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ready []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ready = append(ready, id)
	}
	return ready, rows.Err()
}

func (i *Infrastructure) ClaimNode(ctx context.Context, id string, ttl time.Duration) (task *dflow.NodeTask, err error) {
	if id == "" {
		return nil, dflow.ErrInvalidDefinition
	}
	if ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	err = i.transaction(ctx, func(tx *sql.Tx) error {
		state, incoming, expires, err := getNode(ctx, tx, i.scope, id)
		if err != nil || state == nil {
			return err
		}
		if time.Now().UnixNano() < expires || !claimable(*state) {
			return nil
		}
		state.Version++
		state.Generation++
		state.Status = dflow.Running
		state.Ready = false
		state.Error = ""
		token := lease.Token{Key: id, Generation: state.Generation, ExpiresAt: time.Now().Add(ttl)}
		if err := putNode(ctx, tx, i.scope, id, *state, incoming, token.ExpiresAt.UnixNano()); err != nil {
			return err
		}
		task = &dflow.NodeTask{NodeID: id, State: *state, Lease: token}
		return nil
	})
	return task, err
}

func (i *Infrastructure) RenewNode(ctx context.Context, token lease.Token, ttl time.Duration) (*lease.Token, error) {
	if ttl <= 0 {
		return nil, lease.ErrInvalid
	}
	now := time.Now()
	token.ExpiresAt = now.Add(ttl)
	result, err := i.db.ExecContext(ctx, `UPDATE dflow_nodes SET lease_expires=?
WHERE scope=? AND node=? AND generation=? AND lease_expires>?`,
		token.ExpiresAt.UnixNano(), i.scope, token.Key, token.Generation, now.UnixNano())
	if err := affected(result, err); err != nil {
		return nil, err
	}
	return &token, nil
}

func (i *Infrastructure) ReleaseNode(ctx context.Context, token lease.Token) error {
	result, err := i.db.ExecContext(ctx, `UPDATE dflow_nodes SET lease_expires=0
WHERE scope=? AND node=? AND generation=? AND lease_expires<>0`, i.scope, token.Key, token.Generation)
	return affected(result, err)
}

func (i *Infrastructure) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) (ready []string, err error) {
	err = i.transaction(ctx, func(tx *sql.Tx) error {
		ready = nil
		if token.Key != commit.NodeID {
			return lease.ErrConflict
		}
		current, incoming, expires, err := getNode(ctx, tx, i.scope, commit.NodeID)
		if err != nil {
			return err
		}
		if current == nil || current.Generation != token.Generation || expires <= time.Now().UnixNano() {
			return lease.ErrConflict
		}
		if err := dflow.ValidateCommit(*current, commit, token.Generation); err != nil {
			return err
		}
		if err := putNode(ctx, tx, i.scope, commit.NodeID, dflow.ApplyCommitResult(*current, commit), incoming, 0); err != nil {
			return err
		}
		for targetID, active := range commit.Arrivals {
			before, targetIncoming, targetExpires, err := getNode(ctx, tx, i.scope, targetID)
			if err != nil {
				return err
			}
			if before == nil {
				return dflow.ErrInvalidDefinition
			}
			after, err := dflow.ApplyArrival(*before, commit.NodeID, targetIncoming, active)
			if err != nil {
				return err
			}
			if err := putNode(ctx, tx, i.scope, targetID, after, targetIncoming, targetExpires); err != nil {
				return err
			}
			if !before.Ready && after.Ready {
				ready = append(ready, targetID)
			}
		}
		slices.Sort(ready)
		return nil
	})
	return ready, err
}

func (i *Infrastructure) Node(ctx context.Context, id string) (dflow.NodeState, bool, error) {
	state, _, _, err := getNode(ctx, i.db, i.scope, id)
	if err != nil || state == nil {
		return dflow.NodeState{}, false, err
	}
	return *state, true, nil
}

func (i *Infrastructure) Snapshot(ctx context.Context) (map[string]dflow.NodeState, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT node,state FROM dflow_nodes WHERE scope=? ORDER BY node`, i.scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]dflow.NodeState{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var state dflow.NodeState
		if err := decode(raw, &state); err != nil {
			return nil, err
		}
		result[id] = state
	}
	return result, rows.Err()
}

func (i *Infrastructure) Output(ctx context.Context, id string) (map[string]any, error) {
	state, ok, err := i.Node(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok || state.Status != dflow.Succeeded {
		return nil, dflow.ErrInvalidReference
	}
	return maps.Clone(state.Output), nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getNode(ctx context.Context, q queryer, scope, id string) (*dflow.NodeState, []string, int64, error) {
	var rawState, rawIncoming []byte
	var expires int64
	err := q.QueryRowContext(ctx, `SELECT state,incoming,lease_expires FROM dflow_nodes WHERE scope=? AND node=?`, scope, id).
		Scan(&rawState, &rawIncoming, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, 0, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	var state dflow.NodeState
	var incoming []string
	if err := decode(rawState, &state); err != nil {
		return nil, nil, 0, err
	}
	if err := decode(rawIncoming, &incoming); err != nil {
		return nil, nil, 0, err
	}
	return &state, incoming, expires, nil
}

func putNode(ctx context.Context, tx *sql.Tx, scope, id string, state dflow.NodeState, incoming []string, expires int64) error {
	rawState, err := encode(state)
	if err != nil {
		return err
	}
	rawIncoming, err := encode(incoming)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dflow_nodes(scope,node,state,incoming,ready,status,generation,lease_expires)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(scope,node) DO UPDATE SET
 state=excluded.state,incoming=excluded.incoming,ready=excluded.ready,status=excluded.status,
 generation=excluded.generation,lease_expires=excluded.lease_expires`,
		scope, id, rawState, rawIncoming, boolInt(state.Ready), string(state.Status), state.Generation, expires)
	return err
}

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

func affected(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return lease.ErrConflict
	}
	return nil
}

func claimable(state dflow.NodeState) bool {
	switch state.Status {
	case dflow.Pending:
		return state.Ready
	case dflow.Running, dflow.Failed, dflow.Suspended:
		return true
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func encode(value any) ([]byte, error) {
	var buffer bytes.Buffer
	err := gob.NewEncoder(&buffer).Encode(value)
	return buffer.Bytes(), err
}

func decode(raw []byte, target any) error {
	return gob.NewDecoder(bytes.NewReader(raw)).Decode(target)
}
