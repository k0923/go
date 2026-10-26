package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	xjson "github.com/k0923/go/json"
	"github.com/k0923/go/xfield"
)

func TestDialogJSONRoundTrip(t *testing.T) {
	var wire DocumentJSON
	if err := json.Unmarshal(schemaJSON, &wire); err != nil {
		t.Fatal(err)
	}
	doc, err := wire.Document()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := xfield.CompileDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		t.Fatal(err)
	}
	clean, err := xfield.ValidateModel(input, schema, Params{})
	if err != nil {
		t.Fatal(err)
	}
	if clean["dialog"] != nil || len(clean["mappedDialogs"].([]any)) != 2 {
		t.Fatalf("null or multiple dialogs were lost: %v", clean)
	}
	if _, exists := clean["extra"]; exists {
		t.Fatal("unknown root field was retained")
	}
	if _, exists := clean["mappedDialogs"].([]any)[0].(map[string]any)["extra"]; exists {
		t.Fatal("unknown nested field was retained")
	}
	if _, exists := input["mappedDialogs"].([]any)[0].(map[string]any)["extra"]; !exists {
		t.Fatal("source input was modified")
	}
	field, err := xfield.FindFieldByPath(schema, "mappedDialogs.0.content")
	if err != nil || field.Type() != "string" || !xfield.RequiredOf(field) {
		t.Fatalf("nested interface was not restored: %v, %v", field, err)
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Definitions []struct {
			Name  string          `json:"name"`
			Field json.RawMessage `json:"field"`
		} `json:"definitions"`
		Fields []struct {
			Type  string `json:"type"`
			Ref   string `json:"ref"`
			Label string `json:"label"`
			Items struct {
				Type string `json:"type"`
				Ref  string `json:"ref"`
			} `json:"items"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(encoded, &shape); err != nil {
		t.Fatal(err)
	}
	if len(shape.Definitions) != 1 || shape.Definitions[0].Name != "dialog" || shape.Definitions[0].Field == nil ||
		shape.Fields[3].Type != "ref" || shape.Fields[3].Ref != "dialog" ||
		shape.Fields[3].Label != "模型台词" ||
		shape.Fields[4].Items.Type != "ref" || shape.Fields[4].Items.Ref != "dialog" {
		t.Fatalf("flat JSON did not preserve definitions and references: %s", encoded)
	}
	var restored DocumentJSON
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	restoredDoc, err := restored.Document()
	if err != nil {
		t.Fatal(err)
	}
	restoredSchema, err := xfield.CompileDocument(restoredDoc)
	if err != nil {
		t.Fatal(err)
	}
	restoredClean, err := xfield.ValidateModel(input, restoredSchema, Params{})
	if err != nil || !reflect.DeepEqual(clean, restoredClean) {
		t.Fatalf("round-trip changed validation: %v, %v", restoredClean, err)
	}
	input["mappedDialogs"] = []any{map[string]any{"speaker": "role-1"}}
	if _, err := xfield.ValidateModel(input, restoredSchema, Params{}); !errors.Is(err, xfield.ErrInvalidValue) {
		t.Fatalf("round-trip lost nested required rules: %v", err)
	}
	input["mappedDialogs"] = []any{}
	input["tags"] = []any{"invalid"}
	if _, err := xfield.ValidateModel(input, restoredSchema, Params{}); !errors.Is(err, xfield.ErrInvalidValue) {
		t.Fatalf("round-trip lost enum rules: %v", err)
	}
}

func TestConstructWithNG(t *testing.T) {
	// Both Go-constructed and JSON-decoded frontend arrays convert explicitly.
	wrap := func(field FieldConfig) FieldJSON {
		return xjson.NG[FieldConfig](field)
	}
	minItems := 2
	wire := DocumentJSON{
		Definitions: []DefinitionJSON{
			{Name: "flag", Field: wrap(&BooleanJSON{})},
		},
		Fields: SchemaJSON{
			wrap(&ObjectJSON{
				Meta:    xfield.Meta{ID: "group-id"},
				KeyJSON: KeyJSON{Key: "group"},
				Properties: SchemaJSON{
					wrap(&BooleanJSON{
						BooleanField: xfield.BooleanField[Params]{Meta: xfield.Meta{Required: true}},
						KeyJSON:      KeyJSON{Key: "active"},
					}),
					wrap(&ArrayJSON{
						KeyJSON: KeyJSON{Key: "scores"}, MinItems: &minItems,
						Items: wrap(&NumberJSON{NumberField: xfield.NumberField[Params]{Enum: []any{3, 4}}}),
					}),
				},
			}),
			wrap(&RefJSON{
				Meta:    xfield.Meta{Required: true},
				KeyJSON: KeyJSON{Key: "flag"},
				Ref:     "flag",
			}),
		},
	}
	data := map[string]any{"group": map[string]any{"active": true, "scores": []any{3, 4}}, "flag": false}
	for _, document := range []DocumentJSON{wire, roundTrip(t, wire)} {
		doc, err := document.Document()
		if err != nil {
			t.Fatal(err)
		}
		schema, err := xfield.CompileDocument(doc)
		if err != nil {
			t.Fatal(err)
		}
		clean, err := xfield.ValidateModel(data, schema, Params{})
		if err != nil || !reflect.DeepEqual(data, clean) {
			t.Fatalf("constructed containers: %v, %v", clean, err)
		}
		if schema["group"].(xfield.MetaProvider).FieldMeta().ID != "group-id" {
			t.Fatal("metadata was lost")
		}
		invalid := map[string]any{"group": map[string]any{"active": true, "scores": []any{3}}, "flag": false}
		if _, err := xfield.ValidateModel(invalid, schema, Params{}); !errors.Is(err, xfield.ErrInvalidValue) {
			t.Fatalf("array bounds were lost: %v", err)
		}
	}
}

func roundTrip(t *testing.T, wire DocumentJSON) DocumentJSON {
	t.Helper()
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var restored DocumentJSON
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	return restored
}

func TestRejectInvalidDeclarations(t *testing.T) {
	for _, raw := range []string{
		`{"fields":[{"type":"unknown","key":"bad"}]}`,
		`{"fields":[{"type":"array","key":"bad","items":{"type":"unknown"}}]}`,
		`{"fields":[{"type":"ref","key":"bad","ref":"missing"}]}`,
		`{"fields":[{"type":"array","key":"bad","items":null}]}`,
		`{"definitions":[{"name":"bad","field":null}],"fields":[]}`,
		`{"definitions":[{"field":{"type":"string"}}],"fields":[]}`,
		`{"definitions":[{"name":"bad","field":{"type":"string"}},{"name":"bad","field":{"type":"number"}}],"fields":[]}`,
		`{"fields":[{"type":"string","key":"same"},{"type":"number","key":"same"}]}`,
		`{"fields":[{"type":"string"}]}`,
		`{"fields":[{"type":"object","key":"group","properties":[{"type":"string","key":"same"},{"type":"number","key":"same"}]}]}`,
		`{"definitions":[{"name":"bad","field":{"type":"object","properties":[{"type":"string","key":"same"},{"type":"number","key":"same"}]}}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var wire DocumentJSON
			err := json.Unmarshal([]byte(raw), &wire)
			if err == nil {
				var doc xfield.SchemaDocument[Params]
				doc, err = wire.Document()
				if err == nil {
					_, err = xfield.CompileDocument(doc)
				}
			}
			if err == nil {
				t.Fatal("invalid declaration was accepted")
			}
		})
	}
}

