package xfield

type BooleanField[P any] struct{ Meta }

func (*BooleanField[P]) Type() string { return "boolean" }

func (*BooleanField[P]) Parse(data any, _ P) (any, error) {
	if data == nil {
		return nil, nil
	}
	if _, ok := data.(bool); !ok {
		return nil, invalidValue("expect boolean")
	}
	return data, nil
}
