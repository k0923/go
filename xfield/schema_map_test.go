package xfield_test

import (
	"errors"
	"iter"
	"reflect"
	"testing"

	"github.com/k0923/go/orderedmap"
	"github.com/k0923/go/xfield"
)

type executionOrder struct {
	parsed    []string
	presented []string
}

type orderedField struct {
	xfield.Meta
	name string
}

func (*orderedField) Type() string { return "ordered_test" }
func (f *orderedField) Parse(params *executionOrder, value any) (any, error) {
	params.parsed = append(params.parsed, f.name)
	return value, nil
}
func (f *orderedField) Present(params *executionOrder, value any) any {
	params.presented = append(params.presented, f.name)
	return value
}

func TestMapKeysAndDeterministicExecution(t *testing.T) {
	schema := schemaMap[*executionOrder]{
		"z": &orderedField{name: "last"},
		"a": &orderedField{name: "first"},
		"m": &orderedField{name: "middle"},
	}
	input := map[string]any{"z": 3, "a": 1, "m": 2}
	params := &executionOrder{}
	clean, err := xfield.ValidateModel(input, schema, params)
	if err != nil || !reflect.DeepEqual(clean, input) {
		t.Fatalf("map keys were not used to select data: %v, %v", clean, err)
	}
	xfield.PresentModel(clean, schema, params)
	want := []string{"first", "middle", "last"}
	if !reflect.DeepEqual(params.parsed, want) || !reflect.DeepEqual(params.presented, want) {
		t.Fatalf("execution order: parse=%v present=%v", params.parsed, params.presented)
	}
	field, err := xfield.FindFieldByPath(schema, "m")
	if err != nil || field != schema["m"] {
		t.Fatalf("map lookup: %v, %v", field, err)
	}
}

type fieldEntry[P any] struct {
	key   string
	field xfield.Field[P]
}

type arraySchema[P any] []fieldEntry[P]

func (s arraySchema[P]) Get(key string) (xfield.Field[P], bool) {
	for _, entry := range s {
		if entry.key == key {
			return entry.field, true
		}
	}
	return nil, false
}

func (s arraySchema[P]) All() iter.Seq2[string, xfield.Field[P]] {
	return func(yield func(string, xfield.Field[P]) bool) {
		for _, entry := range s {
			if !yield(entry.key, entry.field) {
				return
			}
		}
	}
}

var (
	_ xfield.Schema[any] = arraySchema[any]{}
	_ xfield.Schema[any] = (*orderedmap.Map[xfield.Field[any]])(nil)
)

func schemaNames[P any](s xfield.Schema[P]) []string {
	var names []string
	for key := range s.All() {
		names = append(names, key)
	}
	return names
}

func TestSchemaAdaptersPreserveCompileAndExecutionOrder(t *testing.T) {
	z, a := &orderedField{name: "z"}, &orderedField{name: "a"}
	var ordered orderedmap.Map[xfield.Field[*executionOrder]]
	ordered.Set("z", z)
	ordered.Set("a", a)
	array := arraySchema[*executionOrder]{{"z", z}, {"a", a}}
	for name, source := range map[string]xfield.Schema[*executionOrder]{
		"orderedmap": &ordered,
		"array":      array,
	} {
		t.Run(name, func(t *testing.T) {
			definitions := arraySchema[*executionOrder]{
				{"object", &xfield.ObjectField[*executionOrder]{Properties: source}},
			}
			doc := xfield.SchemaDocument[*executionOrder]{
				Definitions: definitions,
				Fields: arraySchema[*executionOrder]{
					{"zRoot", &xfield.RefField[*executionOrder]{Ref: "object"}},
					{"aRoot", &orderedField{name: "root"}},
				},
			}
			compiled, err := xfield.CompileDocument(doc)
			if err != nil {
				t.Fatal(err)
			}
			if got := schemaNames(compiled); !reflect.DeepEqual(got, []string{"zRoot", "aRoot"}) {
				t.Fatalf("compiled order = %v", got)
			}
			group, ok := compiled.Get("zRoot")
			if !ok {
				t.Fatal("compiled key missing")
			}
			properties := group.(xfield.ObjectProvider[*executionOrder]).ObjectFields()
			if got := schemaNames(properties); !reflect.DeepEqual(got, []string{"z", "a"}) {
				t.Fatalf("nested compiled order = %v", got)
			}
			found, err := xfield.FindFieldByPath(compiled, "zRoot.z")
			if err != nil || found != z {
				t.Fatalf("nested lookup = %v, %v", found, err)
			}
			if value, ok := compiled.Get("missing"); value != nil || ok {
				t.Fatalf("missing key = %v, %v", value, ok)
			}
			count := 0
			for range compiled.All() {
				count++
				break
			}
			if count != 1 {
				t.Fatal("compiled iterator ignored break")
			}

			params := &executionOrder{}
			input := map[string]any{"zRoot": map[string]any{"z": 1, "a": 2}, "aRoot": 3}
			clean, err := xfield.ValidateModel(input, compiled, params)
			if err != nil || !reflect.DeepEqual(clean, input) {
				t.Fatalf("validation = %v, %v", clean, err)
			}
			xfield.PresentModel(clean, compiled, params)
			want := []string{"z", "a", "root"}
			if !reflect.DeepEqual(params.parsed, want) || !reflect.DeepEqual(params.presented, want) {
				t.Fatalf("execution order: parse=%v present=%v", params.parsed, params.presented)
			}
			// Direct use must follow the same adapter order without compilation.
			params = &executionOrder{}
			if _, err := xfield.ValidateModel(map[string]any{"z": 1, "a": 2}, source, params); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(params.parsed, []string{"z", "a"}) {
				t.Fatalf("direct adapter order = %v", params.parsed)
			}
			if got := schemaNames(source); !reflect.DeepEqual(got, []string{"z", "a"}) {
				t.Fatal("compilation modified source")
			}
		})
	}
}

