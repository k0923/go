package main

import (
	"context"
	"fmt"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/examples/nodes"
	xjson "github.com/k0923/go/json"
)

// JSON types belong to the application. BuildNode can fail (e.g. invalid JS)
// and receives dependencies that are rebuilt after a process restart.
type NodeConfig interface {
	BuildNode(*reviewService) (dflow.Node, error)
}

type DefinitionJSON struct {
	Version string         `json:"version"`
	Nodes   []NodeSpecJSON `json:"nodes"`
	Edges   []EdgeJSON     `json:"edges"`
}

type NodeSpecJSON struct {
	ID     string                              `json:"id"`
	Node   xjson.G[NodeConfig]                 `json:"node"`
	Inputs map[string]xjson.G[dflow.Parameter] `json:"inputs,omitempty"`
}

type EdgeJSON struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Port   string `json:"port,omitempty"`
}

func (e EdgeJSON) Edge() dflow.Edge {
	return dflow.Edge{SourceNodeID: e.Source, TargetNodeID: e.Target, SourcePortID: e.Port}
}

func (n NodeSpecJSON) Build(service *reviewService) (dflow.NodeSpec, error) {
	if n.Node.Value() == nil {
		return dflow.NodeSpec{}, fmt.Errorf("node %q: missing config", n.ID)
	}
	node, err := n.Node.Value().BuildNode(service)
	if err != nil {
		return dflow.NodeSpec{}, fmt.Errorf("node %q: %w", n.ID, err)
	}
	var inputs dflow.Parameters
	if n.Inputs != nil {
		inputs = make(dflow.Parameters, len(n.Inputs))
		for key, value := range n.Inputs {
			inputs[key] = value.Value()
		}
	}
	return dflow.NodeSpec{ID: n.ID, Node: node, Inputs: inputs}, nil
}

func (d DefinitionJSON) Build(service *reviewService) (dflow.Definition, error) {
	def := dflow.Definition{Version: d.Version}
	for _, wire := range d.Nodes {
		node, err := wire.Build(service)
		if err != nil {
			return def, err
		}
		def.Nodes = append(def.Nodes, node)
	}
	for _, edge := range d.Edges {
		def.Edges = append(def.Edges, edge.Edge())
	}
	return def, nil
}

// Store script source, never the compiled VM program.
type ScriptJSON struct {
	Language  string `json:"language,omitempty"`
	Content   string `json:"content"`
	TimeoutMS int64  `json:"timeoutMs,omitempty"`
}

type scriptNode struct {
	*nodes.CodeNode
	config ScriptJSON
}

func (c *ScriptJSON) BuildNode(_ *reviewService) (dflow.Node, error) {
	node, err := nodes.NewCodeNode(nodes.CodeConfig{
		Script: nodes.Script{Language: c.Language, Content: c.Content}, TimeoutMS: c.TimeoutMS,
	})
	if err != nil {
		return nil, err
	}
	return &scriptNode{CodeNode: node, config: *c}, nil
}

type ReviewJSON struct {
	Approver string `json:"approver"`
}

func (c *ReviewJSON) BuildNode(service *reviewService) (dflow.Node, error) {
	if service == nil || c.Approver == "" {
		return nil, fmt.Errorf("review requires a service and an approver")
	}
	return &reviewNode{config: *c, service: service}, nil
}

type EndJSON struct{}

func (*EndJSON) BuildNode(_ *reviewService) (dflow.Node, error) {
	return &nodes.EndNode{}, nil
}

// The service is an injected business dependency. It has no JSON representation.
type reviewService struct{}

func (*reviewService) Summary(approver string, total float64) string {
	return fmt.Sprintf("%s 确认金额 %.0f", approver, total)
}

type reviewNode struct {
	config  ReviewJSON
	service *reviewService
}

func (n *reviewNode) Run(_ context.Context, input map[string]any) (dflow.Result, error) {
	total, ok := input["total"].(float64)
	if !ok {
		return dflow.Result{}, fmt.Errorf("total must be a number")
	}
	return dflow.Result{Suspend: &ReviewRequest{
		Approver: n.config.Approver, Total: total,
		Summary: n.service.Summary(n.config.Approver, total),
	}}, nil
}

