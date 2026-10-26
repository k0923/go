package xfield

import (
	"sort"
	"strings"
)

// SchemaDocument holds reusable definitions and keyed use-site fields.
// Serialization and loading definitions from storage belong to the caller.
type SchemaDocument[P any] struct {
	Definitions map[string]Field[P]
	Fields      Schema[P]
}

// ReferenceProvider exposes a reusable definition's qualified name.
type ReferenceProvider interface {
	ReferenceName() string
}

// RefField carries use-site metadata and refers to a document definition.
// CompileDocument must resolve references before parsing or traversal.
type RefField[P any] struct {
	Meta
	Ref string
}

func (*RefField[P]) Type() string            { return "ref" }
func (f *RefField[P]) ReferenceName() string { return f.Ref }
func (f *RefField[P]) ValidateSchema() error { return f.unresolved() }
func (f *RefField[P]) Parse(_ any, _ P) (any, error) {
	return nil, f.unresolved()
}

func (f *RefField[P]) unresolved() error {
	return invalidSchema("unresolved reference %q; compile the document first", f.Ref)
}

// FieldCompiler lets a custom container rebuild itself with compiled children.
// It must preserve its type, metadata, rules, Parse and Present behavior, and
// leave the source declaration unchanged. Invoke compile for every child.
// Custom leaf fields and containers without references need no implementation.
type FieldCompiler[P any] interface {
	CompileField(compile func(Field[P]) (Field[P], error)) (Field[P], error)
}

// CompileDocument resolves references without mutating the source document.
// It checks every definition, including unused ones, and rejects missing or
// circular references. Definition keys must be non-empty and contain no
// whitespace.
// Compiled declarations may share immutable leaf fields and definitions.
func CompileDocument[P any](doc SchemaDocument[P]) (Schema[P], error) {
	c := documentCompiler[P]{
		definitions: doc.Definitions,
		compiled:    make(map[string]compiledDefinition[P], len(doc.Definitions)),
		visiting:    make(map[string]bool),
	}
	names := make([]string, 0, len(doc.Definitions))
	for name := range doc.Definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !validDefinitionName(name) {
			return nil, wrapPathError("definitions."+name, invalidSchema("definition name must be non-empty and contain no whitespace"))
		}
	}
	for _, name := range names {
		if _, err := c.definition(name, 0); err != nil {
			return nil, err
		}
	}
	fields, err := c.schema(doc.Fields, 0)
	if err != nil {
		return nil, wrapPathError("fields", err)
	}
	if err := ValidateSchema(fields); err != nil {
		return nil, wrapPathError("fields", err)
	}
	return fields, nil
}

func validDefinitionName(name string) bool {
	return name != "" && !strings.ContainsAny(name, " \t\r\n")
}

type documentCompiler[P any] struct {
	definitions map[string]Field[P]
	compiled    map[string]compiledDefinition[P]
	visiting    map[string]bool
}

type compiledDefinition[P any] struct {
	field  Field[P]
	height int
}

func (c *documentCompiler[P]) definition(name string, depth int) (Field[P], error) {
	if !validDefinitionName(name) {
		return nil, invalidSchema("reference name must be non-empty and contain no whitespace: %q", name)
	}
	if c.visiting[name] {
		return nil, invalidSchema("circular reference %q", name)
	}
	if compiled, ok := c.compiled[name]; ok {
		if depth+compiled.height > MaxSchemaDepth {
			return nil, invalidSchema("maximum schema depth exceeded")
		}
		return compiled.field, nil
	}
	field, ok := c.definitions[name]
	if !ok {
		return nil, invalidSchema("unknown reference %q", name)
	}
	c.visiting[name] = true
	compiled, err := c.field(field, depth)
	delete(c.visiting, name)
	if err != nil {
		return nil, wrapPathError("definitions."+name, err)
	}
	height, err := compiledFieldHeight(compiled, 0)
	if err != nil {
		return nil, wrapPathError("definitions."+name, err)
	}
	if depth+height > MaxSchemaDepth {
		return nil, invalidSchema("maximum schema depth exceeded")
	}
	if err := ValidateField(compiled); err != nil {
		return nil, wrapPathError("definitions."+name, err)
	}
	c.compiled[name] = compiledDefinition[P]{field: compiled, height: height}
	return compiled, nil
}

func (c *documentCompiler[P]) schema(schema Schema[P], depth int) (Schema[P], error) {
	if depth >= MaxSchemaDepth {
		return nil, invalidSchema("maximum schema depth exceeded")
	}
	if schema == nil {
		return nil, nil
	}
	out := make(Schema[P], len(schema))
	for _, key := range schemaKeys(schema) {
		compiled, err := c.field(schema[key], depth)
		if err != nil {
			return nil, wrapPathError(key, err)
		}
		out[key] = compiled
	}
	return out, nil
}

