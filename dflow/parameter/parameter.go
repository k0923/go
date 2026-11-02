package parameter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	ErrInvalidReference = errors.New("invalid output reference")
	ErrMissingValue     = errors.New("referenced value is missing")
	ErrParameterType    = errors.New("parameter type mismatch")
)

type ParameterTypeError struct {
	Path     []string
	Expected reflect.Type
	Actual   reflect.Type
}

func (e *ParameterTypeError) Error() string {
	return fmt.Sprintf("parameter %q: expected %v, got %v", e.Path, e.Expected, e.Actual)
}

func (e *ParameterTypeError) Unwrap() error { return ErrParameterType }

// ValueResolver reads a run input or an ancestor's output by path. Business
// runtime APIs implement this interface directly. Missing/skipped values return
// ErrMissingValue.
//
// ValueResolver 按路径读取执行输入或祖先节点的输出。
type ValueResolver interface {
	Resolve(context.Context, ...string) (any, error)
}

// Parameter returns a typed value during execution. Implementations and returned
// values must be immutable.
type Parameter[T any] interface {
	Get(context.Context, ValueResolver) (T, error)
}

type ConstParameter[T any] struct{ Value T }
type RefParameter[T any] struct{ Path []string }
type TemplateParameter struct {
	Text string
	// JSON inserts JSON literals; false inserts plain text.
	//
	// JSON 为 true 时插入 JSON 字面量；为 false 时插入纯文本。
	JSON bool
}

func Const[T any](value T) Parameter[T]    { return ConstParameter[T]{Value: value} }
func Constant[T any](value T) Parameter[T] { return Const(value) }
func Ref[T any](nodeID string, path ...string) Parameter[T] {
	return RefParameter[T]{Path: append([]string{nodeID}, path...)}
}
func Template(text string) Parameter[string] {
	return TemplateParameter{Text: text, JSON: true}
}
func PlainTemplate(text string) Parameter[string] { return TemplateParameter{Text: text} }

func (p ConstParameter[T]) Get(ctx context.Context, _ ValueResolver) (T, error) {
	return p.Value, ctx.Err()
}

func (p RefParameter[T]) Get(ctx context.Context, resolver ValueResolver) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := validatePath(p.Path); err != nil {
		return zero, err
	}
	if isNil(resolver) {
		return zero, ErrInvalidReference
	}
	value, err := resolver.Resolve(ctx, p.Path...)
	if err != nil {
		return zero, err
	}
	if value == nil {
		switch reflect.TypeFor[T]().Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
			return zero, nil
		}
	}
	typed, ok := value.(T)
	if !ok {
		return zero, &ParameterTypeError{
			Path: slices.Clone(p.Path), Expected: reflect.TypeFor[T](), Actual: reflect.TypeOf(value),
		}
	}
	return typed, nil
}

var referencePattern = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

func validatePath(path []string) error {
	if len(path) == 0 || path[0] == "" {
		return fmt.Errorf("%w: reference requires a source", ErrInvalidReference)
	}
	return nil
}

func (p TemplateParameter) Get(ctx context.Context, resolver ValueResolver) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if isNil(resolver) {
		return "", ErrInvalidReference
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

// Values is a read-only, in-memory resolver for preloaded source outputs.
// Keys are node IDs. Omit skipped sources; present nil values are valid.
// Business APIs may embed Values alongside their own capabilities.
type Values map[string]map[string]any

var _ ValueResolver = Values(nil)

func (v Values) Resolve(ctx context.Context, path ...string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePath(path); err != nil {
		return nil, err
	}
	output, exists := v[path[0]]
	if !exists {
		return nil, fmt.Errorf("%w: source %q", ErrMissingValue, path[0])
	}
	return InputValue(output, path[1:]...)
}

// InputValue traverses string-keyed maps, slices/arrays and exported struct
// fields by Go field name. It preserves Go types and returns a read-only value.
//
// InputValue 按 Go 字段名遍历 string key 的 map、slice/array 以及导出的
// struct 字段。它保留 Go 类型，并返回一个只读值。
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

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}
