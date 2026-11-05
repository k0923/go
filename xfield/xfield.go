// Package xfield describes, parses, and presents extensible structured data.
// Declaration serialization is the caller's responsibility.
package xfield

import "iter"

// Field is implemented by built-in and application-defined fields.
// Type identifies the field kind, independent of the Go type name.
// Parse must not modify its input. A nil value represents clearing a field.
type Field[P any] interface {
	Type() string
	Parse(params P, data any) (any, error)
}

// Schema is a read-only collection of named field declarations.
// All must yield each name exactly once, with the same field returned by Get.
// The implementation owns iteration order and serialization. Declarations must
// remain unchanged while in use. A nil Schema is treated as an empty collection.
type Schema[P any] interface {
	Get(key string) (Field[P], bool)
	All() iter.Seq2[string, Field[P]]
}

// MaxSchemaDepth bounds schema validation, compilation and traversal.
const MaxSchemaDepth = 64

// Meta contains shared field metadata. Required means that the key must exist;
// an explicitly supplied nil value is allowed by the built-in fields.
type Meta struct {
	ID       string `json:"id,omitempty"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// FieldMeta makes embedded Meta usable by external field implementations.
func (m *Meta) FieldMeta() *Meta { return m }

// MetaProvider exposes metadata independently of the property's map key.
// Embedding Meta implements this interface for pointer receivers.
type MetaProvider interface {
	FieldMeta() *Meta
}

// ObjectProvider exposes keyed child fields for validation and traversal.
type ObjectProvider[P any] interface {
	ObjectFields() Schema[P]
}

// ArrayProvider exposes the element field for validation and traversal.
type ArrayProvider[P any] interface {
	ItemField() Field[P]
}

// SchemaValidator optionally validates a field's own rule configuration.
type SchemaValidator interface {
	ValidateSchema() error
}

// Presenter optionally converts stored values into application-facing values.
// params uses the same application-defined type as Field.Parse.
type Presenter[P any] interface {
	Present(params P, data any) any
}

func RequiredOf[P any](field Field[P]) bool {
	if m, ok := field.(MetaProvider); ok && !isNil(field) && m.FieldMeta() != nil {
		return m.FieldMeta().Required
	}
	return false
}

func TypeOf[P any](field Field[P]) string {
	if isNil(field) {
		return ""
	}
	return field.Type()
}
