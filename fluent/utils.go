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
	value := reflect.ValueOf(obj)
	switch value.Kind() {
	case reflect.Slice, reflect.Array:
		result := ""
		for i := 0; i < value.Len(); i++ {
			result += fmt.Sprintf("%v,", value.Index(i))
		}
		if len(result) > 0 {
			result = result[0 : len(result)-1]
		}
		return result
	default:
		return fmt.Sprintf("%v", obj)
	}
}

func SetMap(obj interface{}, result map[string]string, tagName string) {
	value := reflect.ValueOf(obj)
	switch value.Kind() {
	case reflect.String:
		result[obj.(string)] = ""
		return
	case reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Bool, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		result[fmt.Sprintf("%v", obj)] = ""
		return
	case reflect.Map:
		for _, key := range value.MapKeys() {
			result[fmt.Sprintf("%v", key.Interface())] = GetValue(value.MapIndex(key).Interface())
		}
	case reflect.Ptr:
		SetMap(value.Elem().Interface(), result, tagName)
	case reflect.Struct:
		tp := value.Type()
		for i := 0; i < tp.NumField(); i++ {
			field := tp.Field(i)
			tag := field.Tag.Get(tagName)
			if strings.Contains(tag, "omitempty") {
				continue
			} else if tag != "" {
				result[tag] = GetValue(value.Field(i).Interface())
			} else {
				switch field.Type.Kind() {
				case reflect.Ptr, reflect.Struct, reflect.Map:
					SetMap(value.Field(i).Interface(), result, tagName)
					continue
				}
			}
		}
	}
}

func BasicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}
