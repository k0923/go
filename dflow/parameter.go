package dflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const InputSource = "$input"

// ValueResolver reads a run input or an ancestor's output by path.
type ValueResolver interface {
	Resolve(context.Context, ...string) (any, error)
}

// Parameter declares every dependency before compilation and resolves a value
// during execution. Implementations and returned values must be immutable.
type Parameter interface {
	References() ([][]string, error)
	Resolve(context.Context, ValueResolver) (any, error)
}

type Parameters map[string]Parameter

type ConstantParameter struct{ Value any }
type ReferenceParameter struct{ Path []string }
type TemplateParameter struct {
	Text string
	// JSON inserts JSON literals; false inserts plain text.
	JSON bool
}

func Constant(value any) Parameter { return ConstantParameter{Value: value} }
func Ref(nodeID string, path ...string) Parameter {
	return ReferenceParameter{Path: append([]string{nodeID}, path...)}
}
func Input(path ...string) Parameter      { return Ref(InputSource, path...) }
func Template(text string) Parameter      { return TemplateParameter{Text: text, JSON: true} }
func PlainTemplate(text string) Parameter { return TemplateParameter{Text: text} }

func (p ConstantParameter) References() ([][]string, error) { return nil, nil }
func (p ConstantParameter) Resolve(ctx context.Context, _ ValueResolver) (any, error) {
	return p.Value, ctx.Err()
}
func (p ReferenceParameter) References() ([][]string, error) {
	if err := validatePath(p.Path); err != nil {
		return nil, err
	}
	return [][]string{append([]string(nil), p.Path...)}, nil
}
func (p ReferenceParameter) Resolve(ctx context.Context, resolver ValueResolver) (any, error) {
	if err := validatePath(p.Path); err != nil {
		return nil, err
	}
	return resolver.Resolve(ctx, p.Path...)
}

var referencePattern = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

func validatePath(path []string) error {
	if len(path) == 0 || path[0] == "" {
		return fmt.Errorf("%w: reference requires a source", ErrInvalidReference)
	}
	return nil
}

func (p TemplateParameter) References() ([][]string, error) {
	var refs [][]string
	for _, match := range referencePattern.FindAllStringSubmatch(p.Text, -1) {
		path := strings.Split(strings.TrimSpace(match[1]), ".")
		for _, segment := range path {
			if segment == "" {
				return nil, fmt.Errorf("%w: empty template path segment", ErrInvalidReference)
			}
		}
		refs = append(refs, path)
	}
	return refs, nil
}

func (p TemplateParameter) Resolve(ctx context.Context, resolver ValueResolver) (any, error) {
	if _, err := p.References(); err != nil {
		return nil, err
	}
	var failure error
	text := referencePattern.ReplaceAllStringFunc(p.Text, func(match string) string {
		if failure != nil {
			return ""
		}
		path := strings.Split(strings.TrimSpace(referencePattern.FindStringSubmatch(match)[1]), ".")
		value, err := resolver.Resolve(ctx, path...)
		if err != nil {
			failure = err
			return ""
		}
		if !p.JSON {
			if value == nil {
				return ""
			}
			return fmt.Sprint(value)
		}
		raw, err := json.Marshal(value)
		failure = err
		return string(raw)
	})
	return text, failure
}

type inputResolver struct {
	infra  Infrastructure
	states map[string]NodeState
}

func (r inputResolver) Resolve(ctx context.Context, path ...string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePath(path); err != nil {
		return nil, err
	}
	if path[0] == InputSource {
		value, err := r.infra.GetInput(ctx, path[1:]...)
		if err != nil {
			return nil, &InfrastructureError{"GetInput", err}
		}
		return value, nil
	}
	state, ok := r.states[path[0]]
	if !ok || state.Status != Succeeded {
		return nil, fmt.Errorf("%w: node %q has no successful output", ErrMissingValue, path[0])
	}
	return InputValue(state.Output, path[1:]...)
}

func (p Parameters) resolveWithInput(infra Infrastructure, ctx context.Context, states map[string]NodeState) (map[string]any, error) {
	result := make(map[string]any, len(p))
	keys := make([]string, 0, len(p))
	for key := range p {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if isNil(p[key]) {
			return nil, ErrInvalidReference
		}
		value, err := p[key].Resolve(ctx, inputResolver{infra, states})
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", key, err)
		}
		result[key] = value
	}
	return result, nil
}

// InputValue traverses string-keyed maps, slices/arrays and exported struct
// fields by Go field name. It preserves Go types and returns a read-only value.
func InputValue(input any, path ...string) (any, error) {
	value := reflect.ValueOf(input)
	for _, part := range path {
		for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
			value = value.Elem()
		}
		if !value.IsValid() {
			return nil, ErrMissingValue
		}
		switch value.Kind() {
		case reflect.Map:
			if value.Type().Key().Kind() != reflect.String {
				return nil, ErrMissingValue
			}
			value = value.MapIndex(reflect.ValueOf(part).Convert(value.Type().Key()))
		case reflect.Slice, reflect.Array:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= value.Len() {
				return nil, ErrMissingValue
			}
			value = value.Index(index)
		case reflect.Struct:
			value = value.FieldByName(part)
		default:
			return nil, ErrMissingValue
		}
		if !value.IsValid() || !value.CanInterface() {
			return nil, ErrMissingValue
		}
	}
	if !value.IsValid() {
		return nil, nil
	}
	return value.Interface(), nil
}
