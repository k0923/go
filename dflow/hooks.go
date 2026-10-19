package dflow

import (
	"context"
	"time"
)

// Hooks observes node lifecycle transitions for metrics, tracing and logging.
// Every field is optional; a nil hook is skipped. Hooks must not block for long,
// must treat all arguments as read-only, and must never panic: a panicking hook
// is recovered and ignored so observation can never corrupt execution.
//
// Hooks are a side channel. They never influence scheduling, commit or retry
// decisions, and the engine ignores any error a hook might imply.
type Hooks struct {
	// OnClaim fires after a node is claimed and marked Running, before input
	// resolution. Attempt carries the driving lease generation, which advances
	// on each recovery, so a rising Attempt for one node indicates retries.
	OnClaim func(context.Context, NodeEvent)
	// OnStart fires immediately before Run or Resume is invoked. Resume reports
	// IsResume true and carries the suspension token.
	OnStart func(context.Context, NodeEvent)
	// OnFinish fires after the node returns, before Commit. Outcome classifies
	// the result; Err is set for Failed. Duration measures Run/Resume only.
	OnFinish func(context.Context, NodeEvent)
}

// NodeEvent carries read-only node lifecycle data. Fields that do not apply to a
// given hook are zero (for example Duration/Outcome are unset before OnFinish).
type NodeEvent struct {
	NodeID   string
	Attempt  int64
	IsResume bool
	Token    string
	Duration time.Duration
	Outcome  Status
	Err      error
}

func (h Hooks) claim(ctx context.Context, event NodeEvent) { safeHook(ctx, h.OnClaim, event) }
func (h Hooks) start(ctx context.Context, event NodeEvent) { safeHook(ctx, h.OnStart, event) }
func (h Hooks) finish(ctx context.Context, event NodeEvent) {
	safeHook(ctx, h.OnFinish, event)
}

func safeHook(ctx context.Context, fn func(context.Context, NodeEvent), event NodeEvent) {
	if fn == nil {
		return
	}
	defer func() { _ = recover() }()
	fn(ctx, event)
}
