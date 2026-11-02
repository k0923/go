package nodes

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/parameter"
)

const InputSource = "input"

type Runtime struct {
	Input  map[string]any
	Store  OutputReader
	Values parameter.Values
}

type OutputReader interface {
	Output(context.Context, string) (map[string]any, error)
}

func (r *Runtime) Resolve(ctx context.Context, path ...string) (any, error) {
	if len(path) > 0 && path[0] == InputSource {
		return parameter.InputValue(r.Input, path[1:]...)
	}
	if len(path) > 0 && r.Store != nil {
		output, err := r.Store.Output(ctx, path[0])
		if err != nil {
			return nil, err
		}
		return parameter.InputValue(output, path[1:]...)
	}
	return r.Values.Resolve(ctx, path...)
}

type Param interface {
	Get(context.Context, parameter.ValueResolver) (any, error)
	Sources() []string
}

type ConstParam struct{ Value any }
type RefParam struct{ Path []string }
type TemplateParam struct {
	Text string
	JSON bool
}

func Const(value any) Param { return ConstParam{Value: value} }
func Input(path ...string) Param {
	return RefParam{Path: append([]string{InputSource}, path...)}
}
func Ref(nodeID string, path ...string) Param {
	return RefParam{Path: append([]string{nodeID}, path...)}
}
func Template(text string) Param     { return TemplateParam{Text: text} }
func JSONTemplate(text string) Param { return TemplateParam{Text: text, JSON: true} }

func (p ConstParam) Get(ctx context.Context, _ parameter.ValueResolver) (any, error) {
	return p.Value, ctx.Err()
}
func (p ConstParam) Sources() []string { return nil }

func (p RefParam) Get(ctx context.Context, resolver parameter.ValueResolver) (any, error) {
	if len(p.Path) == 0 || p.Path[0] == "" {
		return nil, dflow.ErrInvalidReference
	}
	return resolver.Resolve(ctx, p.Path...)
}
func (p RefParam) Sources() []string {
	if len(p.Path) == 0 || p.Path[0] == "" || p.Path[0] == InputSource {
		return nil
	}
	return []string{p.Path[0]}
}

func (p TemplateParam) Get(ctx context.Context, resolver parameter.ValueResolver) (any, error) {
	return parameter.TemplateParameter{Text: p.Text, JSON: p.JSON}.Get(ctx, resolver)
}
func (p TemplateParam) Sources() []string {
	return nil
}

type InputNode interface {
	RunInput(context.Context, map[string]any) (dflow.NodeResult, error)
}

type Validator interface{ Validate() error }

type Spec struct {
	ID     string
	Node   InputNode
	Inputs map[string]Param
}

func (s Spec) GetID() string { return s.ID }

func (s Spec) Validate(context.Context, Graph) error {
	if s.ID == "" || s.Node == nil {
		return dflow.ErrInvalidDefinition
	}
	if validator, ok := s.Node.(Validator); ok {
		return validator.Validate()
	}
	return nil
}

func (s Spec) Build(_ context.Context, graph Graph) (dflow.PublishedNode[*Runtime], error) {
	topology, ok := graph[s.ID]
	if !ok {
		return nil, dflow.ErrInvalidDefinition
	}
	return Published{
		ID:           s.ID,
		Node:         runtimeNode{s.Node, maps.Clone(s.Inputs)},
		Incoming:     slices.Clone(topology.Incoming),
		Outgoing:     slices.Clone(topology.Outgoing),
		Dependencies: s.dependencies(),
	}, nil
}

func (s Spec) dependencies() []string {
	seen := map[string]bool{}
	for _, param := range s.Inputs {
		if param == nil {
			continue
		}
		for _, source := range param.Sources() {
			seen[source] = true
		}
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}

type runtimeNode struct {
	node   InputNode
	inputs map[string]Param
}

func (n runtimeNode) Run(ctx context.Context, runtime *Runtime) dflow.NodeResult {
	input := make(map[string]any, len(n.inputs))
	for name, param := range n.inputs {
		if param == nil {
			return dflow.ResultWithError(fmt.Errorf("%w: nil parameter %q", dflow.ErrInvalidReference, name))
		}
		value, err := param.Get(ctx, runtime)
		if err != nil {
			return dflow.ResultWithError(err)
		}
		input[name] = value
	}
	result, err := n.node.RunInput(ctx, input)
	if err != nil {
		return dflow.ResultWithError(err)
	}
	return result
}

type Edge struct{ Source, Target string }

func (e Edge) GetSourceNodeID() string { return e.Source }
func (e Edge) GetTargetNodeID() string { return e.Target }

type Definition struct {
	Nodes []Spec
	Edges []Edge
}

func (d Definition) GetNodes() []dflow.NodeSpec[Graph, *Runtime] {
	nodes := make([]dflow.NodeSpec[Graph, *Runtime], len(d.Nodes))
	for i := range d.Nodes {
		nodes[i] = d.Nodes[i]
	}
	return nodes
}

func (d Definition) GetEdges() []dflow.Edge {
	edges := make([]dflow.Edge, len(d.Edges))
	for i := range d.Edges {
		edges[i] = d.Edges[i]
	}
	return edges
}

type Links map[string]topology
type Graph map[string]topology

type topology struct {
	Incoming []string
	Outgoing []string
}

type Published struct {
	ID           string
	Node         dflow.RuntimeNode[*Runtime]
	Incoming     []string
	Outgoing     []string
	Dependencies []string
}

func (p Published) GetID() string                        { return p.ID }
func (p Published) GetNode() dflow.RuntimeNode[*Runtime] { return p.Node }
func (p Published) GetIncoming() []string                { return p.Incoming }
func (p Published) GetOutgoing() []string                { return p.Outgoing }
func (p Published) GetDependencies() []string            { return p.Dependencies }

func Compile(ctx context.Context, definition Definition) ([]dflow.PublishedNode[*Runtime], error) {
	graph := Graph{}
	for _, node := range definition.Nodes {
		graph[node.ID] = topology{}
	}
	for _, edge := range definition.Edges {
		source, target := graph[edge.Source], graph[edge.Target]
		source.Outgoing = append(source.Outgoing, edge.Target)
		target.Incoming = append(target.Incoming, edge.Source)
		graph[edge.Source], graph[edge.Target] = source, target
	}
	return dflow.Compile(ctx, definition, graph)
}

func RuntimeFrom(input map[string]any, outputs map[string]dflow.NodeState) *Runtime {
	values := parameter.Values{}
	for id, state := range outputs {
		if state.Status == dflow.Succeeded {
			values[id] = maps.Clone(state.Output)
		}
	}
	return &Runtime{Input: maps.Clone(input), Values: values}
}

func RuntimeWithStore(input map[string]any, store OutputReader) *Runtime {
	return &Runtime{Input: maps.Clone(input), Store: store}
}
