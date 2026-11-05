// Package schema demonstrates a caller-owned map adapter for xfield.
package schema

import (
	"iter"
	"slices"

	"github.com/k0923/go/xfield"
)

// Map uses ordinary JSON object encoding and sorts keys for stable execution.
// The adapter does not decode polymorphic Field values; that belongs to the app.
type Map[P any] map[string]xfield.Field[P]

var _ xfield.Schema[any] = Map[any]{}

func (m Map[P]) Get(key string) (xfield.Field[P], bool) {
	field, ok := m[key]
	return field, ok
}

func (m Map[P]) All() iter.Seq2[string, xfield.Field[P]] {
	return func(yield func(string, xfield.Field[P]) bool) {
		keys := make([]string, 0, len(m))
		for key := range m {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			if !yield(key, m[key]) {
				return
			}
		}
	}
}
