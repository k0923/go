package dflow

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Factory is called once to obtain Type(), then for each decoded node.
// Return fresh instances with a stable nonempty type; no business side effects.
type Factory func() Node

// NodeDecoder optionally decodes complex configuration (e.g. compiled scripts).
// Nodes without it use standard JSON decoding into the factory-created instance.
type NodeDecoder interface {
	DecodeNodeJSON(json.RawMessage) error
}

// Registry is instance-scoped and safe for concurrent registration/decoding.
// Its zero value is usable. Duplicate registrations are rejected.
type Registry struct {
	mu          sync.RWMutex
	factories   map[string]Factory
	suspensions map[string]SuspensionFactory
}

func NewRegistry() *Registry { return &Registry{factories: make(map[string]Factory)} }

func DefaultRegistry() *Registry {
	r := NewRegistry()
	if err := r.RegisterBuiltins(); err != nil {
		panic(err) // Fixed factories in an empty registry cannot conflict.
	}
	for _, factory := range []SuspensionFactory{
		func() Suspension { return &JSONSuspension{} },
		func() Suspension { return &ChildSuspension{} },
		func() Suspension { return &LoopSuspension{} },
	} {
		if err := r.RegisterSuspension(factory); err != nil {
			panic(err)
		}
	}
	return r
}

// RegisterBuiltins adds all built-ins atomically, or rejects the whole batch.
func (r *Registry) RegisterBuiltins() error {
	return r.register(
		func() Node { return &StartNode{} },
		func() Node { return &EndNode{} },
		func() Node { return &PassthroughNode{} },
		func() Node { return &JSONParseNode{} },
		func() Node { return &ConditionNode{} },
		func() Node { return &codeNode{} },
		func() Node { return &HTTPNode{} },
		func() Node { return &SubflowNode{} },
		func() Node { return &LoopNode{} },
	)
}

func (r *Registry) Register(factory Factory) error { return r.register(factory) }

func (r *Registry) register(factories ...Factory) error {
	entries := make(map[string]Factory, len(factories))
	for _, factory := range factories {
		if factory == nil {
			return fmt.Errorf("%w: registration requires a factory", ErrInvalidDefinition)
		}
		node := factory() // User code runs outside the registry lock.
		if isNil(node) || node.Type() == "" {
			return fmt.Errorf("%w: factory returned nil or empty node type", ErrInvalidDefinition)
		}
		kind := node.Type()
		if _, exists := entries[kind]; exists {
			return fmt.Errorf("%w: type %q already registered", ErrInvalidDefinition, kind)
		}
		entries[kind] = factory
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for kind := range entries {
		if _, exists := r.factories[kind]; exists {
			return fmt.Errorf("%w: type %q already registered", ErrInvalidDefinition, kind)
		}
	}
	if r.factories == nil {
		r.factories = make(map[string]Factory)
	}
	for kind, factory := range entries {
		r.factories[kind] = factory
	}
	return nil
}

// DecodeNode instantiates the node selected by spec.Type and decodes spec.Config.
// Graph metadata (ID and Inputs) remains separate from node configuration.
func (r *Registry) DecodeNode(spec NodeSpec) (Node, error) {
	r.mu.RLock()
	factory := r.factories[spec.Type]
	r.mu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("%w: unknown node type %q", ErrInvalidDefinition, spec.Type)
	}
	node := factory()
	if isNil(node) || node.Type() != spec.Type {
		return nil, fmt.Errorf("%w: factory for %q returned incompatible node", ErrInvalidDefinition, spec.Type)
	}
	raw := append(json.RawMessage(nil), spec.Config...)
	var err error
	if decoder, ok := node.(NodeDecoder); ok {
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		err = decoder.DecodeNodeJSON(raw)
	} else {
		err = decodeConfig(raw, node)
	}
	if err != nil {
		return nil, err
	}
	if isNil(node) || node.Type() != spec.Type {
		return nil, fmt.Errorf("%w: config changed node type %q", ErrInvalidDefinition, spec.Type)
	}
	return node, nil
}
