package dflow

import (
	"context"
	"encoding/json"
	"fmt"
)

type JSONParseNode struct{}

func (*JSONParseNode) Type() string { return "json-parse" }
func (*JSONParseNode) Run(_ context.Context, input map[string]any) (Result, error) {
	text, ok := input["json"].(string)
	if !ok {
		return Result{}, fmt.Errorf("json input must be a string")
	}
	var output any
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		return Result{}, err
	}
	return Result{Output: output}, nil
}
