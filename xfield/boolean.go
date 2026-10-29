package xfield

type BooleanField[P any] struct{ Meta }

var (
	_ Field[any]   = (*BooleanField[any])(nil)
	_ MetaProvider = (*BooleanField[any])(nil)
)

func (*BooleanField[P]) Type() string { return "boolean" }

func (*BooleanField[P]) Parse(_ P, data any) (any, error) {
	if data == nil {
		return nil, nil
	}
	if _, ok := data.(bool); !ok {
		return nil, invalidValue("expect boolean")
	}
	return data, nil
}
