package nodes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/k0923/go/dflow"
)

type JSONParseNode struct{}

func (*JSONParseNode) Run(_ context.Context, input map[string]any) (dflow.Result, error) {
	text, ok := input["json"].(string)
	if !ok {
		return dflow.Result{}, fmt.Errorf("json input must be a string")
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		return dflow.Result{}, err
	}
	if output == nil {
		return dflow.Result{}, fmt.Errorf("json input must be an object")
	}
	return dflow.Result{Output: output}, nil
}
