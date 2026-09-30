package stream

// Group contains the values associated with a key.
type Group[K comparable, T any] struct {
	Key    K
	Values []T
}

// GroupStream represents a reusable sequence produced by GroupBy.
type GroupStream[K comparable, T any] struct {
	sequence *sequence[Group[K, T]]
}

func newGroupStream[K comparable, T any](
	factory iteratorFactory[Group[K, T]],
) *GroupStream[K, T] {
	return &GroupStream[K, T]{sequence: newSequence(factory)}
}

// GroupBy returns a grouped stream in first-key-occurrence order.
func (stream *Stream[T]) GroupBy[K comparable](fn func(item T) K) *GroupStream[K, T] {
	upstream := stream.sequence.newIterator
	return newGroupStream(func() iterator[Group[K, T]] {
		source := upstream()
		groups := make([]Group[K, T], 0)
		indexes := make(map[K]int)

		for item, ok := source.next(); ok; item, ok = source.next() {
			key := fn(item)
			index, exists := indexes[key]
			if !exists {
				index = len(groups)
				indexes[key] = index
				groups = append(groups, Group[K, T]{Key: key})
			}
			groups[index].Values = append(groups[index].Values, item)
		}

		return &arrayIterator[Group[K, T]]{items: groups}
	})
}

// First returns the first group and reports whether one exists.
func (stream *GroupStream[K, T]) First() (Group[K, T], bool) {
	return stream.sequence.first()
}

// Last returns the last group and reports whether one exists.
func (stream *GroupStream[K, T]) Last() (Group[K, T], bool) {
	return stream.sequence.last()
}

// ForEach invokes fn once for every group.
func (stream *GroupStream[K, T]) ForEach(fn func(group Group[K, T])) {
	stream.sequence.forEach(fn)
}

// ToSlice collects the grouped stream into a slice.
func (stream *GroupStream[K, T]) ToSlice() []Group[K, T] {
	return stream.sequence.toSlice()
}

// Filter returns a grouped stream containing groups accepted by fn.
func (stream *GroupStream[K, T]) Filter(fn func(group Group[K, T]) bool) *GroupStream[K, T] {
	upstream := stream.sequence.newIterator
	return newGroupStream(func() iterator[Group[K, T]] {
		return &filterIterator[Group[K, T]]{
			iterator: upstream(),
			filter:   fn,
		}
	})
}

// Limit returns a grouped stream containing at most num groups.
func (stream *GroupStream[K, T]) Limit(num int) *GroupStream[K, T] {
	upstream := stream.sequence.newIterator
	return newGroupStream(func() iterator[Group[K, T]] {
		return &limitIterator[Group[K, T]]{
			iterator:  upstream(),
			remaining: num,
		}
	})
}

// Sort returns a grouped stream ordered according to lessFn.
func (stream *GroupStream[K, T]) Sort(
	lessFn func(left Group[K, T], right Group[K, T]) bool,
) *GroupStream[K, T] {
	return newGroupStream(sortedIteratorFactory(stream.sequence.newIterator, lessFn))
}

// Map returns a stream containing values transformed from groups by fn.
func (stream *GroupStream[K, T]) Map[R any](fn func(group Group[K, T]) R) *Stream[R] {
	upstream := stream.sequence.newIterator
	return newStream(func() iterator[R] {
		return &mapIterator[Group[K, T], R]{
			iterator: upstream(),
			mapFn:    fn,
		}
	})
}

// ToDictionary collects groups into a dictionary keyed by Group.Key.
func (stream *GroupStream[K, T]) ToDictionary() map[K][]T {
	result := make(map[K][]T)
	stream.ForEach(func(group Group[K, T]) {
		result[group.Key] = group.Values
	})
	return result
}
