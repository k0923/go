package xfield_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/k0923/go/xfield"
)

type replacementString struct {
	xfield.Meta
	Prefix string `json:"prefix"`
}

func (*replacementString) Type() string { return "string" }
func (f *replacementString) Parse(value any, _ struct{}) (any, error) {
	text, ok := value.(string)
	if !ok {
		return nil, errors.New("expect string")
	}
	return f.Prefix + text, nil
}

func TestRegistryRegister(t *testing.T) {
	t.Run("empty registry and selective string replacement", func(t *testing.T) {
		r := xfield.NewRegistry[struct{}]()
		if _, err := r.DecodeField(json.RawMessage(`{"type":"string"}`)); !errors.Is(err, xfield.ErrUnknownType) {
			t.Fatalf("new registry must be empty: %v", err)
		}
		for _, factory := range []xfield.Factory[struct{}]{
			func() xfield.Field[struct{}] { return &replacementString{} },
			func() xfield.Field[struct{}] { return &xfield.ArrayField[struct{}]{} },
		} {
			if err := r.Register(factory); err != nil {
				t.Fatal(err)
			}
		}
		field, err := r.DecodeField(json.RawMessage(`{"type":"array","items":{"type":"string","prefix":"custom:"}}`))
		if err != nil {
			t.Fatal(err)
		}
		output, err := field.Parse([]any{"value"}, struct{}{})
		if err != nil || output.([]any)[0] != "custom:value" {
			t.Fatalf("custom string was not used in array: %v, %v", output, err)
		}
		if _, err := r.DecodeField(json.RawMessage(`{"type":"number"}`)); !errors.Is(err, xfield.ErrUnknownType) {
			t.Fatalf("unselected built-in was registered: %v", err)
		}
		if err := r.RegisterBuiltins(); !errors.Is(err, xfield.ErrInvalidSchema) {
			t.Fatalf("built-in conflict: %v", err)
		}
		if _, err := r.DecodeField(json.RawMessage(`{"type":"number"}`)); !errors.Is(err, xfield.ErrUnknownType) {
			t.Fatalf("failed batch partially registered built-ins: %v", err)
		}
	})

	t.Run("factory is sampled once and creates fresh decode instances", func(t *testing.T) {
		r := xfield.NewRegistry[struct{}]()
		calls := 0
		var sample *replacementString
		if err := r.Register(func() xfield.Field[struct{}] {
			calls++
			field := &replacementString{}
			if calls == 1 {
				sample = field
			}
			return field
		}); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("registration called factory %d times", calls)
		}
		first, err := r.DecodeField(json.RawMessage(`{"type":"string","key":"first"}`))
		if err != nil {
			t.Fatal(err)
		}
		second, err := r.DecodeField(json.RawMessage(`{"type":"string","key":"second"}`))
		if err != nil || calls != 3 || first == second || first == sample ||
			xfield.KeyOf(first) != "first" || xfield.KeyOf(second) != "second" || sample.Key != "" {
			t.Fatalf("factory lifecycle: calls=%d first=%v second=%v sample=%v err=%v", calls, first, second, sample, err)
		}
	})

	t.Run("invalid factories", func(t *testing.T) {
		r := xfield.NewRegistry[*dependencies]()
		for _, factory := range []xfield.Factory[*dependencies]{
			nil,
			func() xfield.Field[*dependencies] { return nil },
			func() xfield.Field[*dependencies] { return (*resourceField)(nil) },
			func() xfield.Field[*dependencies] { return &namedResource{} },
		} {
			if err := r.Register(factory); !errors.Is(err, xfield.ErrInvalidSchema) {
				t.Fatalf("invalid factory accepted: %v", err)
			}
		}
	})

	t.Run("zero value", func(t *testing.T) {
		var r xfield.Registry[struct{}]
		if err := r.RegisterBuiltins(); err != nil {
			t.Fatal(err)
		}
		if _, err := r.DecodeField(json.RawMessage(`{"type":"boolean"}`)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDefaultRegistry(t *testing.T) {
	r := xfield.DefaultRegistry[*dependencies]()
	for _, kind := range []string{"string", "number", "boolean", "array", "object"} {
		raw, _ := json.Marshal(map[string]any{"type": kind})
		field, err := r.DecodeField(raw)
		if err != nil || field.Type() != kind {
			t.Fatalf("built-in %s: %v, %v", kind, field, err)
		}
	}
	registerResource(t, r)
	if _, err := xfield.DefaultRegistry[*dependencies]().DecodeField(json.RawMessage(`{"type":"resource"}`)); !errors.Is(err, xfield.ErrUnknownType) {
		t.Fatalf("default registries share registrations: %v", err)
	}
	if err := r.Register(func() xfield.Field[*dependencies] { return &xfield.StringField[*dependencies]{} }); !errors.Is(err, xfield.ErrInvalidSchema) {
		t.Fatalf("duplicate built-in accepted: %v", err)
	}
}

type decodedResource struct {
	resourceField
	err error
}

func (*decodedResource) Type() string { return "decoded_resource" }
func (f *decodedResource) DecodeFieldJSON(raw json.RawMessage, _ xfield.Decoder[*dependencies]) error {
	if f.err != nil {
		return f.err
	}
	var config struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return err
	}
	f.Key = config.Name
	return nil
}

func TestRegistryDecodeField(t *testing.T) {
	t.Run("optional custom decoder and error chain", func(t *testing.T) {
		r := xfield.DefaultRegistry[*dependencies]()
		err := r.Register(func() xfield.Field[*dependencies] { return &decodedResource{} })
		if err != nil {
			t.Fatal(err)
		}
		field, err := r.DecodeField(json.RawMessage(`{"type":"decoded_resource","name":"custom-key"}`))
		if err != nil || xfield.KeyOf(field) != "custom-key" {
			t.Fatalf("custom decoder not called: %v, %v", field, err)
		}
		sentinel := errors.New("decode failed")
		failing := xfield.DefaultRegistry[*dependencies]()
		if err := failing.Register(func() xfield.Field[*dependencies] {
			return &decodedResource{err: sentinel}
		}); err != nil {
			t.Fatal(err)
		}
		_, err = failing.DecodeField(json.RawMessage(`{"type":"array","items":{"type":"decoded_resource"}}`))
		var path *xfield.PathError
		if !errors.Is(err, sentinel) || !errors.As(err, &path) || path.Path != "items" {
			t.Fatalf("custom decoder error lost: %v", err)
		}
	})

	t.Run("factory invalid after registration", func(t *testing.T) {
		for _, invalid := range []xfield.Field[*dependencies]{nil, (*resourceField)(nil), &namedResource{typeName: "changed"}} {
			r := xfield.NewRegistry[*dependencies]()
			calls := 0
			if err := r.Register(func() xfield.Field[*dependencies] {
				calls++
				if calls == 1 {
					return &resourceField{}
				}
				return invalid
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.DecodeField(json.RawMessage(`{"type":"resource"}`)); !errors.Is(err, xfield.ErrInvalidSchema) {
				t.Fatalf("invalid factory decode result accepted: %v", err)
			}
		}
	})
}
