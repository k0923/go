package tflow

import (
	"context"
)

// ExecutorFuncs adapts individual operations for asynchronous integrations.
// Missing Resume/Cancel return explicit errors, retaining the delivery for retry.
type ExecutorFuncs struct {
	StartFunc  func(context.Context, SubTask, Execution) (DispatchResult, error)
	ResumeFunc func(context.Context, SubTask, Execution, Event) (DispatchResult, error)
	CancelFunc func(context.Context, SubTask, Execution) error
}

func (f ExecutorFuncs) Start(ctx context.Context, task SubTask, ref Execution) (DispatchResult, error) {
	if f.StartFunc == nil {
		return DispatchResult{}, ErrExecutorUnavailable
	}
	return f.StartFunc(ctx, task, ref)
}

func (f ExecutorFuncs) Resume(ctx context.Context, task SubTask, ref Execution, event Event) (DispatchResult, error) {
	if f.ResumeFunc == nil {
		return DispatchResult{}, ErrResumeUnsupported
	}
	return f.ResumeFunc(ctx, task, ref, event)
}

func (f ExecutorFuncs) Cancel(ctx context.Context, task SubTask, ref Execution) error {
	if f.CancelFunc == nil {
		return ErrCancelUnsupported
	}
	return f.CancelFunc(ctx, task, ref)
}
