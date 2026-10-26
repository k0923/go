package xfield_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/k0923/go/xfield"
)

func dialogDocument() xfield.SchemaDocument[struct{}] {
	return xfield.SchemaDocument[struct{}]{
		Definitions: map[string]xfield.Field[struct{}]{
			"dialog": &xfield.ObjectField[struct{}]{
				Properties: xfield.Schema[struct{}]{
					"speaker": &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}},
					"start":   &xfield.NumberField[struct{}]{Meta: xfield.Meta{Required: true}},
					"end":     &xfield.NumberField[struct{}]{Meta: xfield.Meta{Required: true}},
					"content": &xfield.StringField[struct{}]{Meta: xfield.Meta{Required: true}},
				},
			},
		},
		Fields: xfield.Schema[struct{}]{
			"start": &xfield.NumberField[struct{}]{Meta: xfield.Meta{Required: true}},
			"end":   &xfield.NumberField[struct{}]{Meta: xfield.Meta{Required: true}},
			"tags": &xfield.ArrayField[struct{}]{
				Meta: xfield.Meta{Required: true},
				Items: &xfield.StringField[struct{}]{
					Enum: []string{"text", "role", "hallucination", "missing"},
				},
			},
			"dialog": &xfield.RefField[struct{}]{
				Meta: xfield.Meta{Label: "模型台词", Required: true},
				Ref:  "dialog",
			},
			"mappedDialogs": &xfield.ArrayField[struct{}]{
				Meta:  xfield.Meta{Required: true},
				Items: &xfield.RefField[struct{}]{Ref: "dialog"},
			},
		},
	}
}

func TestCompileDialogDocument(t *testing.T) {
	doc := dialogDocument()
	schema, err := xfield.CompileDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	dialog := map[string]any{"speaker": "role-1", "start": 1000, "end": 2000, "content": "你好", "extra": true}
	input := map[string]any{
		"start": 1000, "end": 2000, "tags": []any{"missing"},
		"dialog": nil, "mappedDialogs": []any{dialog, dialog},
	}
	clean, err := xfield.ValidateModel(input, schema, struct{}{})
	if err != nil || clean["dialog"] != nil || len(clean["mappedDialogs"].([]any)) != 2 {
		t.Fatalf("dialog result: %v, %v", clean, err)
	}
	if _, exists := clean["mappedDialogs"].([]any)[0].(map[string]any)["extra"]; exists {
		t.Fatal("unknown nested field was retained")
	}
	if _, exists := dialog["extra"]; !exists {
		t.Fatal("source input was mutated")
	}
	input["dialog"] = dialog
	if _, err := xfield.ValidateModel(input, schema, struct{}{}); err != nil {
		t.Fatal(err)
	}
	delete(input, "dialog")
	if _, err := xfield.ValidateModel(input, schema, struct{}{}); !errors.Is(err, xfield.ErrInvalidValue) {
		t.Fatalf("missing dialog: %v", err)
	}
	input["dialog"] = nil
	input["mappedDialogs"] = []any{map[string]any{"speaker": "role-1"}}
	if _, err := xfield.ValidateModel(input, schema, struct{}{}); err == nil || !strings.Contains(err.Error(), "mappedDialogs[0].content") {
		t.Fatalf("nested required path: %v", err)
	}
	input["mappedDialogs"] = []any{}
	input["tags"] = []any{"invalid"}
	if _, err := xfield.ValidateModel(input, schema, struct{}{}); !errors.Is(err, xfield.ErrInvalidValue) {
		t.Fatalf("invalid tag: %v", err)
	}
	if _, err := xfield.FindFieldByPath(schema, "mappedDialogs.0.content"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"dialog", "mappedDialogs.0"} {
		field, err := xfield.FindFieldByPath(schema, path)
		if err != nil || xfield.TypeOf(field) != "object" {
			t.Fatalf("resolved field %s: %v, %v", path, field, err)
		}
		if _, ok := field.(xfield.ArrayProvider[struct{}]); ok {
			t.Fatal("object reference exposes array children")
		}
	}
	if _, ok := doc.Fields["dialog"].(*xfield.RefField[struct{}]); !ok {
		t.Fatal("source reference was replaced")
	}
	if _, ok := doc.Fields["mappedDialogs"].(*xfield.ArrayField[struct{}]).Items.(*xfield.RefField[struct{}]); !ok {
		t.Fatal("source array item reference was replaced")
	}
	if doc.Definitions["dialog"].(xfield.MetaProvider).FieldMeta().Label != "" {
		t.Fatal("use-site metadata leaked into definition")
	}
	second, err := xfield.CompileDocument(doc)
	if err != nil || !xfield.RequiredOf(second["dialog"]) {
		t.Fatalf("repeated compilation: %v, %v", second, err)
	}
}

