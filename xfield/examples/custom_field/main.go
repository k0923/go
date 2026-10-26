// 运行：go run ./xfield/examples/custom_field
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/k0923/go/xfield"
)

type Params struct {
	Normalize func(string) string
	URL       func(string) string
}

// ResourceField 直接实现 Field 和 Presenter，无需注册。
type ResourceField struct {
	xfield.Meta
}

func (*ResourceField) Type() string { return "resource" }

func (*ResourceField) Parse(data any, params Params) (any, error) {
	if data == nil {
		return nil, nil
	}
	value, ok := data.(string)
	if !ok {
		return nil, fmt.Errorf("resource expects string")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("resource is empty")
	}
	if params.Normalize != nil {
		value = params.Normalize(value)
	}
	return value, nil
}

func (*ResourceField) Present(data any, params Params) any {
	value, ok := data.(string)
	if !ok || params.URL == nil {
		return data
	}
	return params.URL(value)
}

// StringField 演示替换系统 string 的解析行为。
type StringField struct {
	xfield.StringField[Params]
}

func (f *StringField) Parse(data any, params Params) (any, error) {
	if value, ok := data.(string); ok {
		data = strings.TrimSpace(value)
	}
	return f.StringField.Parse(data, params)
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	schema := xfield.Schema[Params]{
		"images": &xfield.ArrayField[Params]{
			Items: &ResourceField{},
		},
	}
	clean, err := xfield.ValidateModel(map[string]any{"images": []any{" asset-1 "}}, schema, Params{
		Normalize: strings.ToUpper,
	})
	if err != nil {
		return err
	}
	display := xfield.PresentModel(clean, schema, Params{
		URL: func(id string) string { return "https://example.com/resources/" + url.PathEscape(id) },
	})
	for _, result := range []struct {
		label string
		value any
	}{{"内部值", clean}, {"展示值", display}} {
		raw, err := json.Marshal(result.value)
		if err != nil {
			return err
		}
		fmt.Println(result.label+":", string(raw))
	}

	minLength := 1
	field := &StringField{StringField: xfield.StringField[Params]{MinLength: &minLength}}
	value, err := field.Parse(" Alice ", Params{})
	if err != nil {
		return err
	}
	fmt.Printf("自定义 string: %q\n", value)
	return nil
}
