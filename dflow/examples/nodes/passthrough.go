package nodes

import (
	"context"

	"github.com/k0923/go/dflow"
)

type PassthroughNode struct{}

func (*PassthroughNode) RunInput(_ context.Context, input map[string]any) (dflow.NodeResult, error) {
	return dflow.Result{Output: input}, nil
}
