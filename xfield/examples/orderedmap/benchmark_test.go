package orderedmap_test

import (
	"encoding/json"
	"fmt"
	"testing"

	orderedmap "github.com/wk8/go-ordered-map/v2"
)

type entry struct {
	key   string
	value any
}

var valueSink any
var intSink int
var jsonSink []byte

func entries(size int) []entry {
	items := make([]entry, size)
	for i := range items {
		items[i] = entry{key: fmt.Sprintf("field_%05d", i), value: i}
	}
	return items
}

func structures(items []entry) (map[string]any, *orderedmap.OrderedMap[string, any]) {
	native := make(map[string]any, len(items))
	ordered := orderedmap.New[string, any](len(items))
	for _, item := range items {
		native[item.key] = item.value
		ordered.Set(item.key, item.value)
	}
	return native, ordered
}

func BenchmarkLookup(b *testing.B) {
	for _, size := range []int{10, 100, 1000, 10000} {
		items := entries(size)
		native, ordered := structures(items)
		b.Run(fmt.Sprintf("N%d/map", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				valueSink = native[items[i%size].key]
			}
		})
		b.Run(fmt.Sprintf("N%d/ordered", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				valueSink, _ = ordered.Get(items[i%size].key)
			}
		})
		b.Run(fmt.Sprintf("N%d/slice", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				key := items[i%size].key
				for _, item := range items {
					if item.key == key {
						valueSink = item.value
						break
					}
				}
			}
		})
	}
}

func BenchmarkIterate(b *testing.B) {
	for _, size := range []int{10, 100, 1000, 10000} {
		items := entries(size)
		native, ordered := structures(items)
		b.Run(fmt.Sprintf("N%d/map", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sum := 0
				for _, value := range native {
					sum += value.(int)
				}
				intSink = sum
			}
		})
		b.Run(fmt.Sprintf("N%d/ordered", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sum := 0
				for pair := ordered.Oldest(); pair != nil; pair = pair.Next() {
					sum += pair.Value.(int)
				}
				intSink = sum
			}
		})
		b.Run(fmt.Sprintf("N%d/slice", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sum := 0
				for _, item := range items {
					sum += item.value.(int)
				}
				intSink = sum
			}
		})
	}
}

func BenchmarkBuild(b *testing.B) {
	for _, size := range []int{10, 100, 1000, 10000} {
		items := entries(size)
		b.Run(fmt.Sprintf("N%d/map", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				m := make(map[string]any, size)
				for _, item := range items {
					m[item.key] = item.value
				}
				valueSink = m
			}
		})
		b.Run(fmt.Sprintf("N%d/ordered", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				m := orderedmap.New[string, any](size)
				for _, item := range items {
					m.Set(item.key, item.value)
				}
				valueSink = m
			}
		})
		b.Run(fmt.Sprintf("N%d/slice", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				result := make([]entry, size)
				copy(result, items)
				valueSink = result
			}
		})
	}
}

func BenchmarkJSONEncode(b *testing.B) {
	for _, size := range []int{10, 100, 1000} {
		native, ordered := structures(entries(size))
		b.Run(fmt.Sprintf("N%d/map", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var err error
				jsonSink, err = json.Marshal(native)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("N%d/ordered", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var err error
				jsonSink, err = json.Marshal(ordered)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkJSONDecode(b *testing.B) {
	for _, size := range []int{10, 100, 1000} {
		native, _ := structures(entries(size))
		raw, err := json.Marshal(native)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("N%d/map", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var result map[string]any
				if err := json.Unmarshal(raw, &result); err != nil {
					b.Fatal(err)
				}
				valueSink = result
			}
		})
		b.Run(fmt.Sprintf("N%d/ordered", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var result orderedmap.OrderedMap[string, any]
				if err := json.Unmarshal(raw, &result); err != nil {
					b.Fatal(err)
				}
				valueSink = &result
			}
		})
	}
}
