package xfield

import (
	"fmt"
	"iter"
)

// ValidateSchema checks keys, nested items and rule configuration.
// Invalid declarations are rejected even when corresponding data is absent.
func ValidateSchema[P any](schema Schema[P]) error { return validateSchema(schema, 0) }

func ValidateField[P any](field Field[P]) error { return validateField(field, 0) }

func validateSchema[P any](schema Schema[P], depth int) error {
	if depth >= MaxSchemaDepth {
		return invalidSchema("maximum schema depth exceeded")
	}
	seen := make(map[string]bool)
	for key, field := range schemaAll(schema) {
		if key == "" {
			return invalidSchema("property name is empty")
		}
		if seen[key] {
			return wrapPathError(key, invalidSchema("duplicate property name"))
		}
		seen[key] = true
		if err := validateField(field, depth); err != nil {
			return wrapPathError(key, err)
		}
	}
	return nil
}

func validateField[P any](field Field[P], depth int) error {
	if depth >= MaxSchemaDepth {
		return invalidSchema("maximum schema depth exceeded")
	}
	if isNil(field) || field.Type() == "" {
		return invalidSchema("field is nil or missing type")
	}
	if ref, ok := field.(ReferenceProvider); ok {
		return invalidSchema("unresolved reference %q; compile the document first", ref.ReferenceName())
	}
	if ref, ok := field.(interface{ referenceHeight() int }); ok {
		// CompileDocument already validated the immutable target. Reusing that
		// result prevents repeated references from expanding validation work.
		if depth+ref.referenceHeight() > MaxSchemaDepth {
			return invalidSchema("maximum schema depth exceeded")
		}
		return nil
	}
	if validator, ok := field.(SchemaValidator); ok {
		if err := validator.ValidateSchema(); err != nil {
			return err
		}
	}
	object, isObject := field.(ObjectProvider[P])
	array, isArray := field.(ArrayProvider[P])
	if isObject && isArray {
		return invalidSchema("field cannot expose both object and array children")
	}
	if isObject {
		return validateSchema(object.ObjectFields(), depth+1)
	}
	if isArray {
		return wrapPathError("items", validateField(array.ItemField(), depth+1))
	}
	return nil
}

func ValidateModel[P any](data map[string]any, model Schema[P], params P) (map[string]any, error) {
	return parseModel(data, model, false, params)
}

// ValidatePartial skips required checks only at the top level. Supplied nested
// objects still have to satisfy their complete object schema.
func ValidatePartial[P any](data map[string]any, model Schema[P], params P) (map[string]any, error) {
	return parseModel(data, model, true, params)
}

func parseModel[P any](data map[string]any, model Schema[P], partial bool, params P) (map[string]any, error) {
	if err := ValidateSchema(model); err != nil {
		return nil, err
	}
	return parseProperties(data, model, partial, params)
}

func parseProperties[P any](data map[string]any, model Schema[P], partial bool, params P) (map[string]any, error) {
	result := make(map[string]any, len(data))
	// The caller's schema determines parsing order and the first error.
	for key, field := range schemaAll(model) {
		value, exists := data[key]
		if !exists {
			if !partial && RequiredOf(field) {
				return nil, wrapPathError(key, fmt.Errorf("%w: required", ErrInvalidValue))
			}
			continue
		}
		parsed, err := field.Parse(params, value)
		if err != nil {
			return nil, wrapPathError(key, err)
		}
		result[key] = parsed
	}
	return result, nil
}

func schemaAll[P any](schema Schema[P]) iter.Seq2[string, Field[P]] {
	return func(yield func(string, Field[P]) bool) {
		if !isNil(schema) {
			schema.All()(yield)
		}
	}
}
