package xfield_test

import (
	"reflect"
	"testing"

	"github.com/k0923/go/xfield"
)

func TestPresentModel(t *testing.T) {
	t.Run("nested params and unchanged fields", func(t *testing.T) {
		schema := xfield.Schema[*dependencies]{

			"profile": &xfield.ObjectField[*dependencies]{
				Meta: xfield.Meta{},
				Properties: xfield.Schema[*dependencies]{

					"images": &xfield.ArrayField[*dependencies]{
						Meta:  xfield.Meta{},
						Items: &resourceField{},
					},
					"name": &xfield.StringField[*dependencies]{Meta: xfield.Meta{}},
				},
			},
		}
		input := map[string]any{
			"profile": map[string]any{
				"images": []any{"stored:a", "stored:b", nil},
				"name":   "Alice",
				"extra":  true,
			},
			"unknown": 10,
		}
		deps := &dependencies{displayPrefix: "https://example/"}
		output := xfield.PresentModel(input, schema, deps)
		want := map[string]any{
			"profile": map[string]any{
				"images": []any{"https://example/a", "https://example/b", nil},
				"name":   "Alice",
				"extra":  true,
			},
			"unknown": 10,
		}
		if !reflect.DeepEqual(output, want) || deps.presentCalls != 2 {
			t.Fatalf("output=%v calls=%d", output, deps.presentCalls)
		}
		if xfield.GetData(input, []string{"profile", "images", "0"}) != "stored:a" {
			t.Fatal("presentation mutated input")
		}
	})

	t.Run("nil and empty params", func(t *testing.T) {
		schema := xfield.Schema[struct{}]{
			"active": &xfield.BooleanField[struct{}]{Meta: xfield.Meta{}},
		}
		if result := xfield.PresentModel(nil, schema, struct{}{}); result != nil {
			t.Fatalf("nil input became %v", result)
		}
		input := map[string]any{"active": true}
		if result := xfield.PresentModel(input, schema, struct{}{}); !reflect.DeepEqual(result, input) {
			t.Fatalf("field without presenter changed: %v", result)
		}
	})
}
