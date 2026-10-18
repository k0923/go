package dflow

import "context"

type EndNode struct{}

func (*EndNode) Run(_ context.Context, input map[string]any) (Result, error) {
	return Result{Output: input}, nil
}
