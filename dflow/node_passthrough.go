package dflow

import "context"

type PassthroughNode struct{}

func (*PassthroughNode) Run(_ context.Context, input map[string]any) (Result, error) {
	return Result{Output: input}, nil
}