func (c *documentCompiler[P]) field(field Field[P], depth int) (Field[P], error) {
	if depth >= MaxSchemaDepth {
		return nil, invalidSchema("maximum schema depth exceeded")
	}
	if isNil(field) || field.Type() == "" {
		return nil, invalidSchema("field is nil or missing type")
	}
	_, isObject := field.(ObjectProvider[P])
	_, isArray := field.(ArrayProvider[P])
	if isObject && isArray {
		return nil, invalidSchema("field cannot expose both object and array children")
	}
	if ref, ok := field.(ReferenceProvider); ok {
		target, err := c.definition(ref.ReferenceName(), depth+1)
		if err != nil {
			return nil, err
		}
		var meta Meta
		if provider, ok := field.(MetaProvider); ok && provider.FieldMeta() != nil {
			meta = *provider.FieldMeta()
		}
		return bindReference(meta, target, c.compiled[ref.ReferenceName()].height+1), nil
	}
	if custom, ok := field.(FieldCompiler[P]); ok {
		compiled, err := custom.CompileField(func(child Field[P]) (Field[P], error) {
			return c.field(child, depth+1)
		})
		if err != nil {
			return nil, err
		}
		if isNil(compiled) || compiled.Type() != field.Type() {
			return nil, invalidSchema("custom compiler must preserve field type %q", field.Type())
		}
		return compiled, nil
	}
	switch field := field.(type) {
	case *ObjectField[P]:
		copy := *field
		properties, err := c.schema(field.Properties, depth+1)
		if err != nil {
			return nil, err
		}
		copy.Properties = properties
		return &copy, nil
	case *ArrayField[P]:
		copy := *field
		item, err := c.field(field.Items, depth+1)
		if err != nil {
			return nil, wrapPathError("items", err)
		}
		copy.Items = item
		return &copy, nil
	default:
		// Arbitrary application objects cannot be cloned and rebuilt safely.
		// ValidateSchema rejects unresolved RefFields in custom containers.
		return field, nil
	}
}

// boundReference gives a definition use-site metadata without overwriting the
// definition or hiding its custom parsing, presentation and container behavior.
type boundReference[P any] struct {
	Meta
	target Field[P]
	height int
}

func (f *boundReference[P]) Type() string         { return f.target.Type() }
func (f *boundReference[P]) referenceHeight() int { return f.height }
func (f *boundReference[P]) Parse(value any, params P) (any, error) {
	return f.target.Parse(value, params)
}
func (f *boundReference[P]) Present(value any, params P) any {
	return presentFieldValue(f.target, value, params)
}
func (f *boundReference[P]) ValidateSchema() error {
	if validator, ok := f.target.(SchemaValidator); ok {
		return validator.ValidateSchema()
	}
	return nil
}

type boundObjectReference[P any] struct {
	*boundReference[P]
	ObjectProvider[P]
}

type boundArrayReference[P any] struct {
	*boundReference[P]
	ArrayProvider[P]
}

func bindReference[P any](meta Meta, target Field[P], height int) Field[P] {
	ref := &boundReference[P]{Meta: meta, target: target, height: height}
	if object, ok := target.(ObjectProvider[P]); ok {
		return &boundObjectReference[P]{boundReference: ref, ObjectProvider: object}
	}
	if array, ok := target.(ArrayProvider[P]); ok {
		return &boundArrayReference[P]{boundReference: ref, ArrayProvider: array}
	}
	return ref
}

// Cached reference heights prevent memoization from bypassing depth limits.
func compiledFieldHeight[P any](field Field[P], depth int) (int, error) {
	if depth >= MaxSchemaDepth {
		return 0, invalidSchema("maximum schema depth exceeded")
	}
	if isNil(field) {
		return 0, invalidSchema("field is nil")
	}
	if ref, ok := field.(interface{ referenceHeight() int }); ok {
		return ref.referenceHeight(), nil
	}
	height := 1
	if object, ok := field.(ObjectProvider[P]); ok {
		for _, child := range object.ObjectFields() {
			childHeight, err := compiledFieldHeight(child, depth+1)
			if err != nil {
				return 0, err
			}
			height = max(height, childHeight+1)
		}
	} else if array, ok := field.(ArrayProvider[P]); ok {
		childHeight, err := compiledFieldHeight(array.ItemField(), depth+1)
		if err != nil {
			return 0, err
		}
		height = childHeight + 1
	}
	return height, nil
}
