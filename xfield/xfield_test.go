package xfield_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
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

func registerResource(t *testing.T, registry *xfield.Registry[*dependencies]) {
	t.Helper()
	err := registry.Register(func() xfield.Field[*dependencies] {
		return &resourceField{}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExternalFieldNestedRoundTrip(t *testing.T) {
	registry := xfield.DefaultRegistry[*dependencies]()
	registerResource(t, registry)
	raw := []byte(`[{"key":"assets","type":"array","items":{"type":"object","properties":[{"key":"image","type":"resource","required":true}]}}]`)
	schema, err := registry.DecodeSchema(raw)
	if err != nil {
		t.Fatal(err)
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
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := registry.DecodeSchema(encoded)
	if err != nil || !xfield.FieldEqual(schema[0].Value(), decoded[0].Value()) {
		t.Fatalf("round trip: %s, %v", encoded, err)
	}
	if _, err := xfield.DefaultRegistry[*dependencies]().DecodeSchema(raw); !errors.Is(err, xfield.ErrUnknownType) {
		t.Fatalf("registry leaked registration: %v", err)
	}
}

func TestBuiltinsJSONAndRules(t *testing.T) {
	var schema xfield.Schema[struct{}]
	raw := `[{"key":"name","type":"string","minLength":2,"maxLength":4,"pattern":"^[一二三]+$","enum":["一二"]},{"key":"n","type":"number","min":0,"max":100,"enum":[10]},{"key":"ok","type":"boolean"}]`
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
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
	encoded, err := json.Marshal(schema)
	if err != nil || !strings.Contains(string(encoded), `"type":"string"`) {
		t.Fatalf("flat encoding %s: %v", encoded, err)
	}
}

func TestRequiredPartialAndSchemaErrors(t *testing.T) {
	schema := xfield.Schema[struct{}]{xfield.Wrap(&xfield.StringField[struct{}]{Meta: xfield.Meta{Key: "name", Required: true}})}
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
		{xfield.Wrap(&xfield.StringField[struct{}]{})},
		{schema[0], schema[0]},
		{xfield.Wrap(&xfield.ArrayField[struct{}]{Meta: xfield.Meta{Key: "a"}})},
		{xfield.Wrap(&xfield.StringField[struct{}]{Meta: xfield.Meta{Key: "s"}, MinLength: &minusOne})},
		{xfield.Wrap(&xfield.StringField[struct{}]{Meta: xfield.Meta{Key: "s"}, MinLength: &min, MaxLength: &max})},
		{xfield.Wrap(&xfield.StringField[struct{}]{Meta: xfield.Meta{Key: "s"}, Pattern: &badPattern})},
		{xfield.Wrap((*xfield.StringField[struct{}])(nil))},
	}
	for _, bad := range cases {
		if _, err := xfield.ValidateModel(nil, bad, struct{}{}); !errors.Is(err, xfield.ErrInvalidSchema) {
			t.Fatalf("bad schema %v: %v", bad, err)
		}
	}
	cycle := &xfield.ArrayField[struct{}]{Meta: xfield.Meta{Key: "cycle"}}
	cycle.Items = xfield.Wrap(cycle)
	if err := xfield.ValidateField(cycle); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("cycle: %v", err)
	}
}

func TestNestedPathPreservesExternalError(t *testing.T) {
	sentinel := errors.New("upload unavailable")
	deps := &dependencies{ctx: context.Background(), err: sentinel}
	schema := xfield.Schema[*dependencies]{xfield.Wrap(&xfield.ArrayField[*dependencies]{
		Meta: xfield.Meta{Key: "assets"},
		Items: xfield.Wrap(&xfield.ObjectField[*dependencies]{Properties: xfield.Schema[*dependencies]{
			xfield.Wrap(&resourceField{Meta: xfield.Meta{Key: "image"}}),
		}}),
	})}
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
	registry := xfield.DefaultRegistry[struct{}]()
	schema, err := registry.DecodeSchema([]byte(`[{"type":"number","key":"id","enum":[9007199254740993]}]`))
	if err != nil {
		t.Fatal(err)
	}
	field := schema[0].Value()
	if _, err := field.Parse(uint64(9007199254740993), struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := field.Parse(uint64(9007199254740992), struct{}{}); !errors.Is(err, xfield.ErrInvalidValue) {
		t.Fatalf("rounded integer accepted: %v", err)
	}
	a := &xfield.NumberField[struct{}]{Enum: []any{int64(9007199254740993)}}
	b := &xfield.NumberField[struct{}]{Enum: []any{int64(9007199254740992)}}
	if xfield.FieldEqual(a, b) {
		t.Fatal("different large integers compare equal")
	}
	for _, invalid := range []json.Number{"1e999999999", "NaN", "true", "1 2"} {
		if _, err := (&xfield.NumberField[struct{}]{}).Parse(invalid, struct{}{}); err == nil {
			t.Fatalf("invalid number %q accepted", invalid)
		}
	}
}

func TestRegistryConcurrencyAndLimits(t *testing.T) {
	registry := xfield.DefaultRegistry[*dependencies]()
	registerResource(t, registry)
	if err := registry.Register(func() xfield.Field[*dependencies] {
		return &resourceField{}
	}); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("duplicate registration: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			name := fmt.Sprintf("resource%d", i)
			err := registry.Register(func() xfield.Field[*dependencies] {
				return &namedResource{typeName: name}
			})
			if err != nil {
				t.Error(err)
			}
			if _, err := registry.DecodeField(json.RawMessage(`{"type":"resource"}`)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	raw := `{"type":"string"}`
	for range xfield.MaxSchemaDepth {
		raw = `{"type":"array","items":` + raw + `}`
	}
	if _, err := registry.DecodeField(json.RawMessage(raw)); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("depth limit: %v", err)
	}
	if field, err := registry.DecodeField(json.RawMessage(`{"type":"resource0"}`)); err != nil || field.Type() != "resource0" {
		t.Fatalf("concurrent registration: %v, %v", field, err)
	}
}

type namedResource struct {
	resourceField
	typeName string
}

func (f *namedResource) Type() string { return f.typeName }

func TestNamedContainers(t *testing.T) {
	type namedMap map[string]any
	type namedSlice []any
	field := &xfield.ObjectField[struct{}]{Properties: xfield.Schema[struct{}]{
		xfield.Wrap(&xfield.ArrayField[struct{}]{Meta: xfield.Meta{Key: "items"}, Items: xfield.Wrap(&xfield.NumberField[struct{}]{})}),
	}}
	got, err := field.Parse(namedMap{"items": namedSlice{int8(1), uint16(2)}, "extra": true}, struct{}{})
	want := map[string]any{"items": []any{int8(1), uint16(2)}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("named containers: %v, %v", got, err)
	}
}