func TestFrontendArrayOrderSurvivesConversion(t *testing.T) {
	raw := []byte(`{
		"definitions":[
			{"name":"zDef","field":{"type":"string"}},
			{"name":"aDef","field":{"type":"object","properties":[
				{"type":"string","key":"z"},{"type":"number","key":"a"}
			]}}
		],
		"fields":[
			{"type":"ref","key":"z","ref":"aDef"},
			{"type":"ref","key":"a","ref":"zDef"}
		]
	}`)
	var wire DocumentJSON
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := wire.Document()
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Definitions) != 2 || len(doc.Fields) != 2 || doc.Fields["z"] == nil {
		t.Fatalf("arrays did not convert to keyed declarations: %v", doc)
	}
	nested := doc.Definitions["aDef"].(*xfield.ObjectField[Params]).Properties
	if len(nested) != 2 || nested["z"] == nil || nested["a"] == nil {
		t.Fatal("nested property array was not converted")
	}
	if _, err := xfield.CompileDocument(doc); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(wire)
	if err != nil || string(before) != string(after) {
		t.Fatalf("conversion or compilation changed the frontend arrays: %s, %v", after, err)
	}
	restored := roundTrip(t, wire)
	if restored.Definitions[0].Name != "zDef" ||
		restored.Definitions[1].Name != "aDef" ||
		restored.Fields[0].Value().FieldKey() != "z" ||
		restored.Fields[1].Value().FieldKey() != "a" ||
		restored.Definitions[1].Field.Value().(*ObjectJSON).Properties[0].Value().FieldKey() != "z" {
		t.Fatal("saving changed definition, field or property array order")
	}
}
