package orderedmap_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	xjson "github.com/k0923/go/json"
	"github.com/k0923/go/xfield"
	orderedmap "github.com/wk8/go-ordered-map/v2"
)

func keys[V any](m *orderedmap.OrderedMap[string, V]) []string {
	var result []string
	for pair := m.Oldest(); pair != nil; pair = pair.Next() {
		result = append(result, pair.Key)
	}
	return result
}

func TestInsertionUpdateDeleteAndMove(t *testing.T) {
	m := orderedmap.New[string, any]()
	m.Set("speaker", "role-1")
	m.Set("start", 1000)
	m.Set("end", 2000)
	m.Set("content", "你好")
	m.Set("start", 1500)
	if !reflect.DeepEqual(keys(m), []string{"speaker", "start", "end", "content"}) {
		t.Fatal("updating a key changed its insertion position")
	}
	if value, exists := m.Get("start"); !exists || value != 1500 {
		t.Fatal("updated value was not found")
	}
	m.Delete("start")
	m.Set("start", 1600)
	if !reflect.DeepEqual(keys(m), []string{"speaker", "end", "content", "start"}) {
		t.Fatal("reinsertion did not append")
	}
	if err := m.MoveBefore("start", "end"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys(m), []string{"speaker", "start", "end", "content"}) {
		t.Fatal("MoveBefore did not restore the desired order")
	}
}

func TestJSONOrderAndSpecialKeys(t *testing.T) {
	raw := []byte(`{"z":1,"a":null,"剧本":{"name":"示例"},"quote\"key":"value"}`)
	var m orderedmap.OrderedMap[string, any]
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys(&m), []string{"z", "a", "剧本", `quote"key`}) {
		t.Fatal("JSON input order was lost")
	}
	encoded, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	var restored orderedmap.OrderedMap[string, any]
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys(&m), keys(&restored)) {
		t.Fatalf("JSON round-trip reordered keys: %s", encoded)
	}
	if value, exists := restored.Get("a"); !exists || value != nil {
		t.Fatal("explicit null was lost")
	}
	// any does not make nested JSON objects ordered automatically.
	nested, _ := restored.Get("剧本")
	if _, ok := nested.(map[string]any); !ok {
		t.Fatalf("unexpected nested representation: %T", nested)
	}
	t.Logf("nested any object is %T; only the outer map's order is preserved", nested)
}

func TestTypedNestedJSONOrder(t *testing.T) {
	var m orderedmap.OrderedMap[string, *orderedmap.OrderedMap[string, json.RawMessage]]
	if err := json.Unmarshal([]byte(`{"outer":{"z":1,"a":2}}`), &m); err != nil {
		t.Fatal(err)
	}
	nested, _ := m.Get("outer")
	if !reflect.DeepEqual(keys(nested), []string{"z", "a"}) {
		t.Fatal("explicitly typed nested ordered map lost order")
	}
	encoded, err := json.Marshal(&m)
	if err != nil || string(encoded) != `{"outer":{"z":1,"a":2}}` {
		t.Fatalf("typed nested order: %s, %v", encoded, err)
	}
}

func TestNumberRepresentations(t *testing.T) {
	raw := []byte(`{"id":9007199254740993}`)
	var dynamic orderedmap.OrderedMap[string, any]
	if err := json.Unmarshal(raw, &dynamic); err != nil {
		t.Fatal(err)
	}
	value, _ := dynamic.Get("id")
	if value != float64(9007199254740992) {
		t.Fatalf("unexpected any numeric representation: %T %v", value, value)
	}
	t.Logf("any numeric representation: %T %v", value, value)

	var precise orderedmap.OrderedMap[string, json.Number]
	if err := json.Unmarshal(raw, &precise); err != nil {
		t.Fatal(err)
	}
	number, _ := precise.Get("id")
	if number != json.Number("9007199254740993") {
		t.Fatalf("json.Number lost precision: %v", number)
	}
	encoded, err := json.Marshal(&precise)
	if err != nil || string(encoded) != string(raw) {
		t.Fatalf("json.Number round-trip: %s, %v", encoded, err)
	}

	var withUseNumber orderedmap.OrderedMap[string, any]
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&withUseNumber); err != nil {
		t.Fatal(err)
	}
	value, _ = withUseNumber.Get("id")
	if value != float64(9007199254740992) {
		t.Fatalf("outer UseNumber behavior changed: %T %v", value, value)
	}
	t.Logf("outer Decoder.UseNumber representation: %T %v", value, value)
}

