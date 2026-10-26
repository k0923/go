package nodes

import (
	"context"

	"github.com/k0923/go/dflow"
)

type EndNode struct{}

func (*EndNode) Run(_ context.Context, input map[string]any) (dflow.Result, error) {
	return dflow.Result{Output: input}, nil
}
