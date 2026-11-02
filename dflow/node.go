// Package dflow executes persistent DAGs one node at a time.
package dflow

import "context"

// NodeSpec is a complete build-time definition. V supplies business validation
// capabilities; R supplies execution capabilities. Build returns a business
// implementation of PublishedNode with topology matching Definition.GetEdges and
// all declared dependencies. Build must not execute work. Specs and publications
// must be immutable and safe to reuse.
type NodeSpec[V any, R any] interface {
	GetID() string
	Validate(context.Context, V) error
	Build(context.Context, V) (PublishedNode[R], error)
}

type NoContext struct{}

// RuntimeNode contains only execution configuration, never a complete NodeSpec.
// Every invocation, including asynchronous continuation, uses Run. Side effects
// must be idempotent: a lost lease may cause the invocation to be retried.
type RuntimeNode[R any] interface {
	Run(context.Context, R) NodeResult
}

// NodeResult preserves object-shaped input/output for persistence. Custom
// implementations may add business methods. Engine records only this contract
// and returns the original implementation in Executed.Result after commit.
// Results and their getters must be read-only and safe to read repeatedly.
// The engine captures standard fields once; callers may inspect the original
// result again. A panicking getter fails the node and discards the raw result.
type NodeResult interface {
	GetInput() map[string]any
	GetOutput() map[string]any
	GetError() error
}

type AsyncNodeResult interface {
	NodeResult
	IsDone() bool
}

// GetOutgoing selects target node IDs, not port names. Nil and empty both
// select no targets. Duplicate IDs and IDs outside the topology are invalid.
type BranchNodeResult interface {
	NodeResult
	GetOutgoing() []string
}

type Result struct {
	Input  map[string]any
	Output map[string]any
	Err    error
}

func (r Result) GetInput() map[string]any  { return r.Input }
func (r Result) GetOutput() map[string]any { return r.Output }
func (r Result) GetError() error           { return r.Err }

type AsyncResult struct {
	Result
	Done bool
}

func (r AsyncResult) IsDone() bool { return r.Done }

type BranchResult struct {
	Result
	Outgoing []string
}

func (r BranchResult) GetOutgoing() []string { return r.Outgoing }

func ResultWithError(err error) NodeResult { return Result{Err: err} }

// NodeFunc adapts a function to RuntimeNode. Its result determines whether this
// invocation is synchronous, asynchronous or branching.
type NodeFunc[R any] func(context.Context, R) NodeResult

func (f NodeFunc[R]) Run(ctx context.Context, runtime R) NodeResult {
	return f(ctx, runtime)
}

// NodeID identifies the executing node. Combine it with business execution
// scope when constructing an idempotency key.
func NodeID(ctx context.Context) string {
	id, _ := ctx.Value(nodeIDKey{}).(string)
	return id
}

type nodeIDKey struct{}
