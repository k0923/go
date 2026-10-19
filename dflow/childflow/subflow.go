package childflow

import (
	"context"

	"github.com/k0923/go/dflow"
)

// SubflowNode sends its resolved inputs as the child input.
type SubflowNode struct {
	Publication string `json:"publication"`
	OutputNode  string `json:"outputNode"`
}

func (n *SubflowNode) Validate() error {
	if n.Publication == "" || n.OutputNode == "" {
		return errRequired
	}
	return nil
}
func (n *SubflowNode) Run(ctx context.Context, input map[string]any) (dflow.Result, error) {
	if err := ctx.Err(); err != nil {
		return dflow.Result{}, err
	}
	if n.Publication == "" || n.OutputNode == "" {
		return dflow.Result{}, dflow.ErrInvalidDefinition
	}
	return dflow.Result{Suspend: &ChildSuspension{ChildRequest: ChildRequest{
		Publication: n.Publication, OutputNode: n.OutputNode, Input: input,
	}}}, nil
}
func (*SubflowNode) Resume(ctx context.Context, _ map[string]any, resumed dflow.ResumeInput) (dflow.Result, error) {
	if err := ctx.Err(); err != nil {
		return dflow.Result{}, err
	}
	response, err := decodeChildResponse(resumed.Response)
	return dflow.Result{Output: response.Output}, err
}
