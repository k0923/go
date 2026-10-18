package dflow

import (
	"context"
	"fmt"
	"slices"
)

// LoopNode runs one child per iteration. foreach consumes input.items; until
// consumes input.value and expects child output {done:bool,value:any}.
// Child input is {index,item,value,params}; params is the parent node input.
type LoopNode struct {
	Publication   string `json:"publication"`
	OutputNode    string `json:"outputNode"`
	Mode          string `json:"mode,omitempty"` // foreach (default), until
	MaxIterations int    `json:"maxIterations,omitempty"`
}

type LoopSuspension struct {
	Index   int          `json:"index"`
	Value   any          `json:"value"`
	Results []any        `json:"results"`
	Child   ChildRequest `json:"child"`
}

func (*LoopSuspension) Type() string              { return "dflow.loop" }
func (n *LoopSuspension) ChildCall() ChildRequest { return n.Child }

func (n *LoopNode) Validate() error {
	if n.Publication == "" || n.OutputNode == "" {
		return fmt.Errorf("loop: publication and outputNode required")
	}
	if n.Mode != "" && n.Mode != "foreach" && n.Mode != "until" {
		return fmt.Errorf("loop: mode must be foreach or until")
	}
	if n.MaxIterations < 0 {
		return fmt.Errorf("loop: maxIterations must be nonnegative")
	}
	return nil
}
func (n *LoopNode) limit() int {
	if n.MaxIterations == 0 {
		return 1000
	}
	return n.MaxIterations
}
func (n *LoopNode) Run(ctx context.Context, input map[string]any) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := n.Validate(); err != nil {
		return Result{}, err
	}
	return n.next(input, LoopSuspension{Value: input["value"], Results: []any{}})
}
func (n *LoopNode) next(input map[string]any, state LoopSuspension) (Result, error) {
	var item any
	if n.Mode != "until" {
		items, ok := input["items"].([]any)
		if !ok {
			return Result{}, fmt.Errorf("loop: items must be an array")
		}
		if len(items) > n.limit() {
			return Result{}, fmt.Errorf("loop: items exceed maxIterations")
		}
		if state.Index == len(items) {
			return Result{Output: state.Results}, nil
		}
		if state.Index < 0 || state.Index > len(items) {
			return Result{}, fmt.Errorf("loop: invalid saved index")
		}
		item = items[state.Index]
	}
	if state.Index >= n.limit() {
		return Result{}, fmt.Errorf("loop: maxIterations reached")
	}
	state.Child = ChildRequest{Publication: n.Publication, OutputNode: n.OutputNode,
		Input: map[string]any{"index": state.Index, "item": item, "value": state.Value, "params": input}}
	return Result{Suspend: &state}, nil
}
func (n *LoopNode) Resume(ctx context.Context, input map[string]any, resumed ResumeInput) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := n.Validate(); err != nil {
		return Result{}, err
	}
	payload, ok := resumed.Suspension.(*LoopSuspension)
	if !ok || payload == nil {
		return Result{}, ErrInvalidSuspension
	}
	// Copy before mutation so repeated Resume calls do not mutate persisted state.
	state := *payload
	state.Results = slices.Clone(payload.Results)
	if state.Index < 0 || state.Index >= n.limit() {
		return Result{}, fmt.Errorf("loop: invalid saved index")
	}
	response, err := decodeChildResponse(resumed.Response)
	if err != nil {
		return Result{}, err
	}
	state.Index++
	if n.Mode == "until" {
		out, ok := response.Output.(map[string]any)
		if !ok {
			return Result{}, fmt.Errorf("loop until: child output must be {done,value}")
		}
		done, ok := out["done"].(bool)
		if !ok {
			return Result{}, fmt.Errorf("loop until: done must be a boolean")
		}
		value, exists := out["value"]
		if !exists {
			return Result{}, fmt.Errorf("loop until: value is required")
		}
		state.Value = value
		if done {
			return Result{Output: value}, nil
		}
	} else {
		state.Results = append(state.Results, response.Output)
	}
	return n.next(input, state)
}
