package memory

import "reflect"

// clone preserves concrete Go types, aliases and cycles without a wire format.
// Unexported struct fields and functions are shared and must be immutable.
func clone[T any](value T) (T, error) {
	v := copyValue(reflect.ValueOf(value), map[copyKey]reflect.Value{})
	if !v.IsValid() {
		var zero T
		return zero, nil
	}
	return v.Interface().(T), nil
}

type copyKey struct {
	typ reflect.Type
	ptr uintptr
	len int
	cap int
}

func copyValue(v reflect.Value, seen map[copyKey]reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(copyValue(v.Elem(), seen))
		return out
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		key := copyKey{typ: v.Type(), ptr: v.Pointer()}
		if v.Kind() == reflect.Slice {
			key.len, key.cap = v.Len(), v.Cap()
		}
		if out, ok := seen[key]; ok {
			return out
		}
		var out reflect.Value
		switch v.Kind() {
		case reflect.Pointer:
			out = reflect.New(v.Type().Elem())
			seen[key] = out
			out.Elem().Set(copyValue(v.Elem(), seen))
		case reflect.Map:
			out = reflect.MakeMapWithSize(v.Type(), v.Len())
			seen[key] = out
			iter := v.MapRange()
			for iter.Next() {
				out.SetMapIndex(copyValue(iter.Key(), seen), copyValue(iter.Value(), seen))
			}
		case reflect.Slice:
			out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			seen[key] = out
			for i := 0; i < v.Len(); i++ {
				out.Index(i).Set(copyValue(v.Index(i), seen))
			}
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() && v.Type().Field(i).IsExported() {
				out.Field(i).Set(copyValue(v.Field(i), seen))
			}
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(copyValue(v.Index(i), seen))
		}
		return out
	default:
		return v
	}
}
