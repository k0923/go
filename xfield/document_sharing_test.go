package xfield_test

import (
	"fmt"
	"testing"

	"github.com/k0923/go/xfield"
)

func TestCompileSharedDefinitions(t *testing.T) {
	// Each level references the previous level twice. The document is small,
	// while a fully expanded tree would contain tens of millions of fields.
	definitions := map[string]xfield.Field[struct{}]{
		"000": &xfield.StringField[struct{}]{},
	}
	for i := 1; i <= 25; i++ {
		name := fmt.Sprintf("%03d", i)
		child := fmt.Sprintf("%03d", i-1)
		definitions[name] = &xfield.ObjectField[struct{}]{
			Properties: xfield.Schema[struct{}]{
				"left":  &xfield.RefField[struct{}]{Ref: child},
				"right": &xfield.RefField[struct{}]{Ref: child},
			},
		}
	}
	doc := xfield.SchemaDocument[struct{}]{
		Definitions: definitions,
		Fields: xfield.Schema[struct{}]{
			"tree": &xfield.RefField[struct{}]{Ref: "025"},
		},
	}
	schema, err := xfield.CompileDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xfield.ValidateModel(map[string]any{"tree": map[string]any{}}, schema, struct{}{}); err != nil {
		t.Fatal(err)
	}
}
