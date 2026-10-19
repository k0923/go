package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/lease"
)

func (i *Infrastructure) GetInput(ctx context.Context, path ...string) (any, error) {
	state, err := i.LoadMetadata(ctx)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, dflow.ErrMissingValue
	}
	return dflow.InputValue(state.Input, path...)
}
func (i *Infrastructure) LoadMetadata(ctx context.Context) (*dflow.State, error) {
	var raw []byte
	err := i.db.QueryRowContext(ctx, `SELECT state FROM dflow_runs WHERE scope=?`, i.scope).Scan(&raw)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var state dflow.State
	if err := i.codec.Decode(raw, &state); err != nil {
		return nil, err
	}
	if state.Nodes != nil {
		return nil, dflow.ErrRunMismatch
	} // old snapshot format needs explicit migration
	return &state, nil
}
func (i *Infrastructure) Initialize(ctx context.Context, m dflow.Manifest, input map[string]any) error {
	return i.transaction(ctx, func(tx *sql.Tx) error {
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT state FROM dflow_runs WHERE scope=?`, i.scope).Scan(&raw); err != nil {
			return err
		}
		if raw != nil {
			var previous dflow.State
			if err := i.codec.Decode(raw, &previous); err != nil {
				return err
			}
			if previous.Nodes != nil || previous.DefinitionHash != m.Hash || !reflect.DeepEqual(previous.Input, input) {
				return dflow.ErrRunMismatch
			}
			return nil
		}
		raw, err := i.codec.Encode(dflow.State{Version: 1, DefinitionHash: m.Hash, Input: input})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dflow_runs SET state=?,version=1 WHERE scope=?`, raw, i.scope); err != nil {
			return err
		}
		if m.Start == "" {
			return dflow.ErrInvalidDefinition
		}
		if err := i.putNode(ctx, tx, m.Start, dflow.NodeState{Version: 1, Status: dflow.Pending, Ready: true, Enabled: true}); err != nil {
			return err
		}
		return nil
	})
}
func (i *Infrastructure) getNode(ctx context.Context, q reader, id string) (*dflow.NodeState, error) {
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT state FROM dflow_nodes WHERE scope=? AND node=?`, i.scope, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var node dflow.NodeState
	if err := i.codec.Decode(raw, &node); err != nil {
		return nil, err
	}
	return &node, nil
}
func (i *Infrastructure) GetNode(ctx context.Context, id string) (*dflow.NodeState, error) {
	return i.getNode(ctx, i.db, id)
}
func (i *Infrastructure) putNode(ctx context.Context, tx *sql.Tx, id string, node dflow.NodeState) error {
	raw, err := i.codec.Encode(node)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dflow_nodes(scope,node,state,ready,status,generation) VALUES(?,?,?,?,?,?)
ON CONFLICT(scope,node) DO UPDATE SET state=excluded.state,ready=excluded.ready,status=excluded.status,generation=excluded.generation`,
		i.scope, id, raw, node.Ready, string(node.Status), node.Generation)
	return err
}

// ReadyNodes lists drivable node IDs not owned by a live lease: freshly ready
// nodes, nodes left Running by a lost attempt, and Failed nodes that a new run
// may retry. The driver stops the current sweep on the first failure
// (fail-fast); retrying is a deliberate new Run/Drain by the business.
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
		now := time.Now()
		var found string
		err := tx.QueryRowContext(ctx, `SELECT node FROM dflow_nodes WHERE scope=? AND node=?
AND (ready=1 OR status IN ('running','failed')) AND lease_expires<=?`, i.scope, id, now.UnixNano()).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		node, err := i.getNode(ctx, tx, found)
		if err != nil {
			return err
		}
		node.Status, node.Ready, node.Error = dflow.Running, false, ""
		node.Generation++
		node.Version++
		if err := i.putNode(ctx, tx, found, *node); err != nil {
			return err
		}
		expires := now.Add(ttl)
		if err := i.setLease(ctx, tx, found, expires.UnixNano()); err != nil {
			return err
		}
		task = &dflow.NodeTask{NodeID: found, State: *node, Lease: lease.Token{Key: found, Generation: node.Generation, ExpiresAt: expires}}
		return nil
	})
	return task, err
}
func (i *Infrastructure) setLease(ctx context.Context, tx *sql.Tx, id string, expires int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE dflow_nodes SET lease_expires=? WHERE scope=? AND node=?`, expires, i.scope, id)
	return err
}
func (i *Infrastructure) Commit(ctx context.Context, token lease.Token, c dflow.NodeCommit) (ready []string, err error) {
	err = i.transaction(ctx, func(tx *sql.Tx) error {
		ready = nil
		if token.Key != c.NodeID {
			return lease.ErrConflict
		}
		current, err := i.getNode(ctx, tx, c.NodeID)
		if err != nil {
			return err
		}
		if current == nil || current.Generation != token.Generation {
			return lease.ErrConflict // fenced: a newer attempt claimed the node
		}
		if err := dflow.ValidateCommit(*current, c, token.Generation); err != nil {
			return err
		}
		if err := i.checkNodeLease(ctx, tx, token); err != nil {
			return err // lease expired or released
		}
		next := c.State
		next.Version = current.Version + 1
		next.Incoming = current.Incoming
		next.Enabled = current.Enabled
		next.Generation = current.Generation
		next.Ready = false
		if err := i.putNode(ctx, tx, c.NodeID, next); err != nil {
			return err
		}
		// The outcome ends this attempt's ownership.
		if err := i.setLease(ctx, tx, c.NodeID, 0); err != nil {
			return err
		}
		for _, arrival := range c.Arrivals {
			node, err := i.getNode(ctx, tx, arrival.TargetNodeID)
			if err != nil {
				return err
			}
			var old dflow.NodeState
			if node != nil {
				old = *node
			}
			target, err := dflow.ApplyArrival(old, c.NodeID, arrival)
			if err != nil {
				return err
			}
			if err := i.putNode(ctx, tx, arrival.TargetNodeID, target); err != nil {
				return err
			}
			if target.Ready && !old.Ready {
				ready = append(ready, arrival.TargetNodeID)
			}
		}
		if next.Status == dflow.Suspended {
			result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dflow_waits(scope,node,token) VALUES(?,?,?)`, i.scope, c.NodeID, next.Suspension.Token)
			if err != nil {
				return err
			}
			inserted, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if inserted != 0 {
				if err := i.notify(ctx, tx, dflow.Notification{Kind: dflow.RequestNotification, NodeID: c.NodeID, Token: next.Suspension.Token, Request: next.Suspension.Payload}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return ready, err
}
