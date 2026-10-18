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
	registry := xfield.DefaultRegistry[struct{}]()
	schema, err := registry.DecodeSchema([]byte(`[
		{"type":"string","key":"name","required":true,"minLength":1},
		{"type":"number","key":"age","min":0,"max":150},
		{"type":"boolean","key":"active"},
		{"type":"array","key":"tags","items":{"type":"string"}}
	]`))
	if err != nil {
		return err
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
