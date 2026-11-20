package telemetry

// ring is a fixed-size circular buffer that keeps the newest n values.
type ring[T any] struct {
	buf  []T
	next int
	full bool
}

func newRing[T any](n int) *ring[T] { return &ring[T]{buf: make([]T, n)} }

func (r *ring[T]) push(v T) {
	r.buf[r.next] = v
	r.next++
	if r.next == len(r.buf) {
		r.next, r.full = 0, true
	}
}

// each calls fn for every stored value, oldest first.
func (r *ring[T]) each(fn func(T)) {
	if r.full {
		for i := r.next; i < len(r.buf); i++ {
			fn(r.buf[i])
		}
	}
	for i := 0; i < r.next; i++ {
		fn(r.buf[i])
	}
}

func (r *ring[T]) last() (v T, ok bool) {
	if r.next == 0 && !r.full {
		return v, false
	}
	i := r.next - 1
	if i < 0 {
		i = len(r.buf) - 1
	}
	return r.buf[i], true
}
