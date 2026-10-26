package nodes

import (
	"context"

	"github.com/k0923/go/dflow"
)

type PassthroughNode struct{}

func (*PassthroughNode) Run(_ context.Context, input map[string]any) (dflow.Result, error) {
	return dflow.Result{Output: input}, nil
}
