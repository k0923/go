// 运行：go run ./xfield/examples/definitions
package main

import (
	"fmt"

	"github.com/k0923/go/xfield"
)

type Params = struct{}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	// 前端配置经过应用层反序列化后，得到这样的声明。
	doc := xfield.SchemaDocument[Params]{
		Definitions: map[string]xfield.Field[Params]{
			"dialog": &xfield.ObjectField[Params]{
				Properties: xfield.Schema[Params]{
					"speaker": &xfield.StringField[Params]{Meta: xfield.Meta{Required: true}},
					"start":   &xfield.NumberField[Params]{Meta: xfield.Meta{Required: true}},
					"end":     &xfield.NumberField[Params]{Meta: xfield.Meta{Required: true}},
					"content": &xfield.StringField[Params]{Meta: xfield.Meta{Required: true}},
				},
			},
		},
		Fields: xfield.Schema[Params]{
			"start": &xfield.NumberField[Params]{Meta: xfield.Meta{Required: true}},
			"end":   &xfield.NumberField[Params]{Meta: xfield.Meta{Required: true}},
			"tags": &xfield.ArrayField[Params]{
				Meta: xfield.Meta{Required: true},
				Items: &xfield.StringField[Params]{
					Enum: []string{"text", "role", "hallucination", "missing"},
				},
			},
			"dialog": &xfield.RefField[Params]{
				Meta: xfield.Meta{Required: true}, Ref: "dialog",
			},
			"mappedDialogs": &xfield.ArrayField[Params]{
				Meta:  xfield.Meta{Required: true},
				Items: &xfield.RefField[Params]{Ref: "dialog"},
			},
		},
	}
	schema, err := xfield.CompileDocument(doc)
	if err != nil {
		return err
	}
	clean, err := xfield.ValidateModel(map[string]any{
		"start": 1000, "end": 2000, "tags": []any{"missing"},
		"dialog": nil,
		"mappedDialogs": []any{
			map[string]any{"speaker": "role-1", "start": 1000, "end": 2000, "content": "你好"},
		},
	}, schema, Params{})
	if err != nil {
		return err
	}
	fmt.Printf("复用 Dialog 的校验结果: %#v\n", clean)
	field, err := xfield.FindFieldByPath(schema, "mappedDialogs.0.content")
	if err != nil {
		return err
	}
	fmt.Println("匹配台词的 content 类型:", field.Type())
	return nil
}
