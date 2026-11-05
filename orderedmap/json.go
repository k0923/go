package orderedmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

var (
	_ json.Marshaler   = Map[any]{}
	_ json.Marshaler   = (*Map[any])(nil)
	_ json.Unmarshaler = (*Map[any])(nil)
)

// MarshalJSON encodes an object in insertion order. An empty Map encodes as {}.
// The value receiver also supports Map fields in non-addressable struct values.
func (m Map[V]) MarshalJSON() ([]byte, error) {
	if len(m.keys) == 0 {
		return []byte("{}"), nil
	}
	// Reserve space for keys, punctuation and small values. Escaped keys
	// and larger values can still grow the buffer as needed.
	size := 2
	for _, key := range m.keys {
		size += len(key) + 16
	}
	var data bytes.Buffer
	data.Grow(size)
	encoder := json.NewEncoder(&data)
	data.WriteByte('{')
	for i, key := range m.keys {
		if i != 0 {
			data.WriteByte(',')
		}
		// Encoding a string to bytes.Buffer cannot fail. Remove only the
		// newline added by Encoder, not whitespace inside the JSON value.
		_ = encoder.Encode(key)
		data.Truncate(data.Len() - 1)
		data.WriteByte(':')
		if err := encoder.Encode(m.values[key]); err != nil {
			return nil, fmt.Errorf("orderedmap: encode key %q: %w", key, err)
		}
		data.Truncate(data.Len() - 1)
	}
	data.WriteByte('}')
	return data.Bytes(), nil
}

// UnmarshalJSON replaces m with an object in source key order.
// Duplicate keys are rejected. On error, m is unchanged. JSON null clears m.
//
// Values use encoding/json semantics: with V=any, nested objects are ordinary
// map[string]any and numbers are float64. Declare nested Map types explicitly
// to preserve their order.
func (m *Map[V]) UnmarshalJSON(data []byte) error {
	if m == nil {
		return fmt.Errorf("orderedmap: UnmarshalJSON on nil receiver")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	var next Map[V]
	if token != nil {
		if token != json.Delim('{') {
			return fmt.Errorf("orderedmap: expected JSON object or null")
		}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("orderedmap: expected JSON object key")
			}
			if _, exists := next.values[key]; exists {
				return &DuplicateKeyError{Key: key}
			}
			var value V
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("orderedmap: decode key %q: %w", key, err)
			}
			next.Set(key, value)
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("orderedmap: unexpected data after JSON value")
	}
	*m = next
	return nil
}
