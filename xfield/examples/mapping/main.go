// 运行：go run ./xfield/examples/mapping
package main

import (
	"encoding/json"
	"fmt"

	"github.com/k0923/go/xfield"
	"github.com/k0923/go/xfield/examples/internal/schema"
)

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	schema := schema.Map[struct{}]{
		"title": &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}},
		"members": &xfield.ArrayField[struct{}]{
			Items: &xfield.ObjectField[struct{}]{Properties: schema.Map[struct{}]{
				"name":  &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}},
				"score": &xfield.NumberField[struct{}]{},
			}},
		},
	}
	input := map[string]any{
		"group": "一组",
		"users": []any{
			map[string]any{"display_name": "张三", "points": 98},
			map[string]any{"display_name": "李四", "points": 87},
		},
	}
	mappings := []xfield.Mapping{
		xfield.PathMapping([]string{"group"}, []string{"title"}),
		xfield.PathMapping([]string{"users"}, []string{"members"},
			xfield.PathMapping([]string{"display_name"}, []string{"name"}),
			xfield.PathMapping([]string{"points"}, []string{"score"}),
		),
	}
	if err := xfield.ValidateMappings(mappings, schema); err != nil {
		return err
	}
	out, err := xfield.ApplyMappings(input, mappings, schema, struct{}{})
	if err != nil {
		return err
	}
	clean, err := xfield.ValidateModel(out, schema, struct{}{})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}
