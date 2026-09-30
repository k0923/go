package stream

type iterator[T any] interface {
	next() (T, bool)
}

type iteratorFactory[T any] func() iterator[T]

type arrayIterator[T any] struct {
	items []T
	index int
}

func (iterator *arrayIterator[T]) next() (T, bool) {
	if iterator.index >= len(iterator.items) {
		var zero T
		return zero, false
	}

	item := iterator.items[iterator.index]
	iterator.index++
	return item, true
}

type filterIterator[T any] struct {
	iterator iterator[T]
	filter   func(item T) bool
}

func (iterator *filterIterator[T]) next() (T, bool) {
	for {
		item, ok := iterator.iterator.next()
		if !ok {
			var zero T
			return zero, false
		}
		if iterator.filter(item) {
			return item, true
		}
	}
}

type distinctIterator[T any, K comparable] struct {
	iterator iterator[T]
	key      func(item T) K
	seen     map[K]struct{}
}

func (iterator *distinctIterator[T, K]) next() (T, bool) {
	for {
		item, ok := iterator.iterator.next()
		if !ok {
			var zero T
			return zero, false
		}

		key := iterator.key(item)
		if _, exists := iterator.seen[key]; exists {
			continue
		}

		iterator.seen[key] = struct{}{}
		return item, true
	}
}

type limitIterator[T any] struct {
	iterator  iterator[T]
	remaining int
}

func (iterator *limitIterator[T]) next() (T, bool) {
	if iterator.remaining <= 0 {
		var zero T
		return zero, false
	}

	item, ok := iterator.iterator.next()
	if !ok {
		var zero T
		return zero, false
	}

	iterator.remaining--
	return item, true
}
