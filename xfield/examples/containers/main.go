// Run: go run ./xfield/examples/containers
package main

import (
	"encoding/json"
	"fmt"
	"iter"
	"log"

	"github.com/k0923/go/orderedmap"
	"github.com/k0923/go/xfield"
	"github.com/k0923/go/xfield/examples/internal/schema"
)

type Params struct{}

type property struct {
	Key   string               `json:"key"`
	Field xfield.Field[Params] `json:"field"`
}

// fieldArray owns its JSON array representation and iteration order.
// Applications must reject duplicate names when constructing their array.
type fieldArray []property

func (s fieldArray) Get(key string) (xfield.Field[Params], bool) {
	for _, entry := range s {
		if entry.Key == key {
			return entry.Field, true
		}
	}
	return nil, false
}

func (s fieldArray) All() iter.Seq2[string, xfield.Field[Params]] {
	return func(yield func(string, xfield.Field[Params]) bool) {
		for _, entry := range s {
			if !yield(entry.Key, entry.Field) {
				return
			}
		}
	}
}

// Field type tags are application metadata, independent of Schema storage.
type textField struct {
	xfield.StringField[Params]
	Kind string `json:"type"`
}

var (
	_ xfield.Schema[Params] = fieldArray{}
	_ xfield.Schema[Params] = (*orderedmap.Map[xfield.Field[Params]])(nil)
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	z := &textField{Kind: "string", StringField: xfield.StringField[Params]{Meta: xfield.Meta{Label: "Z"}}}
	a := &textField{Kind: "string", StringField: xfield.StringField[Params]{Meta: xfield.Meta{Label: "A"}}}
	plain := schema.Map[Params]{"z": z, "a": a}
	var ordered orderedmap.Map[xfield.Field[Params]]
	ordered.Set("z", z)
	ordered.Set("a", a)
	array := fieldArray{{Key: "z", Field: z}, {Key: "a", Field: a}}

	for _, example := range []struct {
		name   string
		schema xfield.Schema[Params]
	}{
		{"map (sorted by adapter)", plain},
		{"orderedmap", &ordered},
		{"array", array},
	} {
		compiled, err := xfield.CompileDocument(xfield.SchemaDocument[Params]{Fields: example.schema})
		if err != nil {
			return err
		}
		_, err = xfield.ValidateModel(map[string]any{"z": "first", "a": "second"}, compiled, Params{})
		if err != nil {
			return err
		}
		var keys []string
		for key := range compiled.All() {
			keys = append(keys, key)
		}
		// Persist the caller-owned declaration, not the compiled execution object.
		raw, err := json.Marshal(example.schema)
		if err != nil {
			return err
		}
		fmt.Printf("%s: compiled order=%v\n%s\n", example.name, keys, raw)
	}
	return nil
}
