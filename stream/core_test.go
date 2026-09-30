package stream

import (
	"reflect"
	"testing"
)

type student struct {
	name  string
	age   int
	class string
}

func TestStreamPipelineCanBeConsumedRepeatedly(t *testing.T) {
	numbers := Of(1, 2, 3, 4, 5).
		Filter(func(item int) bool { return item > 1 }).
		Filter(func(item int) bool { return item%2 == 0 }).
		Sort(func(left, right int) bool { return left > right }).
		Limit(2)

	want := []int{4, 2}
	if got := numbers.ToSlice(); !reflect.DeepEqual(got, want) {
		t.Fatalf("first ToSlice() = %v, want %v", got, want)
	}
	if got := numbers.ToSlice(); !reflect.DeepEqual(got, want) {
		t.Fatalf("second ToSlice() = %v, want %v", got, want)
	}
}

func TestDistinctCanBeConsumedRepeatedly(t *testing.T) {
	numbers := Of(1, 2, 1, 3, 2).
		Distinct(func(item int) int { return item })
	want := []int{1, 2, 3}

	if got := numbers.ToSlice(); !reflect.DeepEqual(got, want) {
		t.Fatalf("first ToSlice() = %v, want %v", got, want)
	}
	if got := numbers.ToSlice(); !reflect.DeepEqual(got, want) {
		t.Fatalf("second ToSlice() = %v, want %v", got, want)
	}
}

func TestStreamPreservesOperationOrder(t *testing.T) {
	t.Run("limit before filter", func(t *testing.T) {
		got := Of(1, 2, 3).
			Limit(1).
			Filter(func(item int) bool { return item > 1 }).
			ToSlice()

		if len(got) != 0 {
			t.Fatalf("ToSlice() = %v, want empty", got)
		}
	})

	t.Run("filter before limit", func(t *testing.T) {
		got := Of(1, 2, 3).
			Filter(func(item int) bool { return item > 1 }).
			Limit(1).
			ToSlice()

		if want := []int{2}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ToSlice() = %v, want %v", got, want)
		}
	})

	t.Run("limit before sort", func(t *testing.T) {
		got := Of(3, 1, 2).
			Limit(2).
			Sort(func(left, right int) bool { return left < right }).
			ToSlice()

		if want := []int{1, 3}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ToSlice() = %v, want %v", got, want)
		}
	})

	t.Run("distinct before sort", func(t *testing.T) {
		items := []student{
			{name: "first", age: 2, class: "A"},
			{name: "second", age: 1, class: "A"},
		}
		got := From(items).
			Distinct(func(item student) string { return item.class }).
			Sort(func(left, right student) bool { return left.age < right.age }).
			ToSlice()

		if want := []student{items[0]}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ToSlice() = %v, want %v", got, want)
		}
	})
}

func TestMapEvaluatesEachItemOnce(t *testing.T) {
	calls := 0
	got := Of(0, 0).
		Map(func(int) int {
			calls++
			return calls
		}).
		Filter(func(item int) bool { return item%2 == 0 }).
		ToSlice()

	if want := []int{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ToSlice() = %v, want %v", got, want)
	}
	if calls != 2 {
		t.Fatalf("map calls = %d, want 2", calls)
	}
}

func TestForEachVisitsEveryItem(t *testing.T) {
	got := make([]int, 0)
	Of(1, 2, 3).ForEach(func(item int) {
		got = append(got, item)
	})

	if want := []int{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ForEach() visited %v, want %v", got, want)
	}
}

func TestStreamFirstAndLast(t *testing.T) {
	numbers := Of(3, 1, 2).Sort(func(left, right int) bool { return left < right })

	if got, ok := numbers.First(); !ok || got != 1 {
		t.Fatalf("First() = (%d, %t), want (1, true)", got, ok)
	}
	if got, ok := numbers.Last(); !ok || got != 3 {
		t.Fatalf("Last() = (%d, %t), want (3, true)", got, ok)
	}

	if got, ok := Of[int]().First(); ok || got != 0 {
		t.Fatalf("empty First() = (%d, %t), want (0, false)", got, ok)
	}
	if got, ok := Of(1, 2).Filter(func(int) bool { return false }).Last(); ok || got != 0 {
		t.Fatalf("filtered Last() = (%d, %t), want (0, false)", got, ok)
	}
	if got, ok := Of(1, 2, 3).Limit(1).Last(); !ok || got != 1 {
		t.Fatalf("limited Last() = (%d, %t), want (1, true)", got, ok)
	}
}

