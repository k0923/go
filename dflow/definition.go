package dflow

import (
	"context"
	"fmt"
	"reflect"
	"slices"
)

type Edge interface {
	GetSourceNodeID() string
	GetTargetNodeID() string
}

// Definition has one root and may have multiple sinks. V is used only while
// compiling, R only while executing.
type Definition[V any, R any] interface {
	GetNodes() []NodeSpec[V, R]
	GetEdges() []Edge
}

// PublishedNode exposes a validated runtime record without prescribing its
// business representation or storage format. Implementations must be immutable
// and safe for concurrent reads. Returned slices and nodes are read-only.
type PublishedNode[R any] interface {
	GetID() string
	GetNode() RuntimeNode[R]
	GetIncoming() []string
	GetOutgoing() []string
	// GetDependencies is the complete, duplicate-free list of output sources
	// this node may read. It is independent of direct control-flow predecessors.
	GetDependencies() []string
}

// DefinitionProvider is bound to one validated immutable publication. LoadNode
// loads one executable record.
type DefinitionProvider[R any] interface {
	LoadNode(context.Context, string) (PublishedNode[R], error)
}

// Compile validates topology and business-built publications, returning the
// original business objects in topological order. It does not run nodes, wrap
// publications or supply a storage/provider implementation.
func Compile[V any, R any](ctx context.Context, definition Definition[V, R], vc V) ([]PublishedNode[R], error) {
	fail := func(id, detail string, cause error) ([]PublishedNode[R], error) {
		return nil, &ValidationError{NodeID: id, Detail: detail, Cause: cause}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isNil(definition) {
		return fail("", "nil definition", ErrInvalidDefinition)
	}
	nodes := definition.GetNodes()
	edges := definition.GetEdges()
	if len(nodes) == 0 {
		return fail("", "at least one node required", ErrInvalidDefinition)
	}
	specs := make(map[string]NodeSpec[V, R], len(nodes))
	incoming := make(map[string][]string, len(nodes))
	outgoing := make(map[string][]string, len(nodes))
	ids := make([]string, 0, len(nodes))
	for _, spec := range nodes {
		if isNil(spec) {
			return fail("", "nil node spec", ErrInvalidDefinition)
		}
		id := spec.GetID()
		if id == "" {
			return fail(id, "empty node ID", ErrInvalidDefinition)
		}
		if _, exists := specs[id]; exists {
			return fail(id, "duplicate node ID", ErrInvalidDefinition)
		}
		ids = append(ids, id)
		specs[id] = spec
	}
	seen := map[[2]string]bool{}
	degrees := map[string]int{}
	for _, edge := range edges {
		if isNil(edge) {
			return fail("", "nil edge", ErrInvalidDefinition)
		}
		key := [2]string{edge.GetSourceNodeID(), edge.GetTargetNodeID()}
		source, target := key[0], key[1]
		_, sourceOK := specs[source]
		_, targetOK := specs[target]
		if !sourceOK || !targetOK || seen[key] {
			return fail(target, "invalid or duplicate edge", ErrInvalidDefinition)
		}
		seen[key] = true
		outgoing[source] = append(outgoing[source], target)
		incoming[target] = append(incoming[target], source)
		degrees[target]++
	}
	order := make([]string, 0, len(ids))
	for _, id := range ids {
		if degrees[id] == 0 {
			order = append(order, id)
		}
	}
	if len(order) != 1 {
		return fail("", "exactly one root required", ErrInvalidDefinition)
	}
	for i := 0; i < len(order); i++ {
		for _, id := range outgoing[order[i]] {
			degrees[id]--
			if degrees[id] == 0 {
				order = append(order, id)
			}
		}
	}
	if len(order) != len(ids) {
		return fail("", "graph contains a cycle", ErrInvalidDefinition)
	}
	ancestors := map[string]map[string]bool{}
	publication := make([]PublishedNode[R], 0, len(order))
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ancestors[id] = map[string]bool{}
		for _, predecessor := range incoming[id] {
			ancestors[id][predecessor] = true
			for ancestor := range ancestors[predecessor] {
				ancestors[id][ancestor] = true
			}
		}
		spec := specs[id]
		if err := spec.Validate(ctx, vc); err != nil {
			return fail(id, "node configuration", err)
		}
		record, err := spec.Build(ctx, vc)
		if err != nil {
			return fail(id, "build node", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if isNil(record) || record.GetID() != id {
			return fail(id, "invalid publication", ErrInvalidDefinition)
		}
		if isNil(record.GetNode()) {
			return fail(id, "invalid runtime node", ErrInvalidDefinition)
		}
		if !sameIDs(record.GetIncoming(), incoming[id]) || !sameIDs(record.GetOutgoing(), outgoing[id]) {
			return fail(id, "publication topology does not match definition edges", ErrInvalidDefinition)
		}
		deps := record.GetDependencies()
		if err := validateIDs(deps); err != nil {
			return fail(id, "dependencies", ErrInvalidReference)
		}
		for _, dep := range deps {
			if !ancestors[id][dep] {
				return fail(id, fmt.Sprintf("dependency %q is not an ancestor", dep), ErrInvalidReference)
			}
		}
		publication = append(publication, record)
	}
	return publication, nil
}

// sameIDs compares unordered, duplicate-free topology lists.
func sameIDs(actual, expected []string) bool {
	if validateIDs(actual) != nil || len(actual) != len(expected) {
		return false
	}
	for _, id := range actual {
		if !slices.Contains(expected, id) {
			return false
		}
	}
	return true
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}
