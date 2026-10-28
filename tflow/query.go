package tflow

import (
	"context"
	"fmt"
)

func (e *Engine) GetMain(ctx context.Context) (MainProgress, error) {
	record, err := e.store.GetMain(ctx)
	return record.MainProgress, err
}

func (e *Engine) GetSub(ctx context.Context, key string) (SubRecord, error) {
	return e.store.GetSub(ctx, key)
}

// ListSubTasks reads matching subtasks without opening a write transaction.
func (e *Engine) ListSubTasks(ctx context.Context, query SubQuery) ([]SubRecord, error) {
	if query.Limit < 0 {
		return nil, fmt.Errorf("%w: query limit must be nonnegative", ErrInvalid)
	}
	return e.store.FindSubs(ctx, query)
}
