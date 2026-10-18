package dflow

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func (p Parameters) resolveWithInput(infra Infrastructure, ctx context.Context, states map[string]NodeState) (map[string]any, error) {
	resolved := make(Parameters, len(p))
	for key, parameter := range p {
		if parameter.Type == "ref" {
			path, err := referencePath(parameter.Content)
			if err != nil {
				return nil, err
			}
			if path[0] == InputSource {
				value, err := infra.GetInput(ctx, path[1:]...)
				if err != nil {
					return nil, &InfrastructureError{"GetInput", err}
				}
				parameter = Constant(value)
			}
		} else if parameter.Type == "template" || parameter.Type == "plain-template" {
			text, ok := parameter.Content.(string)
			if !ok {
				return nil, ErrInvalidReference
			}
			var failure error
			parameter.Content = referencePattern.ReplaceAllStringFunc(text, func(match string) string {
				path := strings.Split(strings.TrimSpace(referencePattern.FindStringSubmatch(match)[1]), ".")
				var value any
				var err error
				if path[0] == InputSource {
					value, err = infra.GetInput(ctx, path[1:]...)
				} else {
					value, err = resolvePath(path, nil, states)
				}
				if err != nil {
					failure = err
					if path[0] == InputSource {
						failure = &InfrastructureError{"GetInput", err}
					}
					return ""
				}
				if parameter.Type == "plain-template" {
					if value == nil {
						return ""
					}
					return fmt.Sprint(value)
				}
				raw, err := json.Marshal(value)
				if err != nil {
					failure = err
				}
				return string(raw)
			})
			if failure != nil {
				return nil, failure
			}
			// Freeze rendered input fragments as constants so user data containing
			// {{...}} cannot be interpreted a second time as a node reference.
			parameter = Constant(parameter.Content)
		}
		resolved[key] = parameter
	}
	return resolved.resolve(nil, states)
}

const InputSource = "$input"

// InputValue resolves an original input path for Infrastructure adapters.
func InputValue(input any, path ...string) (any, error) {
	value, err := resolvePath(append([]string{InputSource}, path...), input, nil)
	if err != nil {
		return nil, err
	}
	return cloneJSON(value)
}

// Parameter retains flowgram's constant/ref/template tagged value model.
// A ref content is [nodeID, objectKey, arrayIndex, ...]. "$input" selects run input.
type Parameter struct {
	Type    string `json:"type"`
	Content any    `json:"content"`
}

type Parameters map[string]Parameter

func Constant(value any) Parameter { return Parameter{Type: "constant", Content: value} }
func Ref(nodeID string, path ...string) Parameter {
	return Parameter{Type: "ref", Content: append([]string{nodeID}, path...)}
}
func Input(path ...string) Parameter { return Ref(InputSource, path...) }

// Template inserts JSON literals, including quotes around strings.
func Template(text string) Parameter { return Parameter{Type: "template", Content: text} }

// PlainTemplate inserts strings verbatim; use for URLs or human-readable text.
func PlainTemplate(text string) Parameter { return Parameter{Type: "plain-template", Content: text} }

var referencePattern = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

func referencePath(content any) ([]string, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	var path []string
	if err := json.Unmarshal(raw, &path); err != nil || len(path) == 0 || path[0] == "" {
		return nil, fmt.Errorf("%w: ref requires a nonempty string path", ErrInvalidReference)
	}
	return path, nil
}

func (p Parameter) references() ([][]string, error) {
	switch p.Type {
	case "constant":
		return nil, nil
	case "ref":
		path, err := referencePath(p.Content)
		return [][]string{path}, err
	case "template", "plain-template":
		text, ok := p.Content.(string)
		if !ok {
			return nil, fmt.Errorf("%w: template content must be a string", ErrInvalidReference)
		}
		var refs [][]string
		for _, match := range referencePattern.FindAllStringSubmatch(text, -1) {
			path := strings.Split(strings.TrimSpace(match[1]), ".")
			for _, segment := range path {
				if segment == "" {
					return nil, fmt.Errorf("%w: empty template path segment", ErrInvalidReference)
				}
			}
			refs = append(refs, path)
		}
		return refs, nil
	default:
		return nil, fmt.Errorf("%w: unknown parameter type %q", ErrInvalidDefinition, p.Type)
	}
}

func resolvePath(path []string, input any, states map[string]NodeState) (any, error) {
	value := input
	if path[0] != InputSource {
		state, ok := states[path[0]]
		if !ok || state.Status != Succeeded {
			return nil, fmt.Errorf("%w: node %q has no successful output", ErrMissingValue, path[0])
		}
		value = state.Output
	}
	for _, part := range path[1:] {
		switch current := value.(type) {
		case map[string]any:
			var exists bool
			value, exists = current[part]
			if !exists {
				return nil, fmt.Errorf("%w: %v", ErrMissingValue, path)
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(current) {
				return nil, fmt.Errorf("%w: %v", ErrMissingValue, path)
			}
			value = current[index]
		default:
			return nil, fmt.Errorf("%w: %v", ErrMissingValue, path)
		}
	}
	return value, nil
}

func (p Parameter) resolve(input any, states map[string]NodeState) (any, error) {
	switch p.Type {
	case "constant":
		return p.Content, nil
	case "ref":
		path, err := referencePath(p.Content)
		if err != nil {
			return nil, err
		}
		return resolvePath(path, input, states)
	case "template", "plain-template":
		text := p.Content.(string) // Compile has validated every parameter.
		var resolveErr error
		rendered := referencePattern.ReplaceAllStringFunc(text, func(match string) string {
			if resolveErr != nil {
				return ""
			}
			expression := referencePattern.FindStringSubmatch(match)[1]
			value, err := resolvePath(strings.Split(strings.TrimSpace(expression), "."), input, states)
			if err != nil {
				resolveErr = err
				return ""
			}
			if p.Type == "plain-template" {
				if value == nil {
					return ""
				}
				return fmt.Sprint(value)
			}
			raw, err := json.Marshal(value)
			resolveErr = err
			return string(raw)
		})
		return rendered, resolveErr
	default:
		return nil, ErrInvalidDefinition
	}
}

func (p Parameters) resolve(input any, states map[string]NodeState) (map[string]any, error) {
	result := make(map[string]any, len(p))
	keys := make([]string, 0, len(p))
	for key := range p {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := p[key].resolve(input, states)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", key, err)
		}
		result[key] = value
	}
	// Isolate each node from upstream outputs and sibling inputs.
	return cloneJSON(result)
}

func cloneJSON[T any](value T) (T, error) {
	var result T
	raw, err := json.Marshal(value)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(raw, &result)
	return result, err
}
