package nodes

import (
	"context"
	"fmt"

	"github.com/k0923/go/dflow"
)

type StartNode struct{}

// Run forwards the explicitly bound workflow input:
// Inputs: map[string]nodes.Param{"input": nodes.Input()}.
//
// Run 透传显式绑定的工作流输入：
// Inputs: map[string]nodes.Param{"input": nodes.Input()}。
func (*StartNode) RunInput(_ context.Context, input map[string]any) (dflow.NodeResult, error) {
	value, ok := input["input"].(map[string]any)
	if !ok {
		return dflow.Result{}, fmt.Errorf("start: bind input with nodes.Input()")
	}
	return dflow.Result{Output: value}, nil
}
