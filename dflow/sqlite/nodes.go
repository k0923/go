package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"reflect"

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
func (i *Infrastructure) Initialize(ctx context.Context, token lease.Token, m dflow.Manifest, input map[string]any) error {
	return i.transaction(ctx, func(tx *sql.Tx) error {
		if err := i.checkLease(ctx, tx, token); err != nil {
			return err
		}
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
		for _, id := range m.Roots {
			if err := i.putNode(ctx, tx, id, dflow.NodeState{Version: 1, Status: dflow.Pending, Ready: true, Enabled: true}); err != nil {
				return err
			}
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
func (i *Infrastructure) ClaimReady(ctx context.Context, token lease.Token) (task *dflow.NodeTask, err error) {
	return i.claimNode(ctx, token, "")
}
func (i *Infrastructure) ClaimNode(ctx context.Context, token lease.Token, id string) (*dflow.NodeTask, error) {
	if id == "" {
		return nil, dflow.ErrInvalidDefinition
	}
	return i.claimNode(ctx, token, id)
}
func (i *Infrastructure) claimNode(ctx context.Context, token lease.Token, only string) (task *dflow.NodeTask, err error) {
	err = i.transaction(ctx, func(tx *sql.Tx) error {
		if err := i.checkLease(ctx, tx, token); err != nil {
			return err
		}
		var id string
		err := tx.QueryRowContext(ctx, `SELECT node FROM dflow_nodes WHERE scope=?
AND (ready=1 OR (status IN ('running','failed') AND generation<>?))
AND NOT (status='running' AND generation=?) AND (?='' OR node=?) ORDER BY node LIMIT 1`, i.scope, token.Generation, token.Generation, only, only).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		node, err := i.getNode(ctx, tx, id)
		if err != nil {
			return err
		}
		node.Status, node.Ready, node.Generation, node.Error = dflow.Running, false, token.Generation, ""
		node.Version++
		if err := i.putNode(ctx, tx, id, *node); err != nil {
			return err
		}
		task = &dflow.NodeTask{NodeID: id, State: *node}
		return nil
	})
	return task, err
}
func (i *Infrastructure) Commit(ctx context.Context, token lease.Token, c dflow.NodeCommit) error {
	return i.transaction(ctx, func(tx *sql.Tx) error {
		if err := i.checkLease(ctx, tx, token); err != nil {
			return err
		}
		current, err := i.getNode(ctx, tx, c.NodeID)
		if err != nil {
			return err
		}
		if current == nil {
			return dflow.ErrConflict
		}
		if err := dflow.ValidateCommit(*current, c, token.Generation); err != nil {
			return err
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
}