func TestReferencesPreserveCustomParsePresentAndMappings(t *testing.T) {
	doc := xfield.SchemaDocument[*dependencies]{
		Definitions: map[string]xfield.Field[*dependencies]{
			"resource": &resourceField{Meta: xfield.Meta{Required: true}},
			"imagesDef": &xfield.ArrayField[*dependencies]{
				Items: &xfield.RefField[*dependencies]{Ref: "resource"},
			},
			"alias": &xfield.RefField[*dependencies]{Ref: "imagesDef"},
		},
		Fields: xfield.Schema[*dependencies]{
			"optional": &xfield.RefField[*dependencies]{Ref: "resource"},
			"images":   &xfield.RefField[*dependencies]{Meta: xfield.Meta{Required: true}, Ref: "alias"},
		},
	}
	schema, err := xfield.CompileDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	deps := &dependencies{ctx: context.Background(), displayPrefix: "https://resources/"}
	clean, err := xfield.ValidateModel(map[string]any{"images": []any{"a", "b"}}, schema, deps)
	if err != nil || deps.calls != 2 {
		t.Fatalf("custom parse: %v, %v, calls=%d", clean, err, deps.calls)
	}
	display := xfield.PresentModel(clean, schema, deps)
	if deps.presentCalls != 2 || display["images"].([]any)[1] != "https://resources/b" {
		t.Fatalf("custom present: %v, calls=%d", display, deps.presentCalls)
	}
	field, err := xfield.FindFieldByPath(schema, "images.0")
	if err != nil || xfield.TypeOf(field) != "resource" {
		t.Fatalf("array alias traversal: %v, %v", field, err)
	}
	if _, ok := schema["images"].(xfield.ObjectProvider[*dependencies]); ok {
		t.Fatal("array reference exposes object children")
	}
	mapped, err := xfield.ApplyMappings(
		map[string]any{"source": []any{"c"}},
		[]xfield.Mapping{{From: "source", To: "images"}},
		schema, deps, xfield.MappingOptions{},
	)
	if err != nil || mapped["images"].([]any)[0] != "stored:c" || deps.calls != 3 {
		t.Fatalf("custom mapping: %v, %v, calls=%d", mapped, err, deps.calls)
	}
	sentinel := errors.New("unavailable")
	deps.err = sentinel
	_, err = xfield.ValidateModel(map[string]any{"images": []any{"a"}}, schema, deps)
	var path *xfield.PathError
	if !errors.Is(err, sentinel) || !errors.As(err, &path) || path.Path != "images[0]" {
		t.Fatalf("reference error path: %v", err)
	}
}

