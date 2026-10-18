package xfield

import "encoding/json"

type ObjectField[P any] struct {
	Meta
	Properties Schema[P] `json:"properties,omitempty"`
}

func (*ObjectField[P]) Type() string              { return "object" }
func (f *ObjectField[P]) ObjectFields() Schema[P] { return f.Properties }

func (f *ObjectField[P]) DecodeFieldJSON(raw json.RawMessage, decoder Decoder[P]) error {
	var header struct {
		Meta
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}
	var properties Schema[P]
	if len(header.Properties) != 0 {
		var err error
		properties, err = decoder.DecodeSchema(header.Properties)
		if err != nil {
			return wrapPathError("properties", err)
		}
	}
	*f = ObjectField[P]{Meta: header.Meta, Properties: properties}
	return nil
}

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
