// Package xfield describes, parses, and presents extensible structured data.
// It depends only on the Go standard library.
package xfield

// Field is implemented by built-in and application-defined fields.
// Type is the stable JSON discriminator, independent of the Go type name.
// Parse must not modify its input. A nil value represents clearing a field.
type Field[P any] interface {
	Type() string
	Parse(data any, params P) (any, error)
}

// Meta contains shared field metadata. Required means that the key must exist;
// an explicitly supplied nil value is allowed by the built-in fields.
type Meta struct {
	ID       string `json:"id,omitempty"`
	Key      string `json:"key,omitempty"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// FieldMeta makes embedded Meta usable by external field implementations.
func (m *Meta) FieldMeta() *Meta { return m }

// MetaProvider allows fields to participate in keyed schemas.
// Embedding Meta implements this interface for pointer receivers.
type MetaProvider interface {
	FieldMeta() *Meta
}

// ObjectProvider exposes ordered child fields for validation and traversal.
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
	Present(data any, params P) any
}

func KeyOf[P any](field Field[P]) string {
	if m, ok := field.(MetaProvider); ok && !isNil(field) && m.FieldMeta() != nil {
		return m.FieldMeta().Key
	}
	return ""
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
