package xfield_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/k0923/go/xfield"
)

func TestMappingsNestedParseOnce(t *testing.T) {
	schema := xfield.Schema[*dependencies]{
		xfield.Wrap(&xfield.ArrayField[*dependencies]{
			Meta: xfield.Meta{Key: "teams"},
			Items: xfield.Wrap(&xfield.ObjectField[*dependencies]{Properties: xfield.Schema[*dependencies]{
				xfield.Wrap(&xfield.StringField[*dependencies]{Meta: xfield.Meta{Key: "name", Required: true}}),
				xfield.Wrap(&xfield.ArrayField[*dependencies]{
					Meta: xfield.Meta{Key: "images"},
					Items: xfield.Wrap(&xfield.ObjectField[*dependencies]{Properties: xfield.Schema[*dependencies]{
						xfield.Wrap(&resourceField{Meta: xfield.Meta{Key: "url", Required: true}}),
					}}),
				}),
			}}),
		}),
	}
	mappings := []xfield.Mapping{{
		From: "groups", To: "teams", Children: []xfield.Mapping{
			{From: "title", To: "name"},
			{From: "assets", To: "images", Children: []xfield.Mapping{
				{From: "link", To: "url"},
			}},
		},
	}}
	input := map[string]any{"groups": []any{
		map[string]any{"title": "A", "assets": []any{map[string]any{"link": "a"}}},
		map[string]any{"title": "B", "assets": []any{map[string]any{"link": "b"}}},
	}}
	deps := &dependencies{ctx: context.Background()}
	out, err := xfield.ApplyMappings(input, mappings, schema, deps, xfield.MappingOptions{})
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

func TestMappingsMissingNullResolverAndRawJSON(t *testing.T) {
	schema := xfield.Schema[struct{}]{
		xfield.Wrap(&xfield.StringField[struct{}]{Meta: xfield.Meta{Key: "name", Required: true}}),
		xfield.Wrap(&xfield.NumberField[struct{}]{Meta: xfield.Meta{Key: "id"}}),
	}
	mappings := []xfield.Mapping{{From: "name", To: "name"}, {From: "id", To: "id"}}
	out, err := xfield.ApplyMappings(`{"name":null,"id":9007199254740993}`, mappings, schema, struct{}{}, xfield.MappingOptions{})
	if err != nil || out["name"] != nil || out["id"] != json.Number("9007199254740993") {
		t.Fatalf("null/number: %v, %v", out, err)
	}
	out, err = xfield.ApplyMappings(map[string]any{}, mappings, schema, struct{}{}, xfield.MappingOptions{})
	if err != nil || len(out) != 0 {
		t.Fatalf("missing skip: %v, %v", out, err)
	}
	if _, err := xfield.ApplyMappings(map[string]any{}, mappings, schema, struct{}{},
		xfield.MappingOptions{MissingPolicy: xfield.MissingError}); err == nil {
		t.Fatal("missing error policy ignored")
	}
	if _, err := xfield.ApplyMappings(nil, nil, schema, struct{}{},
		xfield.MappingOptions{MissingPolicy: "invalid"}); err == nil {
		t.Fatal("invalid missing policy accepted")
	}
	mappings = []xfield.Mapping{{Type: xfield.MappingTypeGJSON, From: `items.#(active==true).name`, To: "name"}}
	if _, err := xfield.ApplyMappings(map[string]any{}, mappings, schema, struct{}{}, xfield.MappingOptions{}); err == nil {
		t.Fatal("GJSON expression accepted without resolver")
	}
	resolver := xfield.ValueResolverFunc(func(_ any, kind xfield.MappingType, path string) (any, bool, error) {
		if kind != xfield.MappingTypeGJSON || path != mappings[0].From {
			t.Fatal("resolver did not receive the original expression")
		}
		return "resolved", true, nil
	})
	out, err = xfield.ApplyMappings(map[string]any{}, mappings, schema, struct{}{}, xfield.MappingOptions{Resolver: resolver})
	if err != nil || out["name"] != "resolved" {
		t.Fatalf("external resolver: %v, %v", out, err)
	}
	for _, raw := range []string{`{`, `{} {}`} {
		if _, err := xfield.ApplyMappings(raw, mappings, schema, struct{}{}, xfield.MappingOptions{Resolver: resolver}); err == nil {
			t.Fatalf("invalid JSON accepted: %s", raw)
		}
	}
}

func TestMappingsDoNotMutateInput(t *testing.T) {
	schema := xfield.Schema[struct{}]{xfield.Wrap(&xfield.ObjectField[struct{}]{
		Meta: xfield.Meta{Key: "profile"},
		Properties: xfield.Schema[struct{}]{
			xfield.Wrap(&xfield.StringField[struct{}]{Meta: xfield.Meta{Key: "name"}}),
		},
	})}
	input := map[string]any{"profile": map[string]any{"name": "old"}, "replacement": "new"}
	mappings := []xfield.Mapping{{From: "profile", To: "profile"}, {From: "replacement", To: "profile.name"}}
	out, err := xfield.ApplyMappings(input, mappings, schema, struct{}{}, xfield.MappingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if xfield.GetData(input, []string{"profile", "name"}) != "old" ||
		xfield.GetData(out, []string{"profile", "name"}) != "new" {
		t.Fatalf("input %v output %v", input, out)
	}
}

func TestPathsAndSourceValidation(t *testing.T) {
	schema := xfield.Schema[struct{}]{xfield.Wrap(&xfield.ArrayField[struct{}]{
		Meta: xfield.Meta{Key: "items"},
		Items: xfield.Wrap(&xfield.ObjectField[struct{}]{Properties: xfield.Schema[struct{}]{
			xfield.Wrap(&xfield.StringField[struct{}]{Meta: xfield.Meta{Key: "name"}}),
		}}),
	})}
	if err := xfield.ValidateMappingsWithSource(
		[]xfield.Mapping{{From: "items.0.name", To: "items.1.name"}}, schema, schema,
	); err != nil {
		t.Fatal(err)
	}
	if err := xfield.ValidateMappingsWithSource(
		[]xfield.Mapping{{From: "items.0.missing", To: "items.1.name"}}, schema, schema,
	); err == nil {
		t.Fatal("missing source path accepted")
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

func TestExternalContainerAndNestedFactory(t *testing.T) {
	registry := xfield.DefaultRegistry[*dependencies]()
	registerResource(t, registry)
	if err := registry.Register(func() xfield.Field[*dependencies] {
		return &customObject{}
	}); err != nil {
		t.Fatal(err)
	}
	schema, err := registry.DecodeSchema([]byte(`[{"type":"custom_object","key":"asset","properties":[{"type":"resource","key":"image"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	deps := &dependencies{ctx: context.Background()}
	out, err := xfield.ApplyMappings(map[string]any{"source": map[string]any{"url": "link"}},
		[]xfield.Mapping{{From: "source", To: "asset", Children: []xfield.Mapping{{From: "url", To: "image"}}}},
		schema, deps, xfield.MappingOptions{})
	if err != nil || xfield.GetData(out, []string{"asset", "image"}) != "stored:link" || deps.calls != 1 {
		t.Fatalf("external container mapping: %v, %v, calls=%d", out, err, deps.calls)
	}
}
