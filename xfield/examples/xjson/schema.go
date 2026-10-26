package main

import (
	"fmt"

	xjson "github.com/k0923/go/json"
	"github.com/k0923/go/xfield"
)

type Params struct{}

// FieldConfig is the application's JSON interface. Runtime declarations use
// xfield.Field; conversion is explicit and can report duplicate-name errors.
type FieldConfig interface {
	FieldKey() string
	BuildField(depth int) (xfield.Field[Params], error)
}

type FieldJSON = xjson.G[FieldConfig]
type SchemaJSON []FieldJSON

type DefinitionJSON struct {
	Name  string    `json:"name"`
	Field FieldJSON `json:"field"`
}

type DocumentJSON struct {
	Definitions []DefinitionJSON `json:"definitions,omitempty"`
	Fields      SchemaJSON       `json:"fields"`
}

// KeyJSON exists only in the frontend format. Runtime property names are the
// keys of xfield.Schema, so fields do not hold a second copy of their name.
type KeyJSON struct {
	Key string `json:"key,omitempty"`
}

func (k KeyJSON) FieldKey() string { return k.Key }

func (s SchemaJSON) Fields(depth int) (xfield.Schema[Params], error) {
	if depth >= xfield.MaxSchemaDepth {
		return nil, configError("maximum schema depth exceeded")
	}
	if s == nil {
		return nil, nil
	}
	fields := make(xfield.Schema[Params], len(s))
	for i, wrapped := range s {
		config := wrapped.Value()
		if config == nil || config.FieldKey() == "" {
			return nil, configError("[%d]: field is null or missing key", i)
		}
		key := config.FieldKey()
		if _, exists := fields[key]; exists {
			return nil, configError("[%d]: duplicate property key %q", i, key)
		}
		field, err := buildField(wrapped, depth)
		if err != nil {
			return nil, fmt.Errorf("[%d].%s: %w", i, key, err)
		}
		fields[key] = field
	}
	return fields, nil
}

func (d DocumentJSON) Document() (xfield.SchemaDocument[Params], error) {
	doc := xfield.SchemaDocument[Params]{
		Definitions: make(map[string]xfield.Field[Params], len(d.Definitions)),
	}
	for i, definition := range d.Definitions {
		if definition.Name == "" {
			return doc, configError("definitions[%d]: missing name", i)
		}
		if _, exists := doc.Definitions[definition.Name]; exists {
			return doc, configError("definitions[%d]: duplicate definition name %q", i, definition.Name)
		}
		field, err := buildField(definition.Field, 0)
		if err != nil {
			return doc, fmt.Errorf("definitions[%d].field: %w", i, err)
		}
		doc.Definitions[definition.Name] = field
	}
	fields, err := d.Fields.Fields(0)
	if err != nil {
		return doc, fmt.Errorf("fields: %w", err)
	}
	doc.Fields = fields
	return doc, nil
}

func configError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", xfield.ErrInvalidSchema, fmt.Sprintf(format, args...))
}

func buildField(wrapped FieldJSON, depth int) (xfield.Field[Params], error) {
	if depth >= xfield.MaxSchemaDepth {
		return nil, configError("maximum schema depth exceeded")
	}
	if wrapped.Value() == nil {
		return nil, configError("field is null")
	}
	return wrapped.Value().BuildField(depth)
}

// Leaf adapters reuse xfield rule structs. Their key is frontend metadata only.
type StringJSON struct {
	xfield.StringField[Params]
	KeyJSON
}
type NumberJSON struct {
	xfield.NumberField[Params]
	KeyJSON
}
type BooleanJSON struct {
	xfield.BooleanField[Params]
	KeyJSON
}

func (f *StringJSON) BuildField(_ int) (xfield.Field[Params], error) {
	field := f.StringField
	return &field, nil
}
func (f *NumberJSON) BuildField(_ int) (xfield.Field[Params], error) {
	field := f.NumberField
	return &field, nil
}
func (f *BooleanJSON) BuildField(_ int) (xfield.Field[Params], error) {
	field := f.BooleanField
	return &field, nil
}

type ObjectJSON struct {
	xfield.Meta
	KeyJSON
	Properties SchemaJSON `json:"properties,omitempty"`
}

type ArrayJSON struct {
	xfield.Meta
	KeyJSON
	Items    FieldJSON `json:"items"`
	MinItems *int      `json:"minItems,omitempty"`
	MaxItems *int      `json:"maxItems,omitempty"`
}

type RefJSON struct {
	xfield.Meta
	KeyJSON
	Ref string `json:"ref"`
}

func (f *ObjectJSON) BuildField(depth int) (xfield.Field[Params], error) {
	properties, err := f.Properties.Fields(depth + 1)
	if err != nil {
		return nil, fmt.Errorf("properties: %w", err)
	}
	return &xfield.ObjectField[Params]{Meta: f.Meta, Properties: properties}, nil
}
func (f *ArrayJSON) BuildField(depth int) (xfield.Field[Params], error) {
	item, err := buildField(f.Items, depth+1)
	if err != nil {
		return nil, fmt.Errorf("items: %w", err)
	}
	return &xfield.ArrayField[Params]{
		Meta: f.Meta, Items: item, MinItems: f.MinItems, MaxItems: f.MaxItems,
	}, nil
}
func (f *RefJSON) BuildField(_ int) (xfield.Field[Params], error) {
	return &xfield.RefField[Params]{Meta: f.Meta, Ref: f.Ref}, nil
}

func init() {
	// Bind once per application interface type. User definitions are data,
	// not additional entries in this global concrete-type binding.
	xjson.Bind[FieldConfig](map[string]FieldConfig{
		"string": &StringJSON{}, "number": &NumberJSON{}, "boolean": &BooleanJSON{},
		"object": &ObjectJSON{}, "array": &ArrayJSON{}, "ref": &RefJSON{},
	}, xjson.WithFlatLayout())
}
