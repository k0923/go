package xfield

import (
	"encoding/json"
	"fmt"
	"strings"
)

type MappingType string

const (
	MappingTypePath  MappingType = "path"
	MappingTypeGJSON MappingType = "gjson"
)

type Mapping struct {
	Type     MappingType `json:"type,omitempty"`
	From     string      `json:"from,omitempty"`
	To       string      `json:"to,omitempty"`
	Children []Mapping   `json:"children,omitempty"`
}

// ValueResolver lets applications provide GJSON, JSONPath or other extraction
// engines. exists must distinguish a missing value from an explicit null.
type ValueResolver interface {
	Resolve(input any, mappingType MappingType, path string) (value any, exists bool, err error)
}

type ValueResolverFunc func(any, MappingType, string) (any, bool, error)

func (f ValueResolverFunc) Resolve(input any, kind MappingType, path string) (any, bool, error) {
	return f(input, kind, path)
}

type MissingPolicy string

const (
	MissingSkip  MissingPolicy = "skip"
	MissingError MissingPolicy = "error"
)

type MappingOptions struct {
	MissingPolicy MissingPolicy
	Resolver      ValueResolver
}

// PathResolver supports plain dot paths, including numeric array indexes.
// GJSON expressions intentionally require an application-provided resolver.
type PathResolver struct{}

func (PathResolver) Resolve(input any, kind MappingType, path string) (any, bool, error) {
	if kind != "" && kind != MappingTypePath {
		return nil, false, fmt.Errorf("mapping type %q requires an external resolver", kind)
	}
	parts, err := parseFieldPath(path)
	if err != nil {
		return nil, false, err
	}
	value, exists := lookupData(input, parts)
	return value, exists, nil
}

// ApplyMappings parses each mapped subtree exactly once. It does not enforce
// required fields omitted by mappings; call ValidateModel for a complete model.
func ApplyMappings[P any](input any, mappings []Mapping, target Schema[P], params P, opts MappingOptions) (map[string]any, error) {
	if opts.MissingPolicy != "" && opts.MissingPolicy != MissingSkip && opts.MissingPolicy != MissingError {
		return nil, fmt.Errorf("unsupported missing policy %q", opts.MissingPolicy)
	}
	if err := ValidateMappings(mappings, target, nil); err != nil {
		return nil, err
	}
	normalized, err := normalizeMappingInput(input)
	if err != nil {
		return nil, err
	}
	if opts.Resolver == nil {
		opts.Resolver = PathResolver{}
	}
	out, err := mapScope(normalized, mappings, rootObjectField(target), opts, 0)
	if err != nil {
		return nil, err
	}
	return ValidatePartial(out, target, params)
}

func mapScope[P any](input any, mappings []Mapping, target Field[P], opts MappingOptions, depth int) (map[string]any, error) {
	if depth >= MaxSchemaDepth {
		return nil, invalidSchema("maximum mapping depth exceeded")
	}
	out := make(map[string]any)
	for i, mapping := range mappings {
		field, err := FindFieldInField(target, mapping.To)
		if err != nil {
			return nil, wrapPathError(joinIndexPath("mapping", i), err)
		}
		value, exists, err := opts.Resolver.Resolve(input, mapping.Type, mapping.From)
		if err != nil {
			return nil, wrapPathError(joinIndexPath("mapping", i), err)
		}
		if !exists {
			if opts.MissingPolicy == MissingError {
				return nil, fmt.Errorf("mapping[%d]: missing from %q", i, mapping.From)
			}
			continue
		}
		if len(mapping.Children) > 0 {
			if array, ok := field.(ArrayProvider[P]); ok {
				values, ok := asAnySlice(value)
				if !ok {
					return nil, fmt.Errorf("from %q is not an array", mapping.From)
				}
				items := make([]any, len(values))
				for index, item := range values {
					items[index], err = mapScope(item, mapping.Children, array.ItemField(), opts, depth+1)
					if err != nil {
						return nil, wrapPathError(joinIndexPath(mapping.To, index), err)
					}
				}
				value = items
			} else if _, ok := field.(ObjectProvider[P]); ok {
				if _, ok := asStringMap(value); !ok {
					return nil, fmt.Errorf("from %q is not an object", mapping.From)
				}
				value, err = mapScope(value, mapping.Children, field, opts, depth+1)
				if err != nil {
					return nil, wrapPathError(mapping.To, err)
				}
			} else {
				return nil, invalidSchema("to %q requires object or array", mapping.To)
			}
			// Parent parsing is responsible for parsing all descendants.
			// Do not parse these children during the mapping phase.
		}
		parts, _ := parseFieldPath(mapping.To)
		value, err = cloneMappingValue(value, 0)
		if err != nil {
			return nil, err
		}
		if err := SetData(out, parts, value); err != nil {
			return nil, fmt.Errorf("write to %q failed: %w", mapping.To, err)
		}
	}
	return out, nil
}

