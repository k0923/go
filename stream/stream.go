package stream

type sequence[T any] struct {
	newIterator iteratorFactory[T]
}

// Stream represents a reusable sequence of values.
type Stream[T any] struct {
	sequence *sequence[T]
}

// From creates a stream backed by items.
func From[T any](items []T) *Stream[T] {
	return newStream(func() iterator[T] {
		return &arrayIterator[T]{items: items}
	})
}

// Of creates a stream from the provided items.
func Of[T any](items ...T) *Stream[T] {
	return From(items)
}

func newStream[T any](factory iteratorFactory[T]) *Stream[T] {
	return &Stream[T]{sequence: newSequence(factory)}
}

func newSequence[T any](factory iteratorFactory[T]) *sequence[T] {
	return &sequence[T]{newIterator: factory}
}

// Filter returns a stream containing values accepted by fn.
func (stream *Stream[T]) Filter(fn func(item T) bool) *Stream[T] {
	upstream := stream.sequence.newIterator
	return newStream(func() iterator[T] {
		return &filterIterator[T]{
			iterator: upstream(),
			filter:   fn,
		}
	})
}

// Distinct returns a stream containing the first value for each key.
func (stream *Stream[T]) Distinct[K comparable](fn func(item T) K) *Stream[T] {
	upstream := stream.sequence.newIterator
	return newStream(func() iterator[T] {
		return &distinctIterator[T, K]{
			iterator: upstream(),
			key:      fn,
			seen:     make(map[K]struct{}),
		}
	})
}

// Limit returns a stream containing at most num values.
func (stream *Stream[T]) Limit(num int) *Stream[T] {
	upstream := stream.sequence.newIterator
	return newStream(func() iterator[T] {
		return &limitIterator[T]{
			iterator:  upstream(),
			remaining: num,
		}
	})
}

// First returns the first value and reports whether one exists.
func (stream *Stream[T]) First() (T, bool) {
	return stream.sequence.first()
}

func (stream *sequence[T]) first() (T, bool) {
	return stream.newIterator().next()
}

// Last returns the last value and reports whether one exists.
func (stream *Stream[T]) Last() (T, bool) {
	return stream.sequence.last()
}

func (stream *sequence[T]) last() (T, bool) {
	iterator := stream.newIterator()
	var last T
	found := false

	for item, ok := iterator.next(); ok; item, ok = iterator.next() {
		last = item
		found = true
	}

	return last, found
}

// ForEach invokes fn once for every value.
func (stream *Stream[T]) ForEach(fn func(item T)) {
	stream.sequence.forEach(fn)
}

func (stream *sequence[T]) forEach(fn func(item T)) {
	iterator := stream.newIterator()
	for item, ok := iterator.next(); ok; item, ok = iterator.next() {
		fn(item)
	}
}

// ToSlice collects the stream into a slice.
func (stream *Stream[T]) ToSlice() []T {
	return stream.sequence.toSlice()
}

func (stream *sequence[T]) toSlice() []T {
	data := make([]T, 0)
	stream.forEach(func(item T) {
		data = append(data, item)
	})
	return data
}
