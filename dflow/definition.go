package dflow

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

type NodeSpec struct {
	ID     string
	Node   Node
	Inputs Parameters
}

type Edge struct {
	SourceNodeID string `json:"sourceNodeID"`
	TargetNodeID string `json:"targetNodeID"`
	SourcePortID string `json:"sourcePortID,omitempty"`
}

// Definition has exactly one root/start node and may have multiple sinks. All
// nodes participate in the run. Version must change when application-provided
// node implementations change.
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
	incoming   map[string][]Edge
	outgoing   map[string][]Edge
}

// Compile validates topology, references and node configurations without running nodes.
func Compile(definition Definition) (*Graph, error) {
	if definition.Version == "" {
		return nil, fmt.Errorf("%w: immutable definition version required", ErrInvalidDefinition)
	}
	def := definition
	def.Nodes = slices.Clone(definition.Nodes)
	def.Edges = slices.Clone(definition.Edges)
	for index := range def.Nodes {
		def.Nodes[index].Inputs = maps.Clone(def.Nodes[index].Inputs)
	}
	g := &Graph{
		definition: def, specs: map[string]NodeSpec{},
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
	roots := 0
	for _, id := range g.order {
		if len(g.incoming[id]) == 0 {
			roots++
		}
	}
	if roots != 1 {
		return fail("", "exactly one start node is required", ErrInvalidDefinition)
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
			if isNil(parameter) {
				return fail(id, "input "+key, ErrInvalidReference)
			}
			refs, err := parameter.References()
			if err != nil {
				return fail(id, "input "+key, err)
			}
			for _, path := range refs {
				if err := validatePath(path); err != nil {
					return fail(id, "input "+key, err)
				}
				if path[0] != InputSource && !ancestors[id][path[0]] {
					return fail(id, fmt.Sprintf("input %q references non-ancestor %q", key, path[0]), ErrInvalidReference)
				}
			}
		}
	}
	for _, spec := range def.Nodes {
		if isNil(spec.Node) {
			return fail(spec.ID, "node required", ErrInvalidDefinition)
		}
		if validator, ok := spec.Node.(NodeValidator); ok {
			if err := validator.Validate(); err != nil {
				return fail(spec.ID, "node configuration", err)
			}
		}
	}
	g.hash = def.Version
	return g, nil
}

// Manifest is the small immutable header needed to start or recover execution.
type Manifest struct {
	Hash  string `json:"hash"`
	Start string `json:"start"`
}

// PublishedNode contains an executable node plus validated topology.
// Incoming contains UNIQUE predecessor IDs, not the number of incoming ports.
type PublishedNode struct {
	Spec     NodeSpec `json:"spec"`
	Incoming []string `json:"incoming"`
	Outgoing []Edge   `json:"outgoing"`
}

// DefinitionProvider binds to ONE immutable, already validated publication.
// Implementations load individual database records, not the whole definition.
// Return independent metadata copies with immutable nodes and parameters.
// Manifest.Hash is an application-supplied version; change it when definitions change.
type DefinitionProvider interface {
	Manifest(context.Context) (Manifest, error)
	LoadNode(context.Context, string) (*PublishedNode, error)
	// Incoming loads only a target's predecessor index, not its config.
	Incoming(context.Context, string) ([]string, error)
}

func (g *Graph) Incoming(ctx context.Context, id string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, exists := g.specs[id]; !exists {
		return nil, ErrInvalidDefinition
	}
	var result []string
	seen := map[string]bool{}
	for _, edge := range g.incoming[id] {
		if !seen[edge.SourceNodeID] {
			seen[edge.SourceNodeID] = true
			result = append(result, edge.SourceNodeID)
		}
	}
	return result, nil
}
func (g *Graph) Manifest(ctx context.Context) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	m := Manifest{Hash: g.hash}
	for _, id := range g.order {
		if len(g.incoming[id]) == 0 {
			m.Start = id
			return m, nil
		}
	}
	return Manifest{}, ErrInvalidDefinition
}

func (g *Graph) LoadNode(ctx context.Context, id string) (*PublishedNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	spec, exists := g.specs[id]
	if !exists {
		return nil, fmt.Errorf("%w: node %q", ErrInvalidDefinition, id)
	}
	spec.Inputs = maps.Clone(spec.Inputs)
	node := PublishedNode{Spec: spec, Outgoing: slices.Clone(g.outgoing[id])}
	seen := map[string]bool{}
	for _, edge := range g.incoming[id] {
		if !seen[edge.SourceNodeID] {
			seen[edge.SourceNodeID] = true
			node.Incoming = append(node.Incoming, edge.SourceNodeID)
		}
	}
	return &node, nil
}

// Publication exports executable records. The application decides how to store
// configuration and rebuild nodes and dependencies.
func (g *Graph) Publication() (Manifest, []PublishedNode, error) {
	m, err := g.Manifest(context.Background())
	if err != nil {
		return m, nil, err
	}
	nodes := make([]PublishedNode, 0, len(g.order))
	for _, id := range g.order {
		node, err := g.LoadNode(context.Background(), id)
		if err != nil {
			return m, nil, err
		}
		nodes = append(nodes, *node)
	}
	return m, nodes, nil
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
