package dflow

import "context"

// FuncNode adapts a handler into a node with a stable type.
type FuncNode struct {
	Kind    string   `json:"-"`
	Handler NodeFunc `json:"-"`
}

func (n *FuncNode) Type() string { return n.Kind }
func (n *FuncNode) Run(ctx context.Context, input map[string]any) (Result, error) {
	return n.Handler(ctx, input)
}
