package nodes

import (
	"context"
	"fmt"

	"github.com/k0923/go/dflow"
)

// FuncNode adapts a handler into a node.
//
// FuncNode 将处理函数适配为节点。
type FuncNode struct {
	Handler dflow.NodeFunc
}

func (n *FuncNode) Validate() error {
	if n == nil || n.Handler == nil {
		return fmt.Errorf("node handler required")
	}
	return nil
}

func (n *FuncNode) Run(ctx context.Context, input map[string]any) (dflow.Result, error) {
	if err := n.Validate(); err != nil {
		return dflow.Result{}, err
	}
	return n.Handler(ctx, input)
}
