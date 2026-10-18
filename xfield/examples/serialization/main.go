// 运行：go run ./xfield/examples/serialization
package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/k0923/go/xfield"
)

type Params struct{}

// EmailField 没有 Type 属性，也没有自定义 JSON 编解码方法。
// Type() 提供判别符，AllowedDomain 是需要持久化的自定义配置。
type EmailField struct {
	xfield.Meta
	AllowedDomain string `json:"allowedDomain"`
}

func (*EmailField) Type() string { return "email" }

func (f *EmailField) Parse(data any, _ Params) (any, error) {
	if data == nil {
		return nil, nil
	}
	value, ok := data.(string)
	if !ok {
		return nil, fmt.Errorf("expect email string")
	}
	local, domain, found := strings.Cut(value, "@")
	if !found || local == "" || domain != f.AllowedDomain {
		return nil, fmt.Errorf("email must belong to %s", f.AllowedDomain)
	}
	return value, nil
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	original := xfield.Schema[Params]{
		xfield.Wrap(&EmailField{
			Meta:          xfield.Meta{Key: "email", Required: true},
			AllowedDomain: "example.com",
		}),
		xfield.Wrap(&xfield.ArrayField[Params]{
			Meta: xfield.Meta{Key: "backupEmails"},
			Items: xfield.Wrap(&EmailField{
				AllowedDomain: "example.org",
			}),
		}),
	}

	// 序列化无需注册。Wrap/FieldRef 自动把 Type() 写入 JSON。
	raw, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println("生成的 Schema JSON:")
	fmt.Println(string(raw))

	// 注册只负责创建实例，类型名自动从 EmailField.Type() 获取。
	registry := xfield.DefaultRegistry[Params]()
	if err := registry.Register(func() xfield.Field[Params] { return &EmailField{} }); err != nil {
		return err
	}

	// 直接使用上面生成的 raw，不另写一份 JSON。
	restored, err := registry.DecodeSchema(raw)
	if err != nil {
		return err
	}
	if err := xfield.ValidateSchema(restored); err != nil {
		return err
	}
	email, ok := restored[0].Value().(*EmailField)
	if !ok {
		return fmt.Errorf("expected *EmailField, got %T", restored[0].Value())
	}
	array, ok := restored[1].Value().(*xfield.ArrayField[Params])
	if !ok {
		return fmt.Errorf("expected array, got %T", restored[1].Value())
	}
	backup, ok := array.Items.Value().(*EmailField)
	if !ok {
		return fmt.Errorf("expected nested *EmailField, got %T", array.Items.Value())
	}
	if email.Key != "email" || !email.Required || email.AllowedDomain != "example.com" ||
		backup.AllowedDomain != "example.org" {
		return fmt.Errorf("custom field configuration was not restored")
	}
	fmt.Printf("\n恢复顶层字段: %T, key=%q, required=%v, allowedDomain=%q\n",
		email, email.Key, email.Required, email.AllowedDomain)
	fmt.Printf("恢复数组子字段: %T, allowedDomain=%q\n", backup, backup.AllowedDomain)

	reencoded, err := json.MarshalIndent(restored, "", "  ")
	if err != nil {
		return err
	}
	if string(raw) != string(reencoded) {
		return fmt.Errorf("round-trip JSON differs")
	}
	fmt.Println("重新序列化的 JSON 与原 JSON 完全一致: true")

	input := map[string]any{
		"email":        "alice@example.com",
		"backupEmails": []any{"alice@example.org"},
	}
	clean, err := xfield.ValidateModel(input, restored, Params{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(clean, input) {
		return fmt.Errorf("unexpected parsed values: %v", clean)
	}
	values, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	fmt.Println("恢复后的字段正常解析数据:", string(values))
	return nil
}
