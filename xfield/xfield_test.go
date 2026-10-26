package xfield_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/k0923/go/xfield"
)

// This type deliberately lives outside package xfield.
type resourceField struct {
	xfield.Meta
}

func (*resourceField) Type() string { return "resource" }
func (*resourceField) Parse(value any, deps *dependencies) (any, error) {
	if value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, errors.New("resource must be a string")
	}
	if deps != nil {
		if deps.ctx == nil {
			return nil, errors.New("context was not propagated")
		}
		deps.calls++
		if deps.err != nil {
			return nil, deps.err
		}
	}
	return "stored:" + text, nil
}
func (*resourceField) Present(value any, deps *dependencies) any {
	if value == nil {
		return nil
	}
	prefix := ""
	if deps != nil {
		prefix = deps.displayPrefix
		deps.presentCalls++
	}
	return prefix + strings.TrimPrefix(fmt.Sprint(value), "stored:")
}

type dependencies struct {
	ctx           context.Context
	calls         int
	err           error
	displayPrefix string
	presentCalls  int
}

func TestExternalFieldNested(t *testing.T) {
	schema := xfield.Schema[*dependencies]{

		"assets": &xfield.ArrayField[*dependencies]{
			Meta: xfield.Meta{},
			Items: &xfield.ObjectField[*dependencies]{
				Properties: xfield.Schema[*dependencies]{
					"image": &resourceField{Meta: xfield.Meta{Required: true}},
				},
			},
		},
	}
	if err := xfield.ValidateSchema(schema); err != nil {
		t.Fatal(err)
	}
	field, err := xfield.FindFieldByPath(schema, "assets.0.image")
	if err != nil || xfield.TypeOf(field) != "resource" || !xfield.RequiredOf(field) {
		t.Fatalf("external field traversal: %v, %v", field, err)
	}
	ctx := context.WithValue(context.Background(), struct{}{}, "request")
	deps := &dependencies{ctx: ctx}
	input := map[string]any{"assets": []any{map[string]any{"image": "url", "extra": 1}}, "extra": true}
	out, err := xfield.ValidateModel(input, schema, deps)
	if err != nil {
		t.Fatal(err)
	}
	if deps.calls != 1 || xfield.GetData(out, []string{"assets", "0", "image"}) != "stored:url" {
		t.Fatalf("parse result %v, calls %d", out, deps.calls)
	}
	if _, exists := out["extra"]; exists {
		t.Fatal("unknown top-level field was retained")
	}
	deps.displayPrefix = "https://resources/"
	presented := xfield.PresentModel(out, schema, deps)
	if xfield.GetData(presented, []string{"assets", "0", "image"}) != "https://resources/url" || deps.presentCalls != 1 {
		t.Fatalf("present result: %v", presented)
	}
	if xfield.GetData(input, []string{"assets", "0", "image"}) != "url" ||
		xfield.GetData(out, []string{"assets", "0", "image"}) != "stored:url" {
		t.Fatal("parse or present mutated input")
	}
}

func TestBuiltinsRules(t *testing.T) {
	minLength, maxLength, pattern := 2, 4, "^[一二三]+$"
	min, max := 0.0, 100.0
	schema := xfield.Schema[struct{}]{

		"name": &xfield.StringField[struct{}]{
			Meta: xfield.Meta{}, MinLength: &minLength, MaxLength: &maxLength,
			Pattern: &pattern, Enum: []string{"一二"},
		},
		"n":  &xfield.NumberField[struct{}]{Meta: xfield.Meta{}, Min: &min, Max: &max, Enum: []any{10}},
		"ok": &xfield.BooleanField[struct{}]{Meta: xfield.Meta{}},
	}
	out, err := xfield.ValidateModel(map[string]any{"name": "一二", "n": uint8(10), "ok": true}, schema, struct{}{})
	if err != nil || out["n"] != uint8(10) {
		t.Fatalf("valid values: %v, %v", out, err)
	}
	for _, input := range []map[string]any{
		{"name": "一"}, {"name": "一二三"}, {"name": "ab"},
		{"n": 11}, {"n": math.NaN()}, {"n": math.Inf(1)}, {"ok": "true"},
	} {
		if _, err := xfield.ValidateModel(input, schema, struct{}{}); !errors.Is(err, xfield.ErrInvalidValue) {
			t.Fatalf("input %v: %v", input, err)
		}
	}
}

