package dflow

import "context"

type StartNode struct{}

func (*StartNode) Type() string { return "start" }
func (*StartNode) Run(_ context.Context, input map[string]any) (Result, error) {
	return Result{Output: input}, nil
}
