package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/examples/nodes"
	"github.com/k0923/go/dflow/parameter"
	xjson "github.com/k0923/go/json"
)

type NodeConfig interface {
	BuildNode(*reviewService) (nodes.InputNode, error)
}

type ParamConfig interface {
	BuildParam() (nodes.Param, error)
}

type DefinitionJSON struct {
	Version string         `json:"version"`
	Nodes   []NodeSpecJSON `json:"nodes"`
	Edges   []EdgeJSON     `json:"edges"`
}

type NodeSpecJSON struct {
	ID     string                          `json:"id"`
	Node   xjson.G[NodeConfig]             `json:"node"`
	Inputs map[string]xjson.G[ParamConfig] `json:"inputs,omitempty"`
}

type EdgeJSON struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

func (n NodeSpecJSON) Build(service *reviewService) (nodes.Spec, error) {
	if n.Node.Value() == nil {
		return nodes.Spec{}, fmt.Errorf("node %q: missing config", n.ID)
	}
	node, err := n.Node.Value().BuildNode(service)
	if err != nil {
		return nodes.Spec{}, fmt.Errorf("node %q: %w", n.ID, err)
	}
	spec := nodes.Spec{ID: n.ID, Node: node}
	if n.Inputs != nil {
		spec.Inputs = make(map[string]nodes.Param, len(n.Inputs))
		for key, value := range n.Inputs {
			if value.Value() == nil {
				return nodes.Spec{}, fmt.Errorf("node %q input %q: missing parameter", n.ID, key)
			}
			param, err := value.Value().BuildParam()
			if err != nil {
				return nodes.Spec{}, fmt.Errorf("node %q input %q: %w", n.ID, key, err)
			}
			spec.Inputs[key] = param
		}
	}
	return spec, nil
}

func (d DefinitionJSON) Build(service *reviewService) (nodes.Definition, error) {
	def := nodes.Definition{}
	for _, wire := range d.Nodes {
		node, err := wire.Build(service)
		if err != nil {
			return def, err
		}
		def.Nodes = append(def.Nodes, node)
	}
	for _, edge := range d.Edges {
		def.Edges = append(def.Edges, nodes.Edge{Source: edge.Source, Target: edge.Target})
	}
	return def, nil
}

func (d DefinitionJSON) Compile(ctx context.Context, service *reviewService) ([]dflow.PublishedNode[*nodes.Runtime], error) {
	def, err := d.Build(service)
	if err != nil {
		return nil, err
	}
	return nodes.Compile(ctx, def)
}

type ScriptJSON struct {
	Language  string `json:"language,omitempty"`
	Content   string `json:"content"`
	TimeoutMS int64  `json:"timeoutMs,omitempty"`
}

func (c *ScriptJSON) BuildNode(_ *reviewService) (nodes.InputNode, error) {
	return nodes.NewCodeNode(nodes.CodeConfig{
		Script: nodes.Script{Language: c.Language, Content: c.Content}, TimeoutMS: c.TimeoutMS,
	})
}

type ReviewJSON struct {
	Approver string `json:"approver"`
}

func (c *ReviewJSON) BuildNode(service *reviewService) (nodes.InputNode, error) {
	if service == nil || c.Approver == "" {
		return nil, fmt.Errorf("review requires a service and an approver")
	}
	return &reviewNode{config: *c, service: service}, nil
}

type EndJSON struct{}

func (*EndJSON) BuildNode(_ *reviewService) (nodes.InputNode, error) {
	return &nodes.EndNode{}, nil
}

type reviewService struct{}

func (*reviewService) Summary(approver string, total float64) string {
	return fmt.Sprintf("%s 确认金额 %.0f", approver, total)
}

type reviewNode struct {
	config  ReviewJSON
	service *reviewService
}

func (n *reviewNode) RunInput(_ context.Context, input map[string]any) (dflow.NodeResult, error) {
	total, ok := input["total"].(float64)
	if !ok {
		return dflow.Result{}, fmt.Errorf("total must be a number")
	}
	approved, ok := input["approved"].(bool)
	if !ok {
		return dflow.AsyncResult{Result: dflow.Result{Output: map[string]any{
			"approver": n.config.Approver,
			"total":    total,
			"summary":  n.service.Summary(n.config.Approver, total),
		}}}, nil
	}
	return dflow.AsyncResult{Done: true, Result: dflow.Result{Output: map[string]any{
		"approved": approved,
		"total":    total,
		"summary":  n.service.Summary(n.config.Approver, total),
	}}}, nil
}

type ConstantJSON struct {
	Value any `json:"value"`
}

func (p *ConstantJSON) BuildParam() (nodes.Param, error) { return nodes.Const(p.Value), nil }

type ReferenceJSON struct {
	Path     []string `json:"path"`
	Optional bool     `json:"optional,omitempty"`
}

func (p *ReferenceJSON) BuildParam() (nodes.Param, error) {
	if len(p.Path) == 0 {
		return nil, dflow.ErrInvalidReference
	}
	var param nodes.Param
	if p.Path[0] == "$input" || p.Path[0] == nodes.InputSource {
		param = nodes.Input(p.Path[1:]...)
	} else {
		param = nodes.Ref(p.Path[0], p.Path[1:]...)
	}
	if p.Optional {
		return optionalParam{Param: param}, nil
	}
	return param, nil
}

type optionalParam struct{ nodes.Param }

func (p optionalParam) Get(ctx context.Context, resolver parameter.ValueResolver) (any, error) {
	value, err := p.Param.Get(ctx, resolver)
	if errors.Is(err, parameter.ErrMissingValue) {
		return nil, nil
	}
	return value, err
}

type TemplateJSON struct {
	Text string `json:"text"`
	JSON bool   `json:"json,omitempty"`
}

func (p *TemplateJSON) BuildParam() (nodes.Param, error) {
	if p.JSON {
		return nodes.JSONTemplate(p.Text), nil
	}
	return nodes.Template(p.Text), nil
}

func init() {
	xjson.Bind[NodeConfig](map[string]NodeConfig{
		"script": &ScriptJSON{}, "review": &ReviewJSON{}, "end": &EndJSON{},
	}, xjson.WithFlatLayout())
	xjson.Bind[ParamConfig](map[string]ParamConfig{
		"constant": &ConstantJSON{}, "reference": &ReferenceJSON{}, "template": &TemplateJSON{},
	}, xjson.WithFlatLayout())
}