func TestDecodeSemantics(t *testing.T) {
	m := orderedmap.New[string, any]()
	m.Set("old", 1)
	if err := json.Unmarshal([]byte(`{"new":2,"old":3}`), m); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys(m), []string{"old", "new"}) {
		t.Fatal("decoding into an existing map no longer preserves its old insertion positions")
	}
	var duplicates orderedmap.OrderedMap[string, any]
	if err := json.Unmarshal([]byte(`{"a":1,"b":2,"a":3}`), &duplicates); err != nil {
		t.Fatal(err)
	}
	value, _ := duplicates.Get("a")
	if value != float64(3) || !reflect.DeepEqual(keys(&duplicates), []string{"a", "b"}) {
		t.Fatal("duplicate key semantics changed")
	}
	// Pointer decoding supports an explicit null. Decode into a fresh value
	// when the JSON object must define a completely new set and order of keys.
	var pointer *orderedmap.OrderedMap[string, any]
	if err := json.Unmarshal([]byte(`null`), &pointer); err != nil || pointer != nil {
		t.Fatalf("pointer null decoding: %v, %v", pointer, err)
	}
	t.Log("existing-map decode merges; duplicate keys use the last value and first position")
}

func TestConcurrentReadOnly(t *testing.T) {
	m := orderedmap.New[string, any]()
	for i := range 100 {
		m.Set(fmt.Sprint(i), i)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				for pair := m.Oldest(); pair != nil; pair = pair.Next() {
					if value, exists := m.Get(pair.Key); !exists || value != pair.Value {
						t.Error("concurrent read changed the value")
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

func TestXFieldRuntimeMapCompatibility(t *testing.T) {
	m := orderedmap.New[string, any]()
	m.Set("content", "你好")
	field := &xfield.ObjectField[Params]{
		Properties: xfield.Schema[Params]{
			"content": &xfield.StringField[Params]{Meta: xfield.Meta{Required: true}},
		},
	}
	if _, err := field.Parse(m, Params{}); err == nil {
		t.Fatal("ordered map unexpectedly accepted by xfield's native-map parser")
	}
	// OrderedMap is a struct containing a map and list, not a native Go map.
	// Explicit conversion preserves values; xfield sorts schema property names.
	native := make(map[string]any, m.Len())
	for pair := m.Oldest(); pair != nil; pair = pair.Next() {
		native[pair.Key] = pair.Value
	}
	if _, err := field.Parse(native, Params{}); err != nil {
		t.Fatal(err)
	}
}

// Each evaluation application owns its own xjson binding.
type Params struct{}

type FieldJSON = xjson.G[xfield.Field[Params]]

type keyMetadata struct {
	Key string `json:"key,omitempty"`
}

func (m keyMetadata) FieldKey() string { return m.Key }

type StringJSON struct {
	xfield.StringField[Params]
	keyMetadata
}

type NumberJSON struct {
	xfield.NumberField[Params]
	keyMetadata
}

type ObjectJSON struct {
	xfield.ObjectField[Params]
	keyMetadata
	Properties []FieldJSON `json:"properties,omitempty"`
}

type ArrayJSON struct {
	xfield.ArrayField[Params]
	keyMetadata
	Items FieldJSON `json:"items"`
}

type RefJSON struct {
	xfield.RefField[Params]
	keyMetadata
	Ref string `json:"ref"`
}

func init() {
	xjson.Bind[xfield.Field[Params]](map[string]xfield.Field[Params]{
		"string": &StringJSON{},
		"number": &NumberJSON{},
		"object": &ObjectJSON{},
		"array":  &ArrayJSON{},
		"ref":    &RefJSON{},
	}, xjson.WithFlatLayout(), xjson.WithInitializer(func(field xfield.Field[Params]) xfield.Field[Params] {
		switch field := field.(type) {
		case *ObjectJSON:
			field.ObjectField.Properties = make(xfield.Schema[Params], len(field.Properties))
			for _, child := range field.Properties {
				key := child.Value().(interface{ FieldKey() string }).FieldKey()
				field.ObjectField.Properties[key] = child.Value()
			}
		case *ArrayJSON:
			field.ArrayField.Items = field.Items.Value()
		case *RefJSON:
			field.RefField.Ref = field.Ref
		}
		return field
	}))
}

func (f *ObjectJSON) CompileField(compile func(xfield.Field[Params]) (xfield.Field[Params], error)) (xfield.Field[Params], error) {
	result := f.ObjectField
	result.Properties = make(xfield.Schema[Params], len(f.ObjectField.Properties))
	for i, child := range f.ObjectField.Properties {
		field, err := compile(child)
		if err != nil {
			return nil, err
		}
		result.Properties[i] = field
	}
	return &result, nil
}

func (f *ArrayJSON) CompileField(compile func(xfield.Field[Params]) (xfield.Field[Params], error)) (xfield.Field[Params], error) {
	result := f.ArrayField
	item, err := compile(result.Items)
	if err != nil {
		return nil, err
	}
	result.Items = item
	return &result, nil
}

func TestXJSONAndXField(t *testing.T) {
	var wire struct {
		Definitions orderedmap.OrderedMap[string, FieldJSON] `json:"definitions"`
		Fields      []FieldJSON                              `json:"fields"`
	}
	raw := []byte(`{
		"definitions": {
			"zDef": {"type":"string","minLength":1},
			"dialog": {"type":"object","properties":[
				{"type":"string","key":"speaker","required":true},
				{"type":"number","key":"start","required":true},
				{"type":"number","key":"end","required":true},
				{"type":"string","key":"content","required":true}
			]},
			"aDef": {"type":"array","items":{"type":"ref","ref":"dialog"}}
		},
		"fields": [
			{"type":"ref","key":"dialog","required":true,"ref":"dialog"},
			{"type":"ref","key":"mappedDialogs","required":true,"ref":"aDef"}
		]
	}`)
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys(&wire.Definitions), []string{"zDef", "dialog", "aDef"}) {
		t.Fatal("xjson values changed definition order")
	}
	doc := xfield.SchemaDocument[Params]{
		Definitions: make(map[string]xfield.Field[Params]),
		Fields:      make(xfield.Schema[Params]),
	}
	for pair := wire.Definitions.Oldest(); pair != nil; pair = pair.Next() {
		doc.Definitions[pair.Key] = pair.Value.Value()
	}
	for _, field := range wire.Fields {
		key := field.Value().(interface{ FieldKey() string }).FieldKey()
		doc.Fields[key] = field.Value()
	}
	schema, err := xfield.CompileDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := xfield.ValidateModel(map[string]any{
		"dialog": nil,
		"mappedDialogs": []any{map[string]any{
			"speaker": "role-1", "start": 1000, "end": 2000, "content": "你好", "extra": true,
		}},
	}, schema, Params{})
	if err != nil || clean["dialog"] != nil {
		t.Fatalf("xfield validation: %v, %v", clean, err)
	}
	if _, err := xfield.FindFieldByPath(schema, "mappedDialogs.0.content"); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"ref":"dialog"`) {
		t.Fatalf("xjson reference was lost: %s", encoded)
	}
	first := strings.Index(string(encoded), `"zDef":`)
	second := strings.Index(string(encoded), `"dialog":`)
	third := strings.Index(string(encoded), `"aDef":`)
	if !(first >= 0 && first < second && second < third) {
		t.Fatalf("encoded definitions lost order: %s", encoded)
	}
}