func TestGroupByCanBeConsumedRepeatedly(t *testing.T) {
	students := []student{
		{name: "alice", class: "A"},
		{name: "bob", class: "B"},
		{name: "amy", class: "A"},
	}
	grouped := From(students).GroupBy(func(item student) string {
		return item.class
	})
	want := []Group[string, student]{
		{Key: "A", Values: []student{students[0], students[2]}},
		{Key: "B", Values: []student{students[1]}},
	}

	if got := grouped.ToSlice(); !reflect.DeepEqual(got, want) {
		t.Fatalf("first ToSlice() = %v, want %v", got, want)
	}
	if got := grouped.ToSlice(); !reflect.DeepEqual(got, want) {
		t.Fatalf("second ToSlice() = %v, want %v", got, want)
	}
	if got, ok := grouped.First(); !ok || !reflect.DeepEqual(got, want[0]) {
		t.Fatalf("First() = (%v, %t), want (%v, true)", got, ok, want[0])
	}
	if got, ok := grouped.Last(); !ok || !reflect.DeepEqual(got, want[1]) {
		t.Fatalf("Last() = (%v, %t), want (%v, true)", got, ok, want[1])
	}

	dictionary := grouped.ToDictionary()
	wantDictionary := map[string][]student{
		"A": {students[0], students[2]},
		"B": {students[1]},
	}
	if !reflect.DeepEqual(dictionary, wantDictionary) {
		t.Fatalf("ToDictionary() = %v, want %v", dictionary, wantDictionary)
	}
}

func TestGroupStreamCanBeChained(t *testing.T) {
	got := Of("alice", "bob", "amy", "bill").
		GroupBy(func(name string) int { return len(name) }).
		Filter(func(group Group[int, string]) bool { return len(group.Values) > 1 }).
		Map(func(group Group[int, string]) int { return group.Key }).
		ToSlice()

	if want := []int{3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("grouped pipeline = %v, want %v", got, want)
	}
}

func TestGroupStreamCanBeSortedAndLimited(t *testing.T) {
	got := Of("alice", "bob", "amy", "bill").
		GroupBy(func(name string) int { return len(name) }).
		Sort(func(left, right Group[int, string]) bool {
			return left.Key > right.Key
		}).
		Limit(2).
		ToSlice()

	want := []Group[int, string]{
		{Key: 5, Values: []string{"alice"}},
		{Key: 4, Values: []string{"bill"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grouped pipeline = %v, want %v", got, want)
	}
}

func TestDerivedStreamsAreIndependent(t *testing.T) {
	source := Of(1, 2, 3, 4)
	evens := source.Filter(func(item int) bool { return item%2 == 0 })
	firstTwo := source.Limit(2)

	if got, want := evens.ToSlice(), []int{2, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("evens = %v, want %v", got, want)
	}
	if got, want := firstTwo.ToSlice(), []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("firstTwo = %v, want %v", got, want)
	}
	if got, want := source.ToSlice(), []int{1, 2, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source = %v, want %v", got, want)
	}
}

func TestLimitNonPositiveReturnsEmptyStream(t *testing.T) {
	for _, limit := range []int{-1, 0} {
		if got := Of(1, 2, 3).Limit(limit).ToSlice(); len(got) != 0 {
			t.Fatalf("Limit(%d).ToSlice() = %v, want empty", limit, got)
		}
	}
}

func TestLimitCanExceedSourceLength(t *testing.T) {
	got := Of(1, 2).Limit(3).ToSlice()
	if want := []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ToSlice() = %v, want %v", got, want)
	}
}
