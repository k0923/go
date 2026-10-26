package dflow

import (
	"context"
	"fmt"
)

type ConditionNode struct{}

// Run selects true/false ports from a resolved boolean input. Business
// predicates can be supplied by a code/custom predecessor.
//
// Run 根据已解析的布尔输入选择 true/false 端口。业务谓词可以由 code 或自定义
// 前驱节点提供。
func (*ConditionNode) Run(_ context.Context, input map[string]any) (Result, error) {
	value, ok := input["condition"].(bool)
	if !ok {
		return Result{}, fmt.Errorf("condition input must be a boolean")
	}
	port := "false"
	if value {
		port = "true"
	}
	return Result{Output: map[string]any{"matchedKey": port}, Ports: []string{port}}, nil
}
