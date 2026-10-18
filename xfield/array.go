package xfield

import "encoding/json"

type ArrayField[P any] struct {
	Meta
	Items    FieldRef[P] `json:"items"`
	MinItems *int        `json:"minItems,omitempty"`
	MaxItems *int        `json:"maxItems,omitempty"`
}

func (*ArrayField[P]) Type() string          { return "array" }
func (f *ArrayField[P]) ItemField() Field[P] { return f.Items.Value() }
func (f *ArrayField[P]) DecodeFieldJSON(raw json.RawMessage, decoder Decoder[P]) error {
	var header struct {
		Meta
		Items    json.RawMessage `json:"items"`
		MinItems *int            `json:"minItems"`
		MaxItems *int            `json:"maxItems"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}
	var child Field[P]
	if len(header.Items) != 0 {
		var err error
		child, err = decoder.DecodeField(header.Items)
		if err != nil {
			return wrapPathError("items", err)
		}
	}
	*f = ArrayField[P]{Meta: header.Meta, Items: Wrap(child), MinItems: header.MinItems, MaxItems: header.MaxItems}
	return nil
}

func (f *ArrayField[P]) ValidateSchema() error {
	if isNil(f.Items.Value()) {
		return invalidSchema("array schema missing 'items'")
	}
	return validateIntBounds(f.MinItems, f.MaxItems)
}

func (f *ArrayField[P]) Parse(data any, params P) (any, error) {
	if err := ValidateField(f); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	arr, ok := asAnySlice(data)
	if !ok {
		return nil, invalidValue("expect array")
	}
	if f.MinItems != nil && len(arr) < *f.MinItems {
		return nil, invalidValue("len < minItems(%d)", *f.MinItems)
	}
	if f.MaxItems != nil && len(arr) > *f.MaxItems {
		return nil, invalidValue("len > maxItems(%d)", *f.MaxItems)
	}
	out := make([]any, len(arr))
	for i, value := range arr {
		parsed, err := f.Items.Value().Parse(value, params)
		if err != nil {
			return nil, wrapPathError(joinIndexPath("", i), err)
		}
		out[i] = parsed
	}
	return out, nil
}

func (f *ArrayField[P]) Present(data any, params P) any {
	arr, ok := asAnySlice(data)
	if !ok || isNil(f.Items.Value()) {
		return data
	}
	out := make([]any, len(arr))
	for i, value := range arr {
		out[i] = presentFieldValue(f.Items.Value(), value, params)
	}
	return out
}

func validateIntBounds(min, max *int) error {
	if (min != nil && *min < 0) || (max != nil && *max < 0) {
		return invalidSchema("length bounds must be non-negative")
	}
	if min != nil && max != nil && *min > *max {
		return invalidSchema("minimum length exceeds maximum")
	}
	return nil
}
