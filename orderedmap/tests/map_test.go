package orderedmap_test

import (
	"slices"
	"testing"

	"github.com/k0923/go/orderedmap"
)

func TestMapOperations(t *testing.T) {
	var m orderedmap.Map[int]
	if m.Len() != 0 || len(m.Keys()) != 0 || m.Delete("missing") {
		t.Fatal("zero value is not empty")
	}
	if value, ok := m.Get("missing"); ok || value != 0 {
		t.Fatalf("missing key: %v, %v", value, ok)
	}
	for range m.All() {
		t.Fatal("empty iterator yielded an entry")
	}

	m.Set("banana", 20)
	m.Set("apple", 10)
	m.Set("orange", 30)
	m.Set("banana", 25)
	if m.Len() != 3 || !slices.Equal(m.Keys(), []string{"banana", "apple", "orange"}) {
		t.Fatalf("unexpected order: %v", m.Keys())
	}
	var values []int
	for key, value := range m.All() {
		stored, ok := m.Get(key)
		if !ok || stored != value {
			t.Fatalf("iterator and lookup disagree for %q", key)
		}
		values = append(values, value)
	}
	if !slices.Equal(values, []int{25, 10, 30}) {
		t.Fatalf("unexpected values: %v", values)
	}
	count := 0
	for range m.All() {
		count++
		break
	}
	if count != 1 {
		t.Fatalf("break yielded %d times", count)
	}
	keys := m.Keys()
	keys[0] = "changed"
	if m.Keys()[0] != "banana" {
		t.Fatal("Keys exposes internal state")
	}
}

func TestDeleteAndReinsert(t *testing.T) {
	for _, key := range []string{"a", "b", "c"} {
		t.Run(key, func(t *testing.T) {
			var m orderedmap.Map[int]
			for i, key := range []string{"a", "b", "c"} {
				m.Set(key, i)
			}
			if !m.Delete(key) || m.Delete(key) {
				t.Fatal("incorrect deletion result")
			}
			want := slices.DeleteFunc([]string{"a", "b", "c"}, func(k string) bool { return k == key })
			if !slices.Equal(m.Keys(), want) || m.Len() != 2 {
				t.Fatalf("after delete: %v", m.Keys())
			}
			m.Set(key, 9)
			want = append(want, key)
			if !slices.Equal(m.Keys(), want) {
				t.Fatalf("after reinsert: %v", m.Keys())
			}
			for _, key := range want {
				m.Delete(key)
			}
			if m.Len() != 0 || len(m.Keys()) != 0 {
				t.Fatal("map not empty after deleting every key")
			}
			m.Set("", 42)
			if value, ok := m.Get(""); !ok || value != 42 || !slices.Equal(m.Keys(), []string{""}) {
				t.Fatal("cannot reuse emptied map with empty key")
			}
		})
	}
}
