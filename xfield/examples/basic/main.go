// 运行：go run ./xfield/examples/basic
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
	minLength, minAge, maxAge := 1, 0.0, 150.0
	schema := xfield.Schema[struct{}]{

		"name": &xfield.StringField[struct{}]{
			Meta: xfield.Meta{Required: true}, MinLength: &minLength,
		},

		"age": &xfield.NumberField[struct{}]{
			Meta: xfield.Meta{}, Min: &minAge, Max: &maxAge,
		},
		"active": &xfield.BooleanField[struct{}]{Meta: xfield.Meta{}},

		"tags": &xfield.ArrayField[struct{}]{
			Meta: xfield.Meta{}, Items: &xfield.StringField[struct{}]{},
		},
	}
	clean, err := xfield.ValidateModel(map[string]any{
		"name": "张三", "age": 18, "active": true,
		"tags": []any{"Go", "JSON"}, "extra": "被过滤",
	}, schema, struct{}{})
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	fmt.Println("完整模型:", string(encoded))
	partial, err := xfield.ValidatePartial(map[string]any{"age": 20}, schema, struct{}{})
	if err != nil {
		return err
	}
	fmt.Println("局部更新:", partial)
	_, err = xfield.ValidateModel(map[string]any{}, schema, struct{}{})
	fmt.Println("缺少必填字段:", err)
	field, err := xfield.FindFieldByPath(schema, "tags.0")
	if err != nil {
		return err
	}
	fmt.Println("tags 元素类型:", field.Type())
	return nil
}