func cloneMappingValue(value any, depth int) (any, error) {
	if depth >= MaxSchemaDepth {
		return nil, fmt.Errorf("maximum mapping value depth exceeded")
	}
	if object, ok := asStringMap(value); ok {
		out := make(map[string]any, len(object))
		for key, child := range object {
			cloned, err := cloneMappingValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = cloned
		}
		return out, nil
	}
	if array, ok := asAnySlice(value); ok {
		out := make([]any, len(array))
		for i, child := range array {
			cloned, err := cloneMappingValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = cloned
		}
		return out, nil
	}
	return value, nil
}

func normalizeMappingInput(input any) (any, error) {
	switch value := input.(type) {
	case []byte:
		return decodeJSON(value)
	case json.RawMessage:
		return decodeJSON(value)
	case string:
		return decodeJSON([]byte(value))
	default:
		return input, nil
	}
}

func ValidateMappings[P any](mappings []Mapping, target Schema[P], source *Schema[P]) error {
	if err := ValidateSchema(target); err != nil {
		return err
	}
	var sourceField Field[P]
	if source != nil {
		if err := ValidateSchema(*source); err != nil {
			return err
		}
		sourceField = rootObjectField(*source)
	}
	return validateMappingsInScope(mappings, rootObjectField(target), sourceField, 0)
}

func ValidateMappingsWithSource[P any](mappings []Mapping, target Schema[P], source Schema[P]) error {
	return ValidateMappings(mappings, target, &source)
}

func validateMappingsInScope[P any](mappings []Mapping, target, source Field[P], depth int) error {
	if depth >= MaxSchemaDepth {
		return invalidSchema("maximum mapping depth exceeded")
	}
	for i, mapping := range mappings {
		fail := func(err error) error { return wrapPathError(joinIndexPath("mapping", i), err) }
		if strings.TrimSpace(mapping.From) == "" {
			return fail(invalidSchema("from is required"))
		}
		if strings.TrimSpace(mapping.To) == "" {
			return fail(invalidSchema("to is required"))
		}
		field, err := FindFieldInField(target, mapping.To)
		if err != nil {
			return fail(err)
		}
		if mapping.Type == "" || mapping.Type == MappingTypePath {
			if _, err := parseFieldPath(mapping.From); err != nil {
				return fail(err)
			}
		}
		var fromField Field[P]
		if !isNil(source) && ((mapping.Type == "" || mapping.Type == MappingTypePath) ||
			(mapping.Type == MappingTypeGJSON && isSimpleGJSONPath(mapping.From))) {
			fromField, err = FindFieldInField(source, mapping.From)
			if err != nil {
				return fail(err)
			}
		}
		if len(mapping.Children) == 0 {
			continue
		}
		childTarget, err := childScope(field)
		if err != nil {
			return fail(err)
		}
		var childSource Field[P]
		if !isNil(fromField) {
			childSource, err = childScope(fromField)
			if err != nil {
				return fail(err)
			}
		}
		if err := validateMappingsInScope(mapping.Children, childTarget, childSource, depth+1); err != nil {
			return fail(err)
		}
	}
	return nil
}

func childScope[P any](field Field[P]) (Field[P], error) {
	if array, ok := field.(ArrayProvider[P]); ok {
		if isNil(array.ItemField()) {
			return nil, invalidSchema("array schema missing items")
		}
		return array.ItemField(), nil
	}
	if _, ok := field.(ObjectProvider[P]); ok {
		return field, nil
	}
	return nil, invalidSchema("type %q requires object or array", TypeOf(field))
}

func isSimpleGJSONPath(path string) bool {
	return path != "" && !strings.ContainsAny(path, "#*?()[]{}|\\@")
}
