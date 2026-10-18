package dflow

import (
	"context"
	"fmt"
)

// FuncNode adapts a handler into a node.
type FuncNode struct {
	Handler NodeFunc
}

func (n *FuncNode) Validate() error {
	if n == nil || n.Handler == nil {
		return fmt.Errorf("node handler required")
	}
	return nil
}

func (n *FuncNode) Run(ctx context.Context, input map[string]any) (Result, error) {
	if err := n.Validate(); err != nil {
		return Result{}, err
	}
	return n.Handler(ctx, input)
}
