// 运行：go run ./xfield/examples/value_resolver
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/k0923/go/xfield"
)

const MappingTypePointer xfield.MappingType = "json_pointer"

// PointerResolver 演示通过 ValueResolver 接入 JSON Pointer（RFC 6901）。
// 输入由 ApplyMappings 预先解码；~1 表示 /，~0 表示 ~。
// 这里只处理解码后的 JSON map/array，不支持 URI fragment 形式。
type PointerResolver struct{}

func (PointerResolver) Resolve(input any, kind xfield.MappingType, path string) (any, bool, error) {
	if kind != MappingTypePointer {
		return (xfield.PathResolver{}).Resolve(input, kind, path)
	}
	if path == "" {
		return input, true, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false, fmt.Errorf("JSON Pointer must start with /")
	}
	current := input
	for _, token := range strings.Split(path[1:], "/") {
		for i := 0; i < len(token); i++ {
			if token[i] == '~' {
				if i+1 >= len(token) || (token[i+1] != '0' && token[i+1] != '1') {
					return nil, false, fmt.Errorf("invalid JSON Pointer escape")
				}
				i++
			}
		}
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			value, exists := node[token]
			if !exists {
				return nil, false, nil
			}
			current = value
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || strconv.Itoa(index) != token {
				return nil, false, fmt.Errorf("invalid JSON Pointer array index %q", token)
			}
			if index >= len(node) {
				return nil, false, nil
			}
			current = node[index]
		default:
			return nil, false, nil
		}
	}
	return current, true, nil
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	schema := xfield.Schema[struct{}]{
		"name":     &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}},
		"nickname": &xfield.StringField[struct{}]{},
		"missing":  &xfield.StringField[struct{}]{},
	}
	out, err := xfield.ApplyMappings(
		`{"profile":{"display/name":"张三","nickname":null}}`,
		[]xfield.Mapping{
			{Type: MappingTypePointer, From: "/profile/display~1name", To: "name"},
			{Type: MappingTypePointer, From: "/profile/nickname", To: "nickname"},
			{Type: MappingTypePointer, From: "/profile/absent", To: "missing"},
		},
		schema, struct{}{}, xfield.MappingOptions{Resolver: PointerResolver{}},
	)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	_, nicknameExists := out["nickname"]
	_, missingExists := out["missing"]
	fmt.Printf("显式 null 保留: %v；缺失路径跳过: %v\n", nicknameExists, !missingExists)
	return nil
}
