// Package dflow executes declarative directed acyclic workflows.
package dflow

import (
	"context"
)

// Node receives resolved input. Implementations must honor ctx and treat input
// values as read-only. Configuration is immutable; nodes may run concurrently.
type Node interface {
	Run(context.Context, map[string]any) (Result, error)
}

// NodeValidator optionally validates configuration without executing the node.
type NodeValidator interface {
	Validate() error
}

// ResumableNode handles a persisted suspension's response. A recovered suspended
// node calls Resume, never Run. Resume may finish or return another suspension.
// Both methods may be retried after interruption; side effects need idempotency.
type ResumableNode interface {
	Node
	Resume(context.Context, map[string]any, ResumeInput) (Result, error)
}

type ResumeInput struct {
	Token      string
	Suspension Suspension
	Response   any
}

type NodeFunc func(context.Context, map[string]any) (Result, error)

func (f NodeFunc) Run(ctx context.Context, input map[string]any) (Result, error) {
	return f(ctx, input)
}

// Result values must be supported by the application's Infrastructure.
// Ports selects outgoing source ports:
// nil activates every edge; an empty non-nil slice activates none.
type Result struct {
	Output any      `json:"output"`
	Ports  []string `json:"ports"`
	// Suspend is mutually exclusive with Output and Ports.
	Suspend Suspension `json:"-"`
}

// NodeID returns the current node's stable ID, useful for business idempotency.
// The business must combine it with its own run/tenant scope.
func NodeID(ctx context.Context) string {
	id, _ := ctx.Value(nodeIDKey{}).(string)
	return id
}

type nodeIDKey struct{}