func TestSchemaNilContainers(t *testing.T) {
	var pointer *orderedmap.Map[xfield.Field[struct{}]]
	var native schemaMap[struct{}]
	var array arraySchema[struct{}]
	for _, schema := range []xfield.Schema[struct{}]{nil, pointer, native, array} {
		if err := xfield.ValidateSchema(schema); err != nil {
			t.Fatal(err)
		}
		clean, err := xfield.ValidateModel(map[string]any{"extra": 1}, schema, struct{}{})
		if err != nil || len(clean) != 0 {
			t.Fatalf("nil schema validation = %v, %v", clean, err)
		}
		presented := xfield.PresentModel(map[string]any{"extra": 1}, schema, struct{}{})
		if presented["extra"] != 1 {
			t.Fatal("nil schema dropped unknown data during presentation")
		}
		compiled, err := xfield.CompileDocument(xfield.SchemaDocument[struct{}]{Definitions: schema, Fields: schema})
		if err != nil || compiled != nil {
			t.Fatalf("nil schema compilation = %v, %v", compiled, err)
		}
		if _, err := xfield.FindFieldByPath(schema, "missing"); err == nil {
			t.Fatal("nil schema resolved a field")
		}
		field := &xfield.ObjectField[struct{}]{Properties: schema}
		if err := xfield.ValidateField(field); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSchemaDuplicateNames(t *testing.T) {
	duplicate := arraySchema[struct{}]{
		{"same", &xfield.StringField[struct{}]{}},
		{"same", &xfield.NumberField[struct{}]{}},
	}
	if err := xfield.ValidateSchema(duplicate); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("duplicate properties accepted: %v", err)
	}
	for _, doc := range []xfield.SchemaDocument[struct{}]{
		{Fields: duplicate},
		{Definitions: duplicate},
		{Fields: arraySchema[struct{}]{{"group", &xfield.ObjectField[struct{}]{Properties: duplicate}}}},
	} {
		if _, err := xfield.CompileDocument(doc); !errors.Is(err, xfield.ErrInvalidSchema) {
			t.Fatalf("duplicate declaration accepted: %v", err)
		}
	}
}

func TestSchemaFirstErrorFollowsAdapterOrder(t *testing.T) {
	fields := arraySchema[struct{}]{
		{"z", &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}}},
		{"a", &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}}},
	}
	compiled, err := xfield.CompileDocument(xfield.SchemaDocument[struct{}]{Fields: fields})
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []xfield.Schema[struct{}]{fields, compiled} {
		_, err := xfield.ValidateModel(nil, schema, struct{}{})
		var pathError *xfield.PathError
		if !errors.As(err, &pathError) || pathError.Path != "z" {
			t.Fatalf("first error = %v", err)
		}
	}
}
