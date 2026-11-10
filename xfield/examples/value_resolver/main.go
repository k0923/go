// 运行：go run ./xfield/examples/value_resolver
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/k0923/go/xfield"
	"github.com/k0923/go/xfield/examples/internal/schema"
)

// pointerMapping 演示业务 Mapping 接入 JSON Pointer（RFC 6901），并自行
// 决定来源缺失时写入 fallback。显式 null 仍解析为 nil。
func pointerMapping(pointer string, target []string, fallback any) xfield.Mapping {
	return xfield.MappingFunc(target, func(input any) (any, error) {
		return resolvePointer(input, pointer, fallback)
	})
}

func resolvePointer(input any, pointer string, fallback any) (any, error) {
	if pointer == "" {
		return input, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("JSON Pointer must start with /")
	}
	current := input
	for _, token := range strings.Split(pointer[1:], "/") {
		for i := 0; i < len(token); i++ {
			if token[i] == '~' {
				if i+1 >= len(token) || (token[i+1] != '0' && token[i+1] != '1') {
					return nil, fmt.Errorf("invalid JSON Pointer escape")
				}
				i++
			}
		}
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			value, exists := node[token]
			if !exists {
				return fallback, nil
			}
			current = value
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || strconv.Itoa(index) != token {
				return nil, fmt.Errorf("invalid JSON Pointer array index %q", token)
			}
			if index >= len(node) {
				return fallback, nil
			}
			current = node[index]
		default:
			return fallback, nil
		}
	}
	return current, nil
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	schema := schema.Map[struct{}]{
		"name":     &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}},
		"nickname": &xfield.StringField[struct{}]{},
		"missing":  &xfield.StringField[struct{}]{},
	}
	out, err := xfield.ApplyMappings(
		`{"profile":{"display/name":"张三","nickname":null}}`,
		[]xfield.Mapping{
			pointerMapping("/profile/display~1name", []string{"name"}, "匿名"),
			pointerMapping("/profile/nickname", []string{"nickname"}, "未提供"),
			pointerMapping("/profile/absent", []string{"missing"}, "未提供"),
		},
		schema, struct{}{},
	)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	fmt.Printf("显式 null 保留: %v；缺失路径使用业务默认值: %v\n",
		out["nickname"] == nil, out["missing"] == "未提供")
	return nil
}
