package dflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
)

type NodeSpec struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Inputs Parameters      `json:"inputsValues,omitempty"`
	Config json.RawMessage `json:"config,omitempty"`
}

type Edge struct {
	SourceNodeID string `json:"sourceNodeID"`
	TargetNodeID string `json:"targetNodeID"`
	SourcePortID string `json:"sourcePortID,omitempty"`
}

// Definition supports multiple roots and sinks. All nodes participate in the run.
// Version must change when application-provided node implementations change.
type Definition struct {
	Version string     `json:"version,omitempty"`
	Nodes   []NodeSpec `json:"nodes"`
	Edges   []Edge     `json:"edges"`
}

// Graph is compiled, immutable and reusable across run-scoped infrastructures.
type Graph struct {
	definition Definition
	hash       string
	order      []string
	specs      map[string]NodeSpec
	nodes      map[string]Node
	incoming   map[string][]Edge
	outgoing   map[string][]Edge
}

// Compile validates topology, references and node configurations without running nodes.
func Compile(definition Definition, registry *Registry) (*Graph, error) {
	if registry == nil {
		registry = DefaultRegistry()
	}
	def, err := cloneJSON(definition)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDefinition, err)
	}
	g := &Graph{
		definition: def, specs: map[string]NodeSpec{}, nodes: map[string]Node{},
		incoming: map[string][]Edge{}, outgoing: map[string][]Edge{},
	}
	fail := func(id, detail string, cause error) (*Graph, error) {
		return nil, &ValidationError{NodeID: id, Detail: detail, Cause: cause}
	}
	if len(def.Nodes) == 0 {
		return fail("", "at least one node is required", ErrInvalidDefinition)
	}
	for _, spec := range def.Nodes {
		if spec.ID == "" || spec.ID == InputSource {
			return fail(spec.ID, "empty or reserved node ID", ErrInvalidDefinition)
		}
		if _, exists := g.specs[spec.ID]; exists {
			return fail(spec.ID, "duplicate node ID", ErrInvalidDefinition)
		}
		g.specs[spec.ID] = spec
	}
	seen := map[Edge]bool{}
	degrees := map[string]int{}
	for _, edge := range def.Edges {
		if _, ok := g.specs[edge.SourceNodeID]; !ok {
			return fail(edge.SourceNodeID, "edge source not found", ErrInvalidDefinition)
		}
		if _, ok := g.specs[edge.TargetNodeID]; !ok {
			return fail(edge.TargetNodeID, "edge target not found", ErrInvalidDefinition)
		}
		if seen[edge] {
			return fail(edge.TargetNodeID, "duplicate edge", ErrInvalidDefinition)
		}
		seen[edge] = true
		g.incoming[edge.TargetNodeID] = append(g.incoming[edge.TargetNodeID], edge)
		g.outgoing[edge.SourceNodeID] = append(g.outgoing[edge.SourceNodeID], edge)
		degrees[edge.TargetNodeID]++
	}
	for _, spec := range def.Nodes {
		if degrees[spec.ID] == 0 {
			g.order = append(g.order, spec.ID)
		}
	}
	for i := 0; i < len(g.order); i++ {
		for _, edge := range g.outgoing[g.order[i]] {
			degrees[edge.TargetNodeID]--
			if degrees[edge.TargetNodeID] == 0 {
				g.order = append(g.order, edge.TargetNodeID)
			}
		}
	}
	if len(g.order) != len(def.Nodes) {
		return fail("", "graph contains a cycle", ErrInvalidDefinition)
	}
	ancestors := map[string]map[string]bool{}
	for _, id := range g.order {
		ancestors[id] = map[string]bool{}
		for _, edge := range g.incoming[id] {
			ancestors[id][edge.SourceNodeID] = true
			for ancestor := range ancestors[edge.SourceNodeID] {
				ancestors[id][ancestor] = true
			}
		}
		for key, parameter := range g.specs[id].Inputs {
			refs, err := parameter.references()
			if err != nil {
				return fail(id, "input "+key, err)
			}
			for _, path := range refs {
				if path[0] != InputSource && !ancestors[id][path[0]] {
					return fail(id, fmt.Sprintf("input %q references non-ancestor %q", key, path[0]), ErrInvalidReference)
				}
			}
		}
	}
	for _, spec := range def.Nodes {
		factory := registry.factory(spec.Type)
		if factory == nil {
			return fail(spec.ID, "unknown node type "+spec.Type, ErrInvalidDefinition)
		}
		node, err := factory(append(json.RawMessage(nil), spec.Config...))
		if err != nil {
			return fail(spec.ID, "node configuration", err)
		}
		if isNil(node) {
			return fail(spec.ID, "factory returned nil node", ErrInvalidDefinition)
		}
		g.nodes[spec.ID] = node
	}
	raw, _ := json.Marshal(def)
	hash := sha256.Sum256(raw)
	g.hash = hex.EncodeToString(hash[:])
	return g, nil
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
