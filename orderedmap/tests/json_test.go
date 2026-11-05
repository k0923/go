package orderedmap_test

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/k0923/go/orderedmap"
)

func assertJSON(t *testing.T, value any, want string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("JSON = %s, want %s", data, want)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	const input = `{"z":1,"中文":2,"a\"\\\n":3,"":4,"<>&":5}`
	var m orderedmap.Map[int]
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Keys(), []string{"z", "中文", "a\"\\\n", "", "<>&"}) {
		t.Fatalf("decoded order: %q", m.Keys())
	}
	const encoded = `{"z":1,"中文":2,"a\"\\\n":3,"":4,"\u003c\u003e\u0026":5}`
	assertJSON(t, m, encoded)
	assertJSON(t, &m, encoded)
	m.Set("z", 9)
	assertJSON(t, m, `{"z":9,"中文":2,"a\"\\\n":3,"":4,"\u003c\u003e\u0026":5}`)

	if err := json.Unmarshal([]byte(`{"b":20,"z":10}`), &m); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, m, `{"b":20,"z":10}`)
}

func TestJSONEmptyAndNull(t *testing.T) {
	var m orderedmap.Map[int]
	assertJSON(t, m, `{}`)
	var pointer *orderedmap.Map[int]
	assertJSON(t, pointer, `null`)
	for _, input := range []string{"{}", " null \n"} {
		m.Set("old", 1)
		if err := json.Unmarshal([]byte(input), &m); err != nil {
			t.Fatal(err)
		}
		assertJSON(t, m, `{}`)
		m.Set("new", 2)
		assertJSON(t, m, `{"new":2}`)
	}
	if err := pointer.UnmarshalJSON([]byte(`{}`)); err == nil {
		t.Fatal("nil receiver must return an error")
	}
}

func TestJSONErrorsPreserveMap(t *testing.T) {
	for _, input := range []string{
		``, `{`, `{"new":1`, `{"new":1,}`, `{"new":1,"bad":"text"}`,
		`{"new":1,"bad":999999999999999999999999999999999999}`,
		`{"new":1,"new":2}`, `{"a":1,"\u0061":2}`,
		`[]`, `[1]`, `true`, `42`, `"text"`,
		`{} {}`, `null {}`, `{} trailing`, `{"new":1]`,
	} {
		t.Run(input, func(t *testing.T) {
			var m orderedmap.Map[int]
			m.Set("old", 7)
			// Call the method directly to also validate trailing input without
			// encoding/json's outer syntax validation.
			if err := m.UnmarshalJSON([]byte(input)); err == nil {
				t.Fatalf("accepted invalid input: %q", input)
			}
			assertJSON(t, m, `{"old":7}`)
		})
	}
}

func TestJSONErrorTypes(t *testing.T) {
	var m orderedmap.Map[int]
	err := json.Unmarshal([]byte(`{"a":1,"\u0061":2}`), &m)
	if !errors.Is(err, orderedmap.ErrDuplicateKey) {
		t.Fatalf("duplicate error: %v", err)
	}
	var duplicate *orderedmap.DuplicateKeyError
	if !errors.As(err, &duplicate) || duplicate.Key != "a" || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("duplicate details: %v", err)
	}
	err = json.Unmarshal([]byte(`{"a":"bad"}`), &m)
	var mismatch *json.UnmarshalTypeError
	if !errors.As(err, &mismatch) {
		t.Fatalf("value type error not preserved: %v", err)
	}
	var dynamic orderedmap.Map[any]
	dynamic.Set("channel", make(chan int))
	_, err = json.Marshal(dynamic)
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("unsupported value error: %v", err)
	}
	dynamic.Delete("channel")
	dynamic.Set("nan", math.NaN())
	_, err = json.Marshal(dynamic)
	var unsupportedValue *json.UnsupportedValueError
	if !errors.As(err, &unsupportedValue) {
		t.Fatalf("unsupported number error: %v", err)
	}
}

func TestJSONExplicitNestedMaps(t *testing.T) {
	const input = `{"z":{"y":1,"b":2},"a":{"x":3,"c":4}}`
	var values orderedmap.Map[orderedmap.Map[int]]
	if err := json.Unmarshal([]byte(input), &values); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, values, input)

	var pointers orderedmap.Map[*orderedmap.Map[int]]
	if err := json.Unmarshal([]byte(input), &pointers); err != nil {
		t.Fatal(err)
	}
	child, _ := pointers.Get("z")
	child.Set("last", 5)
	assertJSON(t, pointers, `{"z":{"y":1,"b":2,"last":5},"a":{"x":3,"c":4}}`)
	err := json.Unmarshal([]byte(`{"new":{"x":1,"x":2}}`), &pointers)
	if !errors.Is(err, orderedmap.ErrDuplicateKey) {
		t.Fatalf("nested duplicate error: %v", err)
	}
	assertJSON(t, pointers, `{"z":{"y":1,"b":2,"last":5},"a":{"x":3,"c":4}}`)

	var arrays orderedmap.Map[[]orderedmap.Map[int]]
	const arrayInput = `{"z":[{"b":1,"a":2}],"a":[]}`
	if err := json.Unmarshal([]byte(arrayInput), &arrays); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, arrays, arrayInput)
}

func TestJSONAnyUsesStandardTypes(t *testing.T) {
	var m orderedmap.Map[any]
	const input = `{"z":{"b":1,"a":2},"a":[{"y":3,"x":4}],"n":null}`
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatal(err)
	}
	value, _ := m.Get("z")
	object, ok := value.(map[string]any)
	if !ok || object["b"] != float64(1) {
		t.Fatalf("nested object uses unexpected types: %#v", value)
	}
	value, _ = m.Get("a")
	array, ok := value.([]any)
	if !ok || len(array) != 1 {
		t.Fatalf("unexpected array: %#v", value)
	}
	if _, ok := array[0].(map[string]any); !ok {
		t.Fatalf("array object type: %T", array[0])
	}
	assertJSON(t, m, `{"z":{"a":2,"b":1},"a":[{"x":4,"y":3}],"n":null}`)
}

func TestJSONContainerValues(t *testing.T) {
	type Product struct {
		Attributes orderedmap.Map[string] `json:"attributes"`
	}
	var product Product
	const input = `{"attributes":{"z":"last","a":"first"}}`
	if err := json.Unmarshal([]byte(input), &product); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, product, input)
	assertJSON(t, map[string]Product{"product": product}, `{"product":`+input+`}`)
	assertJSON(t, []Product{product}, `[`+input+`]`)
	assertJSON(t, map[string]orderedmap.Map[string]{"m": product.Attributes}, `{"m":{"z":"last","a":"first"}}`)
}

func TestJSONNumberAndStreaming(t *testing.T) {
	var m orderedmap.Map[json.Number]
	const input = `{"large":9007199254740993,"fraction":1.250}`
	decoder := json.NewDecoder(strings.NewReader(input + "\n" + `{"second":2}`))
	if err := decoder.Decode(&m); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, m, input)
	var output strings.Builder
	if err := json.NewEncoder(&output).Encode(m); err != nil {
		t.Fatal(err)
	}
	if output.String() != input+"\n" {
		t.Fatalf("encoded output: %s", output.String())
	}
	if err := decoder.Decode(&m); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, m, `{"second":2}`)
}
