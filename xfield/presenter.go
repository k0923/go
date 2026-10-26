package xfield

import "reflect"

// PresentModel retains unknown keys and recursively presents declared fields.
func PresentModel[P any](data map[string]any, model Schema[P], params P) map[string]any {
	if data == nil {
		return nil
	}
	result := make(map[string]any, len(data))
	for key, value := range data {
		result[key] = value
	}
	for _, key := range schemaKeys(model) {
		field := model[key]
		if value, exists := data[key]; exists && !isNil(field) {
			result[key] = presentFieldValue(field, value, params)
		}
	}
	return result
}

func presentFieldValue[P any](field Field[P], data any, params P) any {
	if presenter, ok := field.(Presenter[P]); ok && !isNil(field) {
		return presenter.Present(data, params)
	}
	return data
}

// Standard-library reflection supports named map/slice types (including
// database-driver containers) without depending on a particular driver.
func asStringMap(data any) (map[string]any, bool) {
	if value, ok := data.(map[string]any); ok {
		return value, true
	}
	v := reflect.ValueOf(data)
	if !v.IsValid() || v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	out := make(map[string]any, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		out[iter.Key().String()] = iter.Value().Interface()
	}
	return out, true
}

func asAnySlice(data any) ([]any, bool) {
	if value, ok := data.([]any); ok {
		return value, true
	}
	v := reflect.ValueOf(data)
	if !v.IsValid() || (v.Kind() != reflect.Slice && v.Kind() != reflect.Array) {
		return nil, false
	}
	// Binary data is not a JSON array.
	if v.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	out := make([]any, v.Len())
	for i := range out {
		out[i] = v.Index(i).Interface()
	}
	return out, true
}
