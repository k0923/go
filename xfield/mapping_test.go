package xfield_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/k0923/go/xfield"
)

type customMapping struct {
	target  []string
	resolve func(any) (any, error)
}

func (m customMapping) Target() []string               { return m.target }
func (m customMapping) Resolve(input any) (any, error) { return m.resolve(input) }
func (customMapping) Children() []xfield.Mapping       { return nil }

var _ xfield.Mapping = customMapping{}

func TestMappingsNestedParseOnce(t *testing.T) {
	schema := schemaMap[*dependencies]{
		"teams": &xfield.ArrayField[*dependencies]{
			Items: &xfield.ObjectField[*dependencies]{Properties: schemaMap[*dependencies]{
				"name": &xfield.StringField[*dependencies]{Meta: xfield.Meta{Required: true}},
				"images": &xfield.ArrayField[*dependencies]{
					Items: &xfield.ObjectField[*dependencies]{Properties: schemaMap[*dependencies]{
						"url": &resourceField{Meta: xfield.Meta{Required: true}},
					}},
				},
			}},
		},
	}
	mappings := []xfield.Mapping{
		xfield.PathMapping([]string{"groups"}, []string{"teams"},
			xfield.PathMapping([]string{"title"}, []string{"name"}),
			xfield.PathMapping([]string{"assets"}, []string{"images"},
				xfield.PathMapping([]string{"link"}, []string{"url"}),
			),
		),
	}
	input := map[string]any{"groups": []any{
		map[string]any{"title": "A", "assets": []any{map[string]any{"link": "a"}}},
		map[string]any{"title": "B", "assets": []any{map[string]any{"link": "b"}}},
	}}
	deps := &dependencies{ctx: context.Background()}
	out, err := xfield.ApplyMappings(input, mappings, schema, deps)
	if err != nil {
		t.Fatal(err)
	}
	if deps.calls != 2 {
		t.Fatalf("each resource must parse once, got %d calls", deps.calls)
	}
	if value := xfield.GetData(out, []string{"teams", "1", "images", "0", "url"}); value != "stored:b" {
		t.Fatalf("nested mapping value: %v", value)
	}
}

func TestMappingsNullCustomResolverAndRawJSON(t *testing.T) {
	schema := schemaMap[struct{}]{
		"name": &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}},
		"id":   &xfield.NumberField[struct{}]{},
	}
	mappings := []xfield.Mapping{
		xfield.PathMapping([]string{"name"}, []string{"name"}),
		xfield.PathMapping([]string{"id"}, []string{"id"}),
	}
	out, err := xfield.ApplyMappings(`{"name":null,"id":9007199254740993}`, mappings, schema, struct{}{})
	if err != nil || out["name"] != nil || out["id"] != json.Number("9007199254740993") {
		t.Fatalf("null/number: %v, %v", out, err)
	}
	out, err = xfield.ApplyMappings(map[string]any{}, mappings, schema, struct{}{})
	if err != nil || len(out) != 2 || out["name"] != nil || out["id"] != nil {
		t.Fatalf("missing path should resolve to nil: %v, %v", out, err)
	}
	mappings = []xfield.Mapping{xfield.MappingFunc(
		[]string{"name"},
		func(input any) (any, error) {
			if input == nil {
				t.Fatal("resolver did not receive normalized input")
			}
			return "resolved", nil
		},
	)}
	out, err = xfield.ApplyMappings(map[string]any{}, mappings, schema, struct{}{})
	if err != nil || out["name"] != "resolved" {
		t.Fatalf("custom resolver: %v, %v", out, err)
	}
	for _, raw := range []string{`{`, `{} {}`} {
		if _, err := xfield.ApplyMappings(raw, mappings, schema, struct{}{}); err == nil {
			t.Fatalf("invalid JSON accepted: %s", raw)
		}
	}
}

