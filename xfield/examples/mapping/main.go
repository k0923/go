// 运行：go run ./xfield/examples/mapping
package main

import (
	"encoding/json"
	"fmt"

	"github.com/k0923/go/xfield"
)

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	schema := xfield.Schema[struct{}]{
		"title": &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}},
		"members": &xfield.ArrayField[struct{}]{
			Items: &xfield.ObjectField[struct{}]{Properties: xfield.Schema[struct{}]{
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
		{From: "group", To: "title"},
		{From: "users", To: "members", Children: []xfield.Mapping{
			{From: "display_name", To: "name"},
			{From: "points", To: "score"},
		}},
	}
	if err := xfield.ValidateMappings(mappings, schema, nil); err != nil {
		return err
	}
	out, err := xfield.ApplyMappings(input, mappings, schema, struct{}{}, xfield.MappingOptions{
		MissingPolicy: xfield.MissingError,
	})
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
