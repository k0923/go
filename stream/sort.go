package stream

import "sort"

// Sort returns a stream ordered according to lessFn.
func (stream *Stream[T]) Sort(lessFn func(left T, right T) bool) *Stream[T] {
	return newStream(sortedIteratorFactory(stream.sequence.newIterator, lessFn))
}

func sortedIteratorFactory[T any](
	upstream iteratorFactory[T],
	lessFn func(left T, right T) bool,
) iteratorFactory[T] {
	return func() iterator[T] {
		source := upstream()
		items := make([]T, 0)
		for item, ok := source.next(); ok; item, ok = source.next() {
			items = append(items, item)
		}

		sort.Slice(items, func(i, j int) bool {
			return lessFn(items[i], items[j])
		})
		return &arrayIterator[T]{items: items}
	}
}