func TestMappingsDoNotMutateInput(t *testing.T) {
	schema := schemaMap[struct{}]{
		"profile": &xfield.ObjectField[struct{}]{
			Properties: schemaMap[struct{}]{
				"name": &xfield.StringField[struct{}]{},
			},
		}}
	input := map[string]any{"profile": map[string]any{"name": "old"}, "replacement": "new"}
	mappings := []xfield.Mapping{
		xfield.PathMapping([]string{"profile"}, []string{"profile"}),
		xfield.PathMapping([]string{"replacement"}, []string{"profile", "name"}),
	}
	out, err := xfield.ApplyMappings(input, mappings, schema, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if xfield.GetData(input, []string{"profile", "name"}) != "old" ||
		xfield.GetData(out, []string{"profile", "name"}) != "new" {
		t.Fatalf("input %v output %v", input, out)
	}
}

func TestMappingInterfaceAndPathSegments(t *testing.T) {
	schema := schemaMap[struct{}]{
		"a.b":  &xfield.StringField[struct{}]{},
		"name": &xfield.StringField[struct{}]{},
	}
	sourcePath := []string{"source"}
	targetPath := []string{"name"}
	pathMapping := xfield.PathMapping(sourcePath, targetPath)
	sourcePath[0] = "other"
	targetPath[0] = "other"
	returnedTarget := pathMapping.Target()
	returnedTarget[0] = "other"

	mappings := []xfield.Mapping{
		customMapping{
			target: []string{"a.b"},
			resolve: func(any) (any, error) {
				return "literal segment", nil
			},
		},
		pathMapping,
	}
	out, err := xfield.ApplyMappings(
		map[string]any{"source": "cloned paths"},
		mappings,
		schema,
		struct{}{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out["a.b"] != "literal segment" || out["name"] != "cloned paths" {
		t.Fatalf("mapping interface/path segments: %v", out)
	}
}

func TestMappingsOpenObjectAcceptsFullValue(t *testing.T) {
	schema := schemaMap[struct{}]{
		"profile": &xfield.ObjectField[struct{}]{},
	}
	input := map[string]any{"source": map[string]any{
		"name": "Alice",
		"meta": map[string]any{"role": "admin"},
		"tags": []any{"owner"},
	}}
	out, err := xfield.ApplyMappings(input,
		[]xfield.Mapping{xfield.PathMapping([]string{"source"}, []string{"profile"})},
		schema, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"profile": map[string]any{
		"name": "Alice",
		"meta": map[string]any{"role": "admin"},
		"tags": []any{"owner"},
	}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("open object mapping: %v", out)
	}
	out["profile"].(map[string]any)["meta"].(map[string]any)["role"] = "changed"
	if xfield.GetData(input, []string{"source", "meta", "role"}) != "admin" {
		t.Fatalf("output aliases input: input=%v output=%v", input, out)
	}
}

func TestPathsAndMappingValidation(t *testing.T) {
	schema := schemaMap[struct{}]{
		"items": &xfield.ArrayField[struct{}]{
			Items: &xfield.ObjectField[struct{}]{Properties: schemaMap[struct{}]{
				"name": &xfield.StringField[struct{}]{},
			}},
		}}
	if err := xfield.ValidateMappings(
		[]xfield.Mapping{xfield.PathMapping([]string{"source"}, []string{"items", "1", "name"})},
		schema,
	); err != nil {
		t.Fatal(err)
	}
	if err := xfield.ValidateMappings(
		[]xfield.Mapping{xfield.PathMapping([]string{"source"}, []string{"items", "1", "missing"})},
		schema,
	); err == nil {
		t.Fatal("missing target path accepted")
	}
	if err := xfield.ValidateMappings([]xfield.Mapping{nil}, schema); err == nil {
		t.Fatal("nil mapping accepted")
	}
	if err := xfield.ValidateMappings(
		[]xfield.Mapping{xfield.MappingFunc(nil, func(any) (any, error) { return nil, nil })},
		schema,
	); err == nil {
		t.Fatal("empty target path accepted")
	}
	for _, path := range []string{"", "$", ".items", "items..name", "items[0].name"} {
		if _, err := xfield.FindFieldByPath(schema, path); err == nil {
			t.Fatalf("invalid path accepted: %q", path)
		}
	}
	var root any = []any{}
	root, err := xfield.SetDataValue(root, []string{"2", "name"}, "value")
	if err != nil || xfield.GetData(root, []string{"2", "name"}) != "value" {
		t.Fatalf("slice expansion: %v, %v", root, err)
	}
	if err := xfield.SetData([]any{}, []string{"2"}, "value"); err == nil {
		t.Fatal("root slice growth silently lost")
	}
	if _, err := xfield.SetDataValue([]any{}, []string{"999999999999"}, "value"); err == nil {
		t.Fatal("unbounded sparse array accepted")
	}
	target := map[string]any{}
	if err := xfield.SetData(target, []string{"items", "0", "name"}, "test"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"items": []any{map[string]any{"name": "test"}}}
	if !reflect.DeepEqual(target, want) {
		t.Fatalf("nested path: %v", target)
	}
}

type customObject struct {
	xfield.ObjectField[*dependencies]
}

func (*customObject) Type() string { return "custom_object" }

func TestExternalContainer(t *testing.T) {
	schema := schemaMap[*dependencies]{
		"asset": &customObject{ObjectField: xfield.ObjectField[*dependencies]{
			Properties: schemaMap[*dependencies]{
				"image": &resourceField{},
			},
		}},
	}
	deps := &dependencies{ctx: context.Background()}
	out, err := xfield.ApplyMappings(map[string]any{"source": map[string]any{"url": "link"}},
		[]xfield.Mapping{xfield.PathMapping([]string{"source"}, []string{"asset"},
			xfield.PathMapping([]string{"url"}, []string{"image"}),
		)},
		schema, deps)
	if err != nil || xfield.GetData(out, []string{"asset", "image"}) != "stored:link" || deps.calls != 1 {
		t.Fatalf("external container mapping: %v, %v, calls=%d", out, err, deps.calls)
	}
}
