package fluent

import (
	"encoding"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// encodeValues 把 url.Values、map[string]string、map[string][]string 或带标签的 struct 编码为 url.Values。
//
// struct 规则（与 encoding/json 保持一致的心智模型）：
//   - 标签形如 `query:"name,omitempty"`；"-" 表示忽略；无标签时使用字段名。
//   - omitempty 只跳过零值字段。
//   - slice/array 展开为同名多值（?tag=a&tag=b），而不是逗号拼接。
//   - 匿名嵌入的 struct 会被展开；nil 指针被跳过。
//   - time.Time 编码为 RFC3339；实现 encoding.TextMarshaler 的类型使用 MarshalText。
func encodeValues(v any, tag string) (url.Values, error) {
	switch m := v.(type) {
	case nil:
		return url.Values{}, nil
	case url.Values:
		return m.Clone(), nil
	case map[string][]string:
		return url.Values(m).Clone(), nil
	case map[string]string:
		out := make(url.Values, len(m))
		for k, val := range m {
			out.Set(k, val)
		}
		return out, nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return url.Values{}, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("cannot encode %T as %s values: want struct, url.Values or map[string]string", v, tag)
	}
	out := url.Values{}
	if err := encodeStruct(rv, tag, out); err != nil {
		return nil, err
	}
	return out, nil
}

var (
	timeType          = reflect.TypeFor[time.Time]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

func encodeStruct(rv reflect.Value, tag string, out url.Values) error {
	rt := rv.Type()
	for i := range rt.NumField() {
		field := rt.Field(i)
		fv := rv.Field(i)
		if field.Anonymous && field.Tag.Get(tag) == "" {
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct && fv.Type() != timeType {
				if err := encodeStruct(fv, tag, out); err != nil {
					return err
				}
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(field.Tag.Get(tag), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if hasOpt(opts, "omitempty") && fv.IsZero() {
			continue
		}
		for fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				break
			}
			fv = fv.Elem()
		}
		if fv.Kind() == reflect.Pointer {
			continue
		}
		if (fv.Kind() == reflect.Slice || fv.Kind() == reflect.Array) && fv.Type().Elem().Kind() != reflect.Uint8 {
			for j := range fv.Len() {
				s, err := scalar(fv.Index(j))
				if err != nil {
					return fmt.Errorf("field %s: %w", field.Name, err)
				}
				out.Add(name, s)
			}
			continue
		}
		s, err := scalar(fv)
		if err != nil {
			return fmt.Errorf("field %s: %w", field.Name, err)
		}
		out.Add(name, s)
	}
	return nil
}

func hasOpt(opts, want string) bool {
	for opt := range strings.SplitSeq(opts, ",") {
		if opt == want {
			return true
		}
	}
	return false
}

func scalar(v reflect.Value) (string, error) {
	if v.Type() == timeType {
		return v.Interface().(time.Time).Format(time.RFC3339Nano), nil
	}
	if v.Type().Implements(textMarshalerType) {
		b, err := v.Interface().(encoding.TextMarshaler).MarshalText()
		return string(b), err
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), nil
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), nil
	case reflect.Float32:
		return strconv.FormatFloat(v.Float(), 'g', -1, 32), nil
	case reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64), nil
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return string(v.Bytes()), nil
		}
	}
	return "", fmt.Errorf("unsupported type %s", v.Type())
}
