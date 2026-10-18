package xfield

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sync"
)

// FieldRef preserves the flat {"type": "...", ...} JSON representation.
type FieldRef[P any] struct{ field Field[P] }

type Schema[P any] []FieldRef[P]

func Wrap[P any](field Field[P]) FieldRef[P] { return FieldRef[P]{field: field} }
func (r FieldRef[P]) Value() Field[P]        { return r.field }
func (r FieldRef[P]) TypeName() string       { return TypeOf(r.field) }

func (s Schema[P]) Values() []Field[P] {
	out := make([]Field[P], len(s))
	for i, ref := range s {
		out[i] = ref.Value()
	}
	return out
}

func (r FieldRef[P]) MarshalJSON() ([]byte, error) {
	if isNil(r.field) {
		return []byte("null"), nil
	}
	if r.field.Type() == "" {
		return nil, invalidSchema("field type is empty")
	}
	raw, err := json.Marshal(r.field)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, invalidSchema("field must encode as a JSON object")
	}
	discriminator, _ := json.Marshal(r.field.Type())
	if existing, ok := object["type"]; ok && !bytes.Equal(existing, discriminator) {
		return nil, invalidSchema("field encodes a conflicting type discriminator")
	}
	object["type"] = discriminator
	return json.Marshal(object)
}

// UnmarshalJSON supports built-in types. Use Registry.DecodeSchema or
// Registry.DecodeField when application-defined types are present.
func (r *FieldRef[P]) UnmarshalJSON(raw []byte) error {
	field, err := DefaultRegistry[P]().DecodeField(raw)
	if err != nil {
		return err
	}
	r.field = field
	return nil
}

// Decoder can be replaced by an application-specific schema codec.
type Decoder[P any] interface {
	DecodeField(json.RawMessage) (Field[P], error)
	DecodeSchema([]byte) (Schema[P], error)
}

// FieldDecoder optionally decodes configuration into an existing field.
// Containers must use decoder for children to retain registrations and depth
// limits. Fields without this interface use standard JSON decoding.
type FieldDecoder[P any] interface {
	DecodeFieldJSON(raw json.RawMessage, decoder Decoder[P]) error
}

// Factory creates a fresh field. It is called once during registration to
// obtain Type(), then on every decode. It must be lightweight, have no business
// side effects and return a fresh instance with a stable, non-empty Type().
type Factory[P any] func() Field[P]

// Registry owns its registrations. There is no mutable process-global registry.
// Registration and decoding are safe to call concurrently. Factories must be
// safe for concurrent calls and return fresh field instances.
type Registry[P any] struct {
	mu        sync.RWMutex
	factories map[string]Factory[P]
}

// NewRegistry returns an empty registry. The zero value is also usable.
func NewRegistry[P any]() *Registry[P] {
	return &Registry[P]{factories: make(map[string]Factory[P])}
}

// DefaultRegistry returns an independent registry with all five built-in types.
func DefaultRegistry[P any]() *Registry[P] {
	r := NewRegistry[P]()
	if err := r.RegisterBuiltins(); err != nil {
		panic(err) // Fresh registry and fixed built-in factories cannot conflict.
	}
	return r
}

// RegisterBuiltins adds all five built-ins atomically. If any type is already
// registered it returns an error without adding any types.
func (r *Registry[P]) RegisterBuiltins() error {
	return r.register(
		func() Field[P] { return &StringField[P]{} },
		func() Field[P] { return &NumberField[P]{} },
		func() Field[P] { return &BooleanField[P]{} },
		func() Field[P] { return &ArrayField[P]{} },
		func() Field[P] { return &ObjectField[P]{} },
	)
}

func (r *Registry[P]) Register(factory Factory[P]) error {
	return r.register(factory)
}

func (r *Registry[P]) register(factories ...Factory[P]) error {
	entries := make(map[string]Factory[P], len(factories))
	// User code is called outside the lock.
	for _, factory := range factories {
		if factory == nil {
			return invalidSchema("registration requires a factory")
		}
		field := factory()
		name := TypeOf(field)
		if name == "" {
			return invalidSchema("factory returned nil or empty field type")
		}
		if _, exists := entries[name]; exists {
			return invalidSchema("type %q already registered", name)
		}
		entries[name] = factory
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range entries {
		if _, exists := r.factories[name]; exists {
			return invalidSchema("type %q already registered", name)
		}
	}
	if r.factories == nil {
		r.factories = make(map[string]Factory[P])
	}
	for name, factory := range entries {
		r.factories[name] = factory
	}
	return nil
}

// MaxSchemaDepth bounds decoded and validated schema nesting.
const MaxSchemaDepth = 64

type depthDecoder[P any] struct {
	registry *Registry[P]
	depth    int
}

func (r *Registry[P]) DecodeField(raw json.RawMessage) (Field[P], error) {
	return (depthDecoder[P]{registry: r}).DecodeField(raw)
}

func (r *Registry[P]) DecodeSchema(raw []byte) (Schema[P], error) {
	return (depthDecoder[P]{registry: r}).DecodeSchema(raw)
}

func (d depthDecoder[P]) DecodeField(raw json.RawMessage) (Field[P], error) {
	if d.depth >= MaxSchemaDepth {
		return nil, invalidSchema("maximum schema depth exceeded")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, err
	}
	d.registry.mu.RLock()
	factory := d.registry.factories[header.Type]
	d.registry.mu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, header.Type)
	}
	field := factory()
	if isNil(field) || field.Type() != header.Type {
		return nil, invalidSchema("factory for %q returned incompatible field", header.Type)
	}
	var err error
	if custom, ok := field.(FieldDecoder[P]); ok {
		err = custom.DecodeFieldJSON(raw, depthDecoder[P]{registry: d.registry, depth: d.depth + 1})
	} else {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err = decoder.Decode(field)
	}
	if err != nil {
		return nil, err
	}
	if isNil(field) || field.Type() != header.Type {
		return nil, invalidSchema("factory for %q returned incompatible field", header.Type)
	}
	return field, nil
}

func (d depthDecoder[P]) DecodeSchema(raw []byte) (Schema[P], error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, nil
	}
	schema := make(Schema[P], len(entries))
	for i, entry := range entries {
		field, err := d.DecodeField(entry)
		if err != nil {
			return nil, wrapPathError(joinIndexPath("", i), err)
		}
		schema[i] = Wrap(field)
	}
	return schema, nil
}

// FieldEqual compares normalized JSON without converting numbers to float64.
func FieldEqual[P any](a, b Field[P]) bool {
	normalize := func(field Field[P]) ([]byte, error) {
		raw, err := json.Marshal(Wrap(field))
		if err != nil {
			return nil, err
		}
		value, err := decodeJSON(raw)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	left, err := normalize(a)
	if err != nil {
		return false
	}
	right, err := normalize(b)
	return err == nil && bytes.Equal(left, right)
}

func decodeJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}
