package dflow

import (
	"context"
	"encoding/json"
	"fmt"
)

type JSONParseNode struct{}

func (*JSONParseNode) Run(_ context.Context, input map[string]any) (Result, error) {
	text, ok := input["json"].(string)
	if !ok {
		return Result{}, fmt.Errorf("json input must be a string")
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		return Result{}, err
	}
	if output == nil {
		return Result{}, fmt.Errorf("json input must be an object")
	}
	return Result{Output: output}, nil
}
