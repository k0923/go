package stream

type mapIterator[S any, T any] struct {
	iterator iterator[S]
	mapFn    func(item S) T
}

func (iterator *mapIterator[S, T]) next() (T, bool) {
	item, ok := iterator.iterator.next()
	if !ok {
		var zero T
		return zero, false
	}
	return iterator.mapFn(item), true
}

// Map returns a stream containing values transformed by fn.
func (stream *Stream[S]) Map[T any](fn func(source S) T) *Stream[T] {
	upstream := stream.sequence.newIterator
	return newStream(func() iterator[T] {
		return &mapIterator[S, T]{
			iterator: upstream(),
			mapFn:    fn,
		}
	})
}
