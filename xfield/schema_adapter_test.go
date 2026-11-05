package xfield_test

import (
	"iter"
	"slices"

	"github.com/k0923/go/xfield"
)

// schemaMap is a caller-owned adapter. Sorting is this adapter's policy,
// rather than a requirement imposed by xfield.
type schemaMap[P any] map[string]xfield.Field[P]

func (s schemaMap[P]) Get(key string) (xfield.Field[P], bool) {
	field, ok := s[key]
	return field, ok
}

func (s schemaMap[P]) All() iter.Seq2[string, xfield.Field[P]] {
	return func(yield func(string, xfield.Field[P]) bool) {
		keys := make([]string, 0, len(s))
		for key := range s {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			if !yield(key, s[key]) {
				return
			}
		}
	}
}
