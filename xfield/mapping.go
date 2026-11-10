package xfield

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Mapping resolves one source value and identifies where it is written in the
// target model. Resolve owns all source-side behavior, including how missing
// values are handled. Target and Children must remain unchanged while in use.
type Mapping interface {
	Target() []string
	Resolve(input any) (any, error)
	Children() []Mapping
}

type mapping struct {
	target   []string
	resolve  func(any) (any, error)
	children []Mapping
}

func (m *mapping) Target() []string    { return slices.Clone(m.target) }
func (m *mapping) Children() []Mapping { return slices.Clone(m.children) }

func (m *mapping) Resolve(input any) (any, error) {
	if m.resolve == nil {
		return nil, fmt.Errorf("mapping resolver is nil")
	}
	return m.resolve(input)
}

// PathMapping resolves a value from source using path segments. Missing source
// paths resolve to nil; callers that need another policy should use MappingFunc
// or implement Mapping directly.
func PathMapping(source, target []string, children ...Mapping) Mapping {
	source = slices.Clone(source)
	return &mapping{
		target:   slices.Clone(target),
		children: slices.Clone(children),
		resolve: func(input any) (any, error) {
			return GetData(input, source), nil
		},
	}
}

// MappingFunc constructs a Mapping from an application-defined resolver.
func MappingFunc(target []string, resolve func(any) (any, error), children ...Mapping) Mapping {
	return &mapping{
		target:   slices.Clone(target),
		resolve:  resolve,
		children: slices.Clone(children),
	}
}

// ApplyMappings parses each mapped subtree exactly once. It does not enforce
// required fields omitted by mappings; call ValidateModel for a complete model.
func ApplyMappings[P any](input any, mappings []Mapping, target Schema[P], params P) (map[string]any, error) {
	if err := ValidateMappings(mappings, target); err != nil {
		return nil, err
	}
	normalized, err := normalizeMappingInput(input)
	if err != nil {
		return nil, err
	}
	out, err := mapScope(normalized, mappings, rootObjectField(target), 0)
	if err != nil {
		return nil, err
	}
	return ValidatePartial(out, target, params)
}

func mapScope[P any](input any, mappings []Mapping, target Field[P], depth int) (map[string]any, error) {
	if depth >= MaxSchemaDepth {
		return nil, invalidSchema("maximum mapping depth exceeded")
	}
	out := make(map[string]any)
	for i, mapping := range mappings {
		targetPath := slices.Clone(mapping.Target())
		field, err := findFieldInField(target, targetPath, 0)
		if err != nil {
			return nil, wrapPathError(joinIndexPath("mapping", i), err)
		}
		value, err := mapping.Resolve(input)
		if err != nil {
			return nil, wrapPathError(joinIndexPath("mapping", i), err)
		}
		children := slices.Clone(mapping.Children())
		if len(children) > 0 {
			if array, ok := field.(ArrayProvider[P]); ok {
				values, ok := asAnySlice(value)
				if !ok {
					return nil, fmt.Errorf("mapping[%d] resolved value is not an array", i)
				}
				items := make([]any, len(values))
				for index, item := range values {
					items[index], err = mapScope(item, children, array.ItemField(), depth+1)
					if err != nil {
						return nil, wrapPathError(joinIndexPath(formatMappingPath(targetPath), index), err)
					}
				}
				value = items
			} else if _, ok := field.(ObjectProvider[P]); ok {
				if _, ok := asStringMap(value); !ok {
					return nil, fmt.Errorf("mapping[%d] resolved value is not an object", i)
				}
				value, err = mapScope(value, children, field, depth+1)
				if err != nil {
					return nil, wrapPathError(formatMappingPath(targetPath), err)
				}
			} else {
				return nil, invalidSchema("target %q requires object or array", formatMappingPath(targetPath))
			}
			// Parent parsing is responsible for parsing all descendants.
			// Do not parse these children during the mapping phase.
		}
		value, err = cloneMappingValue(value, 0)
		if err != nil {
			return nil, err
		}
		if err := SetData(out, targetPath, value); err != nil {
			return nil, fmt.Errorf("write to %q failed: %w", formatMappingPath(targetPath), err)
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

func ValidateMappings[P any](mappings []Mapping, target Schema[P]) error {
	if err := ValidateSchema(target); err != nil {
		return err
	}
	return validateMappingsInScope(mappings, rootObjectField(target), 0)
}

func validateMappingsInScope[P any](mappings []Mapping, target Field[P], depth int) error {
	if depth >= MaxSchemaDepth {
		return invalidSchema("maximum mapping depth exceeded")
	}
	for i, mapping := range mappings {
		fail := func(err error) error { return wrapPathError(joinIndexPath("mapping", i), err) }
		if isNil(mapping) {
			return fail(invalidSchema("mapping is nil"))
		}
		targetPath := slices.Clone(mapping.Target())
		if err := validateMappingPath(targetPath); err != nil {
			return fail(err)
		}
		field, err := findFieldInField(target, targetPath, 0)
		if err != nil {
			return fail(err)
		}
		children := slices.Clone(mapping.Children())
		if len(children) == 0 {
			continue
		}
		childTarget, err := childScope(field)
		if err != nil {
			return fail(err)
		}
		if err := validateMappingsInScope(children, childTarget, depth+1); err != nil {
			return fail(err)
		}
	}
	return nil
}

func validateMappingPath(path []string) error {
	if len(path) == 0 {
		return invalidSchema("target path is required")
	}
	if len(path) > MaxSchemaDepth {
		return invalidSchema("maximum target path depth exceeded")
	}
	for _, segment := range path {
		if strings.TrimSpace(segment) == "" {
			return invalidSchema("target path requires non-empty segments")
		}
	}
	return nil
}

func formatMappingPath(path []string) string {
	return strings.Join(path, ".")
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
