package nodes

import (
	"context"
	"fmt"

	"github.com/k0923/go/dflow"
)

type StartNode struct{}

// Run forwards the explicitly bound workflow input:
// Inputs: dflow.Parameters{"input": dflow.Input()}.
//
// Run 透传显式绑定的工作流输入：
// Inputs: dflow.Parameters{"input": dflow.Input()}。
func (*StartNode) Run(_ context.Context, input map[string]any) (dflow.Result, error) {
	value, ok := input["input"].(map[string]any)
	if !ok {
		return dflow.Result{}, fmt.Errorf("start: bind input with dflow.Input()")
	}
	return dflow.Result{Output: value}, nil
}
