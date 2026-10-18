package dflow

import (
	"context"
	"fmt"
)

type ConditionNode struct{}

func (*ConditionNode) Type() string { return "condition" }

// Run selects true/false ports from a resolved boolean input.
// Business predicates can be supplied by a code/custom predecessor.
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
