// Package executor contains example-only adapters for tflow.Executor.
package executor

import (
	"context"
	"fmt"

	"github.com/k0923/go/tflow"
)

// Func adapts a synchronous function for examples. It must return a terminal
// result; use tflow.ExecutorFuncs or a full tflow.Executor for async handoff.
type Func func(context.Context, tflow.SubTask, tflow.Execution) (tflow.DispatchResult, error)

func (f Func) Start(ctx context.Context, task tflow.SubTask, ref tflow.Execution) (tflow.DispatchResult, error) {
	if f == nil {
		return tflow.DispatchResult{}, tflow.ErrExecutorUnavailable
	}
	result, err := f(ctx, task, ref)
	if err == nil && (result.Status == tflow.Accepted || result.Status == tflow.Waiting) {
		return tflow.DispatchResult{}, fmt.Errorf("%w: executor.Func must return a terminal result", tflow.ErrInvalid)
	}
	return result, err
}

func (Func) Resume(context.Context, tflow.SubTask, tflow.Execution, tflow.Event) (tflow.DispatchResult, error) {
	return tflow.DispatchResult{}, tflow.ErrResumeUnsupported
}

func (Func) Cancel(context.Context, tflow.SubTask, tflow.Execution) error { return nil }
