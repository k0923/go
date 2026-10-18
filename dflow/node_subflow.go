package dflow

import (
	"context"
	"fmt"
)

// SubflowNode sends its resolved inputs as the child input.
type SubflowNode struct {
	Publication string `json:"publication"`
	OutputNode  string `json:"outputNode"`
}

func (n *SubflowNode) Validate() error {
	if n.Publication == "" || n.OutputNode == "" {
		return fmt.Errorf("subflow: publication and outputNode required")
	}
	return nil
}
func (n *SubflowNode) Run(ctx context.Context, input map[string]any) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if n.Publication == "" || n.OutputNode == "" {
		return Result{}, ErrInvalidDefinition
	}
	return Result{Suspend: &ChildSuspension{ChildRequest: ChildRequest{
		Publication: n.Publication, OutputNode: n.OutputNode, Input: input,
	}}}, nil
}
func (*SubflowNode) Resume(ctx context.Context, _ map[string]any, resumed ResumeInput) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	response, err := decodeChildResponse(resumed.Response)
	return Result{Output: response.Output}, err
}
