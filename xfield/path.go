package xfield

import (
	"fmt"
	"strconv"
	"strings"
)

// GetData returns nil for a missing path. Use a ValueResolver when missing
// values must be distinguished from explicit null.
func GetData(data any, path []string) any {
	value, _ := lookupData(data, path)
	return value
}

func lookupData(data any, path []string) (any, bool) {
	current := data
	for _, key := range path {
		if object, ok := asStringMap(current); ok {
			var exists bool
			current, exists = object[key]
			if !exists {
				return nil, false
			}
			continue
		}
		if array, ok := asAnySlice(current); ok {
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(array) {
				return nil, false
			}
			current = array[index]
			continue
		}
		return nil, false
	}
	return current, true
}

// SetData mutates a map or an existing []any root. To grow/replace a slice
// root, use SetDataValue and retain its returned value.
func SetData(target any, path []string, value any) error {
	if isNil(target) {
		return fmt.Errorf("target cannot be nil")
	}
	if len(path) == 0 {
		return fmt.Errorf("path cannot be empty")
	}
	if array, ok := target.([]any); ok {
		index, err := strconv.Atoi(path[0])
		if err != nil || index < 0 || index >= len(array) {
			return fmt.Errorf("root array growth requires SetDataValue")
		}
	}
	_, err := SetDataValue(target, path, value)
	return err
}

// MaxArrayIndex bounds allocation when creating sparse arrays from paths.
const MaxArrayIndex = 1 << 20

func SetDataValue(target any, path []string, value any) (any, error) {
	if len(path) == 0 {
		return value, nil
	}
	if len(path) > MaxSchemaDepth {
		return nil, fmt.Errorf("maximum path depth exceeded")
	}
	return setDataRecursive(target, path, value)
}

func setDataRecursive(current any, path []string, value any) (any, error) {
	if len(path) == 0 {
		return value, nil
	}
	key := path[0]
	if current == nil {
		if isArrayKey(key) {
			current = []any{}
		} else {
			current = map[string]any{}
		}
	}
	switch node := current.(type) {
	case map[string]any:
		if node == nil {
			node = make(map[string]any)
		}
		child, err := setDataRecursive(node[key], path[1:], value)
		if err != nil {
			return nil, err
		}
		node[key] = child
		return node, nil
	case []any:
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index > MaxArrayIndex {
			return nil, fmt.Errorf("invalid array index %q", key)
		}
		if index >= len(node) {
			expanded := make([]any, index+1)
			copy(expanded, node)
			node = expanded
		}
		child, err := setDataRecursive(node[index], path[1:], value)
		if err != nil {
			return nil, err
		}
		node[index] = child
		return node, nil
	default:
		return nil, fmt.Errorf("cannot set path %q on type %T", key, current)
	}
}

func FindFieldByPath[P any](schema Schema[P], path string) (Field[P], error) {
	return FindFieldInField(rootObjectField(schema), path)
}

func FindFieldInField[P any](field Field[P], path string) (Field[P], error) {
	parts, err := parseFieldPath(path)
	if err != nil {
		return nil, err
	}
	return findFieldInField(field, parts, 0)
}

func findFieldInField[P any](field Field[P], parts []string, depth int) (Field[P], error) {
	if isNil(field) {
		return nil, invalidSchema("schema is nil")
	}
	if depth >= MaxSchemaDepth {
		return nil, invalidSchema("maximum schema depth exceeded")
	}
	if len(parts) == 0 {
		return field, nil
	}
	if object, ok := field.(ObjectProvider[P]); ok {
		for _, ref := range object.ObjectFields() {
			child := ref.Value()
			if KeyOf(child) == parts[0] {
				return findFieldInField(child, parts[1:], depth+1)
			}
		}
		return nil, fmt.Errorf("field %q not found", parts[0])
	}
	if array, ok := field.(ArrayProvider[P]); ok {
		if isArrayKey(parts[0]) {
			parts = parts[1:]
		}
		return findFieldInField(array.ItemField(), parts, depth+1)
	}
	return nil, fmt.Errorf("field with type %q does not support child path %q", TypeOf(field), strings.Join(parts, "."))
}

func rootObjectField[P any](schema Schema[P]) *ObjectField[P] {
	return &ObjectField[P]{Properties: schema}
}

func parseFieldPath(path string) ([]string, error) {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "$.") {
		path = path[2:]
	}
	if path == "" || path == "$" {
		return nil, fmt.Errorf("root or empty path is not supported")
	}
	parts := strings.Split(path, ".")
	if len(parts) > MaxSchemaDepth {
		return nil, fmt.Errorf("maximum path depth exceeded")
	}
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.ContainsAny(part, "[]") {
			return nil, fmt.Errorf("path requires non-empty dot segments")
		}
		parts[i] = part
	}
	return parts, nil
}

func isArrayKey(key string) bool {
	index, err := strconv.Atoi(key)
	return err == nil && index >= 0
}