func (n *reviewNode) Resume(_ context.Context, _ map[string]any, input dflow.ResumeInput) (dflow.Result, error) {
	request, ok := input.Suspension.(*ReviewRequest)
	if !ok {
		return dflow.Result{}, dflow.ErrInvalidSuspension
	}
	reply, ok := input.Response.(*ReviewReply)
	if !ok {
		return dflow.Result{}, fmt.Errorf("expected a typed review reply, got %T", input.Response)
	}
	return dflow.Result{Output: map[string]any{
		"approved": reply.Approved, "total": request.Total,
		"summary": n.service.Summary(request.Approver, request.Total),
	}}, nil
}

type ReviewRequest struct {
	Approver string  `json:"approver"`
	Total    float64 `json:"total"`
	Summary  string  `json:"summary"`
}

func (*ReviewRequest) Type() string { return "review" }

type Callback interface{ isCallback() }

type ReviewReply struct {
	Approved bool `json:"approved"`
}

func (*ReviewReply) isCallback() {}

// Adapters keep JSON field names in the application and reuse dflow's parameter
// behavior, including reference validation and plain/JSON template rendering.
type ConstantJSON struct {
	Value any `json:"value"`
}

func (p *ConstantJSON) References() ([][]string, error) { return nil, nil }
func (p *ConstantJSON) Resolve(ctx context.Context, r dflow.ValueResolver) (any, error) {
	return dflow.Constant(p.Value).Resolve(ctx, r)
}

type ReferenceJSON struct {
	Path []string `json:"path"`
}

func (p *ReferenceJSON) References() ([][]string, error) {
	return (dflow.ReferenceParameter{Path: p.Path}).References()
}
func (p *ReferenceJSON) Resolve(ctx context.Context, r dflow.ValueResolver) (any, error) {
	return (dflow.ReferenceParameter{Path: p.Path}).Resolve(ctx, r)
}

type TemplateJSON struct {
	Text string `json:"text"`
	JSON bool   `json:"json,omitempty"`
}

func (p *TemplateJSON) References() ([][]string, error) {
	return (dflow.TemplateParameter{Text: p.Text, JSON: p.JSON}).References()
}
func (p *TemplateJSON) Resolve(ctx context.Context, r dflow.ValueResolver) (any, error) {
	return (dflow.TemplateParameter{Text: p.Text, JSON: p.JSON}).Resolve(ctx, r)
}

func nodeSpecJSON(spec dflow.NodeSpec) (NodeSpecJSON, error) {
	var config NodeConfig
	switch node := spec.Node.(type) {
	case *scriptNode:
		copy := node.config
		config = &copy
	case *reviewNode:
		copy := node.config
		config = &copy
	case *nodes.EndNode:
		config = &EndJSON{}
	default:
		return NodeSpecJSON{}, fmt.Errorf("unsupported node %T", spec.Node)
	}
	wire := NodeSpecJSON{ID: spec.ID, Node: xjson.NG[NodeConfig](config)}
	if spec.Inputs != nil {
		wire.Inputs = make(map[string]xjson.G[dflow.Parameter], len(spec.Inputs))
		for key, value := range spec.Inputs {
			wire.Inputs[key] = xjson.NG[dflow.Parameter](value)
		}
	}
	return wire, nil
}

func init() {
	// Bind once for each application interface. No registry is added to dflow.
	xjson.Bind[NodeConfig](map[string]NodeConfig{
		"script": &ScriptJSON{}, "review": &ReviewJSON{}, "end": &EndJSON{},
	}, xjson.WithFlatLayout())
	xjson.Bind[dflow.Parameter](map[string]dflow.Parameter{
		"constant": &ConstantJSON{}, "reference": &ReferenceJSON{}, "template": &TemplateJSON{},
	}, xjson.WithFlatLayout())
	xjson.Bind[dflow.Suspension](map[string]dflow.Suspension{
		"review": &ReviewRequest{},
	})
	xjson.Bind[Callback](map[string]Callback{"reviewReply": &ReviewReply{}})
}
