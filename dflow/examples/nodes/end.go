package nodes

import (
	"context"

	"github.com/k0923/go/dflow"
)

type EndNode struct{}

func (*EndNode) RunInput(_ context.Context, input map[string]any) (dflow.NodeResult, error) {
	return dflow.Result{Output: input}, nil
}
