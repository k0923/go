package dflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

func decodeConfig(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("node config must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing config data")
	}
	return nil
}

func passFactory(raw json.RawMessage) (Node, error) {
	if err := decodeConfig(raw, &struct{}{}); err != nil {
		return nil, err
	}
	return NodeFunc(func(_ context.Context, input map[string]any) (Result, error) {
		return Result{Output: input}, nil
	}), nil
}

func jsonParseFactory(raw json.RawMessage) (Node, error) {
	if err := decodeConfig(raw, &struct{}{}); err != nil {
		return nil, err
	}
	return NodeFunc(func(_ context.Context, input map[string]any) (Result, error) {
		text, ok := input["json"].(string)
		if !ok {
			return Result{}, fmt.Errorf("json input must be a string")
		}
		var output any
		if err := json.Unmarshal([]byte(text), &output); err != nil {
			return Result{}, err
		}
		return Result{Output: output}, nil
	}), nil
}

// condition uses a resolved boolean input and selects "true" or "false" ports.
// Business predicates can be supplied by a code/custom predecessor.
func conditionFactory(raw json.RawMessage) (Node, error) {
	if err := decodeConfig(raw, &struct{}{}); err != nil {
		return nil, err
	}
	return NodeFunc(func(_ context.Context, input map[string]any) (Result, error) {
		value, ok := input["condition"].(bool)
		if !ok {
			return Result{}, fmt.Errorf("condition input must be a boolean")
		}
		port := "false"
		if value {
			port = "true"
		}
		return Result{Output: map[string]any{"matchedKey": port}, Ports: []string{port}}, nil
	}), nil
}
