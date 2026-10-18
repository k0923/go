// Package dflow executes declarative directed acyclic workflows.
package dflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Node receives resolved, JSON-normalized input. Implementations must honor ctx.
// Dependencies such as service clients belong in the factory's closure.
type Node interface {
	Run(context.Context, map[string]any) (Result, error)
}

// ResumableNode handles a persisted suspension's response. A recovered suspended
// node calls Resume, never Run. Resume may finish or return another suspension.
// Both methods may be retried after interruption; side effects need idempotency.
type ResumableNode interface {
	Node
	Resume(context.Context, map[string]any, ResumeInput) (Result, error)
}

// Suspension describes the continuation data and external request to persist.
// Request is delivered via the outbox AFTER the suspension is committed.
type Suspension struct {
	State   any `json:"state"`
	Request any `json:"request"`
}

type ResumeInput struct {
	Token    string
	State    any
	Response any
}

type NodeFunc func(context.Context, map[string]any) (Result, error)

func (f NodeFunc) Run(ctx context.Context, input map[string]any) (Result, error) {
	return f(ctx, input)
}

// Result.Output must be JSON-serializable. Ports selects outgoing source ports:
// nil activates every edge; an empty non-nil slice activates none.
type Result struct {
	Output any      `json:"output"`
	Ports  []string `json:"ports"`
	// Suspend is mutually exclusive with Output and Ports.
	Suspend *Suspension `json:"suspend,omitempty"`
}

// NodeFactory validates configuration and creates an immutable/shareable node.
// It must not execute user code or perform business side effects.
type NodeFactory func(json.RawMessage) (Node, error)

// Registry is instance-scoped. Register deliberately replaces an existing type.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]NodeFactory
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]NodeFactory)}
}

func DefaultRegistry() *Registry {
	r := NewRegistry()
	_ = r.Register("start", passFactory)
	_ = r.Register("end", passFactory)
	_ = r.Register("passthrough", passFactory)
	_ = r.Register("json-parse", jsonParseFactory)
	_ = r.Register("condition", conditionFactory)
	_ = r.Register("code", codeFactory)
	return r
}

func (r *Registry) Register(kind string, factory NodeFactory) error {
	if kind == "" || factory == nil {
		return fmt.Errorf("%w: node type and factory are required", ErrInvalidDefinition)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]NodeFactory)
	}
	r.factories[kind] = factory
	return nil
}

func (r *Registry) factory(kind string) NodeFactory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.factories[kind]
}

// NodeID returns the current node's stable ID, useful for business idempotency.
// The business must combine it with its own run/tenant scope.
func NodeID(ctx context.Context) string {
	id, _ := ctx.Value(nodeIDKey{}).(string)
	return id
}

type nodeIDKey struct{}
