package debugger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/k0923/go/cflow"
	"github.com/k0923/go/cflow/memory"
)

var _ cflow.FlowInfrastructure = (*Client)(nil)

type startLoopRequest struct {
	Spec  cflow.LoopSpec
	Items []cflow.FlowExecution
}
type commitFlowRequest struct {
	Lease    cflow.Lease
	Item     *cflow.FlowExecution
	Expected int64
}

func (c *Client) GetLoop(ctx context.Context, key string) (out *cflow.LoopState, err error) {
	err = c.infra(ctx, "get_loop", keyRequest{key}, &out)
	return
}
func (c *Client) StartLoop(ctx context.Context, spec cflow.LoopSpec, items []cflow.FlowExecution) (out *cflow.LoopState, err error) {
	err = c.infra(ctx, "start_loop", startLoopRequest{spec, items}, &out)
	return
}
func (c *Client) GetFlow(ctx context.Context, key string) (out *cflow.FlowExecution, err error) {
	err = c.infra(ctx, "get_flow", keyRequest{key}, &out)
	return
}
func (c *Client) StartFlow(ctx context.Context, item cflow.FlowExecution) (out *cflow.FlowExecution, err error) {
	err = c.infra(ctx, "start_flow", item, &out)
	return
}
func (c *Client) CommitFlow(ctx context.Context, lease cflow.Lease, item *cflow.FlowExecution, expected int64) (out *cflow.FlowExecution, err error) {
	err = c.infra(ctx, "commit_flow", commitFlowRequest{lease, item, expected}, &out)
	return
}
func (c *Client) LoopResults(ctx context.Context, key string) (out []json.RawMessage, err error) {
	err = c.infra(ctx, "loop_results", keyRequest{key}, &out)
	return
}

func (t *txn) loop(key string) (*cflow.LoopState, error) {
	var raw []byte
	err := t.tx.QueryRow(`SELECT data FROM loops WHERE run_id = ? AND key = ?`, t.run, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out cflow.LoopState
	err = json.Unmarshal(raw, &out)
	return &out, err
}

func (t *txn) flow(key string) (*cflow.FlowExecution, error) {
	var raw []byte
	err := t.tx.QueryRow(`SELECT data FROM flow_executions WHERE run_id = ? AND key = ?`, t.run, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out cflow.FlowExecution
	err = json.Unmarshal(raw, &out)
	return &out, err
}

func (t *txn) saveLoop(loop *cflow.LoopState) error {
	raw, err := json.Marshal(loop)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT OR REPLACE INTO loops (run_id, key, parent_key, data) VALUES (?, ?, ?, ?)`, t.run, loop.Key, loop.ParentKey, raw)
	return err
}

func (t *txn) saveFlow(item *cflow.FlowExecution) error {
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	loopKey := item.LoopKey
	if loopKey == "" {
		loopKey = item.Key
	}
	_, err = t.exec(`INSERT OR REPLACE INTO flow_executions (run_id, key, loop_key, parent_key, item_index, data) VALUES (?, ?, ?, ?, ?, ?)`,
		t.run, item.Key, loopKey, item.ParentKey, item.Index, raw)
	return err
}

func (t *txn) saveOwner(key, owner string) error {
	_, err := t.exec(`INSERT OR IGNORE INTO execution_owners (run_id, key, execution_key) VALUES (?, ?, ?)`, t.run, key, owner)
	if err != nil {
		return err
	}
	var existing string
	if err = t.tx.QueryRow(`SELECT execution_key FROM execution_owners WHERE run_id = ? AND key = ?`, t.run, key).Scan(&existing); err != nil {
		return err
	}
	if existing != owner {
		return cflow.ErrChanged
	}
	return nil
}

func (t *txn) executionKeyExists(key string) (bool, error) {
	var exists bool
	err := t.tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM loops WHERE run_id=? AND key=? UNION ALL
		SELECT 1 FROM flow_executions WHERE run_id=? AND key=?)`, t.run, key, t.run, key).Scan(&exists)
	return exists, err
}

func (s *Store) GetLoop(ctx context.Context, run, key string) (out *cflow.LoopState, err error) {
	err = s.tx(ctx, run, func(t *txn) error { out, err = t.loop(key); return err })
	return
}
func (s *Store) GetFlow(ctx context.Context, run, key string) (out *cflow.FlowExecution, err error) {
	err = s.tx(ctx, run, func(t *txn) error { out, err = t.flow(key); return err })
	return
}

func (s *Store) StartFlow(ctx context.Context, run string, spec cflow.FlowExecution) (out *cflow.FlowExecution, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		if err := cflow.ValidateFlow(spec); err != nil {
			return err
		}
		var err error
		out, err = t.flow(spec.Key)
		if err != nil {
			return err
		}
		if out != nil {
			if !cflow.SameFlow(spec, *out) {
				return cflow.ErrChanged
			}
			return nil
		}
		var occupied bool
		if err := t.tx.QueryRow(`SELECT EXISTS(
			SELECT 1 FROM records WHERE run_id=? AND key=? UNION ALL
			SELECT 1 FROM loops WHERE run_id=? AND key=? UNION ALL
			SELECT 1 FROM wait_groups WHERE run_id=? AND key=?)`,
			run, spec.Key, run, spec.Key, run, spec.Key).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return cflow.ErrChanged
		}
		spec.Status, spec.Version, spec.Output, spec.Failure = cflow.Running, 1, nil, nil
		if err := t.saveFlow(&spec); err != nil {
			return err
		}
		if err := t.saveOwner(spec.Key, spec.Key); err != nil {
			return err
		}
		if err := t.enqueueID(cflow.FlowWakeID(spec.Key, 1), spec.Key, memory.WakeNotification, t.now); err != nil {
			return err
		}
		out = &spec
		return nil
	})
	return
}

