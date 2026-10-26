package xfield_test

import (
	"reflect"
	"testing"

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
func (f *orderedField) Parse(value any, params *executionOrder) (any, error) {
	params.parsed = append(params.parsed, f.name)
	return value, nil
}
func (f *orderedField) Present(value any, params *executionOrder) any {
	params.presented = append(params.presented, f.name)
	return value
}

func TestMapKeysAndDeterministicExecution(t *testing.T) {
	schema := xfield.Schema[*executionOrder]{
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
