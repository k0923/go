package stream_test

import (
	"reflect"
	"testing"

	"github.com/k0923/go/stream"
)

func TestExportedStreamType(t *testing.T) {
	var numbers *stream.Stream[int] = stream.Of(1, 2, 3)

	if got, want := numbers.ToSlice(), []int{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ToSlice() = %v, want %v", got, want)
	}
}

func TestMapReturnsChainableStream(t *testing.T) {
	lengths := stream.Of("alice", "bob", "amy").
		Map(func(name string) int { return len(name) }).
		Filter(func(length int) bool { return length > 3 }).
		Sort(func(left, right int) bool { return left < right }).
		Distinct(func(length int) int { return length }).
		ToSlice()

	if want := []int{5}; !reflect.DeepEqual(lengths, want) {
		t.Fatalf("mapped pipeline = %v, want %v", lengths, want)
	}
}

func TestGenericMethodsExposeCompleteAPI(t *testing.T) {
	grouped := stream.Of("alice", "bob", "amy").
		GroupBy(func(name string) int { return len(name) }).
		ToDictionary()

	want := map[int][]string{
		3: {"bob", "amy"},
		5: {"alice"},
	}
	if !reflect.DeepEqual(grouped, want) {
		t.Fatalf("GroupBy() = %v, want %v", grouped, want)
	}
}
