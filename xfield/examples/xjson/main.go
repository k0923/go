// 运行：go run ./xfield/examples/xjson
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/k0923/go/xfield"
)

// schema.json 模拟前端一次提交的公共定义和组件字段配置。
//
//go:embed schema.json
var schemaJSON []byte

const inputJSON = `{
	"start": 1000,
	"end": 3000,
	"tags": ["missing"],
	"dialog": null,
	"mappedDialogs": [
		{"speaker":"role-1","start":1000,"end":2000,"content":"你好","extra":true},
		{"speaker":"role-2","start":2000,"end":3000,"content":"再见"}
	],
	"extra": true
}`

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	// 1. encoding/json 会调用每个 xjson.G 的 UnmarshalJSON，恢复具体字段。
	var wire DocumentJSON
	if err := json.Unmarshal(schemaJSON, &wire); err != nil {
		return err
	}
	doc, err := wire.Document()
	if err != nil {
		return err
	}
	fmt.Printf("xjson 恢复的公共定义: %T\n", doc.Definitions["dialog"])

	// 2. xfield 解析 dialog 的引用，生成运行时 Schema。
	schema, err := xfield.CompileDocument(doc)
	if err != nil {
		return err
	}

	// 3. 校验和清洗数据。dialog 允许显式 null；人工台词可匹配多段。
	var input map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return err
	}
	clean, err := xfield.ValidateModel(input, schema, Params{})
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println("校验并清洗后的 DialogResult:")
	fmt.Println(string(raw))

	// 4. 保存原始 JSON DTO，保留公共定义和引用关系。编译结果用于运行。
	encoded, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return err
	}
	var restored DocumentJSON
	if err := json.Unmarshal(encoded, &restored); err != nil {
		return err
	}
	restoredDoc, err := restored.Document()
	if err != nil {
		return err
	}
	restoredSchema, err := xfield.CompileDocument(restoredDoc)
	if err != nil {
		return err
	}
	restoredClean, err := xfield.ValidateModel(input, restoredSchema, Params{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(clean, restoredClean) {
		return fmt.Errorf("JSON 往返后校验结果发生变化")
	}
	fmt.Println("Schema JSON 往返成功，校验结果一致，引用关系保留。")
	return nil
}
