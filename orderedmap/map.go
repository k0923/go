// Package orderedmap provides a string-keyed map with insertion-order iteration
// and JSON encoding. Only explicitly typed Map values preserve JSON key order.
package orderedmap

import (
	"iter"
	"slices"
)

// Map stores values in key insertion order. Its zero value is ready to use.
// Updating a key preserves its position; deleting and reinserting appends it.
//
// A Map must not be copied after first use. Concurrent access requires external
// synchronization if any caller writes. Do not mutate a Map while iterating it.
type Map[V any] struct {
	values map[string]V
	keys   []string
}

// Set inserts or updates a value.
func (m *Map[V]) Set(key string, value V) {
	if m.values == nil {
		m.values = make(map[string]V)
	}
	if _, exists := m.values[key]; !exists {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

// Get returns the value for key and whether it exists.
func (m *Map[V]) Get(key string) (V, bool) {
	value, ok := m.values[key]
	return value, ok
}

// Delete removes key and reports whether it existed.
// Removing an existing key takes O(n) time to preserve the remaining order.
func (m *Map[V]) Delete(key string) bool {
	if _, exists := m.values[key]; !exists {
		return false
	}
	delete(m.values, key)
	index := slices.Index(m.keys, key)
	// slices.Delete clears the unused tail, releasing its string reference.
	m.keys = slices.Delete(m.keys, index, index+1)
	return true
}

// Len returns the number of keys.
func (m *Map[V]) Len() int {
	return len(m.values)
}

// Keys returns an independent slice of keys in insertion order.
func (m *Map[V]) Keys() []string {
	keys := make([]string, len(m.keys))
	copy(keys, m.keys)
	return keys
}

// All returns an iterator over keys and values in insertion order.
// The iterator is not a snapshot; do not mutate m during iteration.
func (m *Map[V]) All() iter.Seq2[string, V] {
	return func(yield func(string, V) bool) {
		for _, key := range m.keys {
			if !yield(key, m.values[key]) {
				return
			}
		}
	}
}