func TestCompileDocumentRejectsInvalidDeclarations(t *testing.T) {
	type field = xfield.Field[struct{}]
	type ref = xfield.RefField[struct{}]
	tests := map[string]xfield.SchemaDocument[struct{}]{
		"empty definition name": {Definitions: map[string]field{"": &xfield.StringField[struct{}]{}}},
		"whitespace name":       {Definitions: map[string]field{"bad name": &xfield.StringField[struct{}]{}}},
		"nil definition":        {Definitions: map[string]field{"bad": (*xfield.StringField[struct{}])(nil)}},
		"missing reference":     {Fields: xfield.Schema[struct{}]{"dialog": &ref{Ref: "missing"}}},
		"empty reference":       {Fields: xfield.Schema[struct{}]{"dialog": &ref{}}},
		"self reference":        {Definitions: map[string]field{"aDef": &ref{Ref: "aDef"}}},
		"indirect reference": {Definitions: map[string]field{
			"aDef": &ref{Ref: "bDef"},
			"bDef": &xfield.ArrayField[struct{}]{Items: &ref{Ref: "aDef"}},
		}},
		"unused invalid definition": {Definitions: map[string]field{
			"bad": &xfield.ObjectField[struct{}]{Properties: xfield.Schema[struct{}]{"": &xfield.StringField[struct{}]{}}},
		}},
		"empty property name": {Fields: xfield.Schema[struct{}]{"": &xfield.StringField[struct{}]{}}},
		"missing array items": {Fields: xfield.Schema[struct{}]{"items": &xfield.ArrayField[struct{}]{}}},
	}
	cycle := &xfield.ArrayField[struct{}]{}
	cycle.Items = cycle
	tests["pointer cycle"] = xfield.SchemaDocument[struct{}]{Fields: xfield.Schema[struct{}]{"cycle": cycle}}
	var deep field = &xfield.StringField[struct{}]{}
	for range xfield.MaxSchemaDepth {
		deep = &xfield.ArrayField[struct{}]{Items: deep}
	}
	tests["deep definition"] = xfield.SchemaDocument[struct{}]{Definitions: map[string]field{"deep": deep}}
	aliases := make(map[string]field)
	for i := range xfield.MaxSchemaDepth + 1 {
		aliases[fmt.Sprintf("%03d", i)] = &ref{Ref: fmt.Sprintf("%03d", i+1)}
	}
	aliases[fmt.Sprintf("%03d", xfield.MaxSchemaDepth+1)] = &xfield.StringField[struct{}]{}
	tests["deep aliases"] = xfield.SchemaDocument[struct{}]{Definitions: aliases}
	cachedAliases := map[string]field{"000": &xfield.StringField[struct{}]{}}
	for i := 1; i <= xfield.MaxSchemaDepth; i++ {
		cachedAliases[fmt.Sprintf("%03d", i)] = &ref{Ref: fmt.Sprintf("%03d", i-1)}
	}
	tests["deep cached aliases"] = xfield.SchemaDocument[struct{}]{Definitions: cachedAliases}
	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := xfield.CompileDocument(doc); !errors.Is(err, xfield.ErrInvalidSchema) {
				t.Fatalf("invalid declaration was accepted: %v", err)
			}
		})
	}
	refField := &ref{Ref: "dialog"}
	if err := xfield.ValidateSchema(xfield.Schema[struct{}]{"refField": refField}); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("uncompiled schema: %v", err)
	}
	if _, err := refField.Parse(nil, struct{}{}); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("uncompiled reference parse: %v", err)
	}
}

type customReferenceObject struct {
	xfield.ObjectField[*dependencies]
	Marker string
}

func (*customReferenceObject) Type() string { return "custom_reference_object" }
func (f *customReferenceObject) Parse(value any, params *dependencies) (any, error) {
	parsed, err := f.ObjectField.Parse(value, params)
	if err == nil && parsed != nil {
		parsed.(map[string]any)["marker"] = f.Marker
	}
	return parsed, err
}
func (f *customReferenceObject) CompileField(compile func(xfield.Field[*dependencies]) (xfield.Field[*dependencies], error)) (xfield.Field[*dependencies], error) {
	copy := *f
	copy.Properties = make(xfield.Schema[*dependencies], len(f.Properties))
	for i, child := range f.Properties {
		compiled, err := compile(child)
		if err != nil {
			return nil, err
		}
		copy.Properties[i] = compiled
	}
	return &copy, nil
}

func TestCustomContainerCompiler(t *testing.T) {
	container := &customReferenceObject{
		ObjectField: xfield.ObjectField[*dependencies]{
			Properties: xfield.Schema[*dependencies]{
				"image": &xfield.RefField[*dependencies]{Ref: "resource"},
			},
		},
		Marker: "preserved",
	}
	doc := xfield.SchemaDocument[*dependencies]{
		Definitions: map[string]xfield.Field[*dependencies]{
			"resource": &resourceField{},
			"assetDef": container,
		},
		Fields: xfield.Schema[*dependencies]{
			"asset": &xfield.RefField[*dependencies]{Ref: "assetDef"},
		},
	}
	schema, err := xfield.CompileDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	deps := &dependencies{ctx: context.Background()}
	mapped, err := xfield.ApplyMappings(
		map[string]any{"source": map[string]any{"url": "a"}},
		[]xfield.Mapping{{From: "source", To: "asset", Children: []xfield.Mapping{{From: "url", To: "image"}}}},
		schema, deps, xfield.MappingOptions{},
	)
	if err != nil || mapped["asset"].(map[string]any)["marker"] != "preserved" || deps.calls != 1 {
		t.Fatalf("custom container behavior: %v, %v, calls=%d", mapped, err, deps.calls)
	}
	if _, ok := container.Properties["image"].(*xfield.RefField[*dependencies]); !ok {
		t.Fatal("custom source container was mutated")
	}
}

func TestCompileDocumentConcurrent(t *testing.T) {
	doc := dialogDocument()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			schema, err := xfield.CompileDocument(doc)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := xfield.FindFieldByPath(schema, "dialog.content"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