func (s *Store) StartLoop(ctx context.Context, run string, spec cflow.LoopSpec, items []cflow.FlowExecution) (out *cflow.LoopState, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		if err := cflow.ValidateLoopBatch(spec, items); err != nil {
			return err
		}
		var err error
		out, err = t.loop(spec.Key)
		if err != nil {
			return err
		}
		if out != nil {
			if !cflow.SameLoop(spec, out.LoopSpec) {
				return cflow.ErrChanged
			}
			return nil
		}
		keys := []string{spec.Key}
		for _, item := range items {
			keys = append(keys, item.Key)
		}
		for _, key := range keys {
			var exists bool
			if err := t.tx.QueryRow(`SELECT EXISTS(
				SELECT 1 FROM records WHERE run_id=? AND key=? UNION ALL
				SELECT 1 FROM wait_groups WHERE run_id=? AND key=? UNION ALL
				SELECT 1 FROM loops WHERE run_id=? AND key=? UNION ALL
				SELECT 1 FROM flow_executions WHERE run_id=? AND key=?)`,
				run, key, run, key, run, key, run, key).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return cflow.ErrChanged
			}
		}
		out = cflow.NewLoopState(spec)
		if err := t.saveLoop(out); err != nil {
			return err
		}
		if err := t.saveOwner(spec.Key, spec.ParentKey); err != nil {
			return err
		}
		for i, item := range items {
			item.Status, item.Version, item.Output, item.Failure = cflow.Queued, 1, nil, nil
			if i < out.AdmittedCount {
				item.Status = cflow.Running
				if err := t.enqueueID(cflow.FlowWakeID(item.Key, 1), item.Key, memory.WakeNotification, t.now); err != nil {
					return err
				}
			}
			if err := t.saveFlow(&item); err != nil {
				return err
			}
			if err := t.saveOwner(item.Key, item.Key); err != nil {
				return err
			}
		}
		t.log("store.loop_started", spec.Key, map[string]any{"total": spec.Total, "concurrency": out.AdmittedCount})
		return nil
	})
	return
}

func (s *Store) CommitFlow(ctx context.Context, run string, token cflow.Lease, result *cflow.FlowExecution, expected int64) (out *cflow.FlowExecution, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		if result == nil || token.Key != result.Key {
			return cflow.ErrInvalid
		}
		valid, err := t.validLease(token)
		if err != nil {
			return err
		}
		if !valid {
			return cflow.ErrConflict
		}
		out, err = t.flow(result.Key)
		if err != nil {
			return err
		}
		if out == nil {
			return cflow.ErrNotFound
		}
		if out.Status == cflow.Completed || out.Status == cflow.Failed {
			return nil
		}
		loop, err := t.loop(out.LoopKey)
		if err != nil {
			return err
		}
		next, err := cflow.ApplyFlowCompletion(loop, out, result, expected)
		if err != nil {
			return err
		}
		if err := t.saveFlow(out); err != nil {
			return err
		}
		if out.LoopKey == "" {
			wakeKey := out.ParentKey
			// Every child has its own completion notification, even when
			// siblings finish at the same version and share the same parent.
			return t.enqueueID(cflow.FlowCompletionWakeID(out.Key, out.Version), wakeKey, memory.WakeNotification, t.now)
		}
		if err := t.saveLoop(loop); err != nil {
			return err
		}
		if next >= 0 {
			var nextKey string
			if err := t.tx.QueryRow(`SELECT key FROM flow_executions WHERE run_id=? AND loop_key=? AND item_index=?`,
				run, loop.Key, next).Scan(&nextKey); err != nil {
				return err
			}
			item, err := t.flow(nextKey)
			if err != nil {
				return err
			}
			item.Status, item.Version = cflow.Running, item.Version+1
			if err := t.saveFlow(item); err != nil {
				return err
			}
			if err := t.enqueueID(cflow.FlowWakeID(item.Key, item.Version), item.Key, memory.WakeNotification, t.now); err != nil {
				return err
			}
		}
		t.log("store.flow_finished", out.Key, map[string]any{"status": out.Status, "completed": loop.CompletedCount,
			"running": loop.RunningCount, "total": loop.Total})
		if loop.Status == cflow.Suspended {
			return nil
		}
		return t.enqueueID(cflow.FlowWakeID(loop.Key, loop.Version), loop.Key, memory.WakeNotification, t.now)
	})
	return
}

func (s *Store) LoopResults(ctx context.Context, run, key string) (out []json.RawMessage, err error) {
	err = s.tx(ctx, run, func(t *txn) error {
		loop, err := t.loop(key)
		if err != nil {
			return err
		}
		if loop == nil {
			return cflow.ErrNotFound
		}
		if loop.Status != cflow.Completed && loop.Status != cflow.Failed {
			return cflow.ErrConflict
		}
		out, err = t.loopOutputs(loop)
		return err
	})
	return
}

// loopOutputs reads every output of a completed loop in index order; failed
// and DiscardOutputs loops have none.
// loopOutputs 按下标读取成功循环的全部输出；失败或 DiscardOutputs 循环没有输出。
func (t *txn) loopOutputs(loop *cflow.LoopState) ([]json.RawMessage, error) {
	if loop.Status != cflow.Completed || loop.DiscardOutputs {
		return nil, nil
	}
	rows, err := t.tx.Query(`SELECT data FROM flow_executions WHERE run_id=? AND loop_key=? AND key != loop_key
		ORDER BY item_index`, t.run, loop.Key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]json.RawMessage, 0, loop.Total)
	for rows.Next() {
		var raw []byte
		var item cflow.FlowExecution
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		out = append(out, item.Output)
	}
	return out, rows.Err()
}
