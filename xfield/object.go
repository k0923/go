package xfield

type ObjectField[P any] struct {
	Meta
	Properties Schema[P] `json:"properties,omitempty"`
}

func (*ObjectField[P]) Type() string              { return "object" }
func (f *ObjectField[P]) ObjectFields() Schema[P] { return f.Properties }

func (f *ObjectField[P]) Parse(data any, params P) (any, error) {
	if err := ValidateField(f); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	object, ok := asStringMap(data)
	if !ok {
		return nil, invalidValue("expect object")
	}
	return parseProperties(object, f.Properties, false, params)
}

func (f *ObjectField[P]) Present(data any, params P) any {
	object, ok := asStringMap(data)
	if !ok {
		return data
	}
	return PresentModel(object, f.Properties, params)
}