func TestRequiredPartialAndSchemaErrors(t *testing.T) {
	schema := xfield.Schema[struct{}]{"name": &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}}}
	if _, err := xfield.ValidateModel(map[string]any{}, schema, struct{}{}); !errors.Is(err, xfield.ErrInvalidValue) {
		t.Fatal(err)
	}
	if _, err := xfield.ValidateModel(map[string]any{"name": nil}, schema, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := xfield.ValidatePartial(map[string]any{}, schema, struct{}{}); err != nil {
		t.Fatal(err)
	}
	minusOne, min, max := -1, 5, 2
	badPattern := "["
	cases := []xfield.Schema[struct{}]{
		{"": &xfield.StringField[struct{}]{}},
		{"a": &xfield.ArrayField[struct{}]{}},
		{"s": &xfield.StringField[struct{}]{MinLength: &minusOne}},
		{"s": &xfield.StringField[struct{}]{MinLength: &min, MaxLength: &max}},
		{"s": &xfield.StringField[struct{}]{Pattern: &badPattern}},
		{"s": (*xfield.StringField[struct{}])(nil)},
	}
	for _, bad := range cases {
		if _, err := xfield.ValidateModel(nil, bad, struct{}{}); !errors.Is(err, xfield.ErrInvalidSchema) {
			t.Fatalf("bad schema %v: %v", bad, err)
		}
	}
	cycle := &xfield.ArrayField[struct{}]{Meta: xfield.Meta{}}
	cycle.Items = cycle
	if err := xfield.ValidateField(cycle); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("cycle: %v", err)
	}
}

func TestNestedPathPreservesExternalError(t *testing.T) {
	sentinel := errors.New("upload unavailable")
	deps := &dependencies{ctx: context.Background(), err: sentinel}
	schema := xfield.Schema[*dependencies]{
		"assets": &xfield.ArrayField[*dependencies]{
			Meta: xfield.Meta{},
			Items: &xfield.ObjectField[*dependencies]{Properties: xfield.Schema[*dependencies]{
				"image": &resourceField{Meta: xfield.Meta{}},
			}},
		}}
	_, err := xfield.ValidateModel(
		map[string]any{"assets": []any{map[string]any{"image": "url"}}},
		schema, deps,
	)
	var pathError *xfield.PathError
	if !errors.Is(err, sentinel) || !errors.As(err, &pathError) || pathError.Path != "assets[0].image" {
		t.Fatalf("path error: %v", err)
	}
}

func TestLargeNumberPrecision(t *testing.T) {
	field := &xfield.NumberField[struct{}]{Enum: []any{json.Number("9007199254740993")}}
	if _, err := field.Parse(uint64(9007199254740993), struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := field.Parse(uint64(9007199254740992), struct{}{}); !errors.Is(err, xfield.ErrInvalidValue) {
		t.Fatalf("rounded integer accepted: %v", err)
	}
	for _, invalid := range []json.Number{"1e999999999", "NaN", "true", "1 2"} {
		if _, err := (&xfield.NumberField[struct{}]{}).Parse(invalid, struct{}{}); err == nil {
			t.Fatalf("invalid number %q accepted", invalid)
		}
	}
}

func TestSchemaDepthLimit(t *testing.T) {
	var field xfield.Field[struct{}] = &xfield.StringField[struct{}]{}
	for range xfield.MaxSchemaDepth {
		field = &xfield.ArrayField[struct{}]{Items: field}
	}
	if err := xfield.ValidateField(field); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("depth limit: %v", err)
	}
}

func TestNamedContainers(t *testing.T) {
	type namedMap map[string]any
	type namedSlice []any
	field := &xfield.ObjectField[struct{}]{Properties: xfield.Schema[struct{}]{
		"items": &xfield.ArrayField[struct{}]{Meta: xfield.Meta{}, Items: &xfield.NumberField[struct{}]{}},
	}}
	got, err := field.Parse(namedMap{"items": namedSlice{int8(1), uint16(2)}, "extra": true}, struct{}{})
	want := map[string]any{"items": []any{int8(1), uint16(2)}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("named containers: %v, %v", got, err)
	}
}
