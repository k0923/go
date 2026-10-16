package fluent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

func JsonResultHandler(result interface{}) ResultHandler {
	return func(ctx context.Context, response *http.Response) error {
		if response.Body == nil {
			return nil
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		err = json.Unmarshal(data, result)
		if err != nil {
			response.Body = io.NopCloser(bytes.NewReader(data))
			return NewResponseError(response, err)
		}
		return nil
	}
}

func GetValue(obj interface{}) string {
	if s, ok := scalarString(obj); ok {
		return s
	}
	value := reflect.ValueOf(obj)
	switch value.Kind() {
	case reflect.Slice, reflect.Array:
		var sb strings.Builder
		for i := range value.Len() {
			if i > 0 {
				sb.WriteByte(',')
			}
			elem := value.Index(i)
			if s, ok := scalarString(elem.Interface()); ok {
				sb.WriteString(s)
			} else {
				fmt.Fprintf(&sb, "%v", elem)
			}
		}
		return sb.String()
	default:
		return fmt.Sprintf("%v", obj)
	}
}

// scalarString 为常见标量提供免反射、免 fmt 的快速路径，输出与 fmt.Sprintf("%v") 一致。
func scalarString(obj interface{}) (string, bool) {
	switch v := obj.(type) {
	case string:
		return v, true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case uint32:
		return strconv.FormatUint(uint64(v), 10), true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32), true
	}
	return "", false
}

func SetMap(obj interface{}, result map[string]string, tagName string) {
	value := reflect.ValueOf(obj)
	switch value.Kind() {
	case reflect.String:
		result[value.String()] = ""
		return
	case reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Bool, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		result[GetValue(obj)] = ""
		return
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			key := iter.Key().Interface()
			k, ok := scalarString(key)
			if !ok {
				k = fmt.Sprintf("%v", key)
			}
			result[k] = GetValue(iter.Value().Interface())
		}
	case reflect.Ptr:
		// nil 指针没有任何字段可写，直接跳过而不是 panic。
		if value.IsNil() {
			return
		}
		SetMap(value.Elem().Interface(), result, tagName)
	case reflect.Struct:
		// 未使用 Go 1.26 的 reflect.Value.Fields 迭代器：SetMap 是递归函数，
		// 迭代器闭包会逃逸到堆上，基准测试中每次调用多出约 500B 分配。
		tp := value.Type()
		for i := range tp.NumField() {
			field, fieldValue := tp.Field(i), value.Field(i)
			if !field.IsExported() {
				continue
			}
			name, omitEmpty := parseTag(field.Tag.Get(tagName))
			if name != "" {
				if omitEmpty && fieldValue.IsZero() {
					continue
				}
				result[name] = GetValue(fieldValue.Interface())
				continue
			}
			switch field.Type.Kind() {
			case reflect.Ptr, reflect.Struct, reflect.Map:
				SetMap(fieldValue.Interface(), result, tagName)
			}
		}
	}
}

// parseTag 解析形如 `name,omitempty` 的标签。
func parseTag(tag string) (name string, omitEmpty bool) {
	name, opts, _ := strings.Cut(tag, ",")
	for opt := range strings.SplitSeq(opts, ",") {
		if opt == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty
}

func BasicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}
