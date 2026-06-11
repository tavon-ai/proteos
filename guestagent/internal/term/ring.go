package term

// ring is a fixed-capacity byte ring buffer holding the most recent `size`
// bytes written to it. The PTY reader writes into it unconditionally (whether
// or not anyone is attached) so a reattaching client can replay recent
// scrollback. It is not safe for concurrent use; Session guards it with a mutex.
type ring struct {
	buf    []byte
	size   int
	start  int // index of the oldest byte
	length int // number of bytes currently stored (≤ size)
}

func newRing(size int) *ring {
	if size < 1 {
		size = 1
	}
	return &ring{buf: make([]byte, size), size: size}
}

// Write appends p, evicting the oldest bytes once the buffer is full. Writing
// more than `size` bytes at once keeps only the trailing `size`.
func (r *ring) Write(p []byte) {
	if len(p) >= r.size {
		copy(r.buf, p[len(p)-r.size:])
		r.start = 0
		r.length = r.size
		return
	}
	end := (r.start + r.length) % r.size
	n := copy(r.buf[end:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}
	r.length += len(p)
	if r.length > r.size {
		overflow := r.length - r.size
		r.start = (r.start + overflow) % r.size
		r.length = r.size
	}
}

// snapshot returns a fresh copy of the buffered bytes in write order.
func (r *ring) snapshot() []byte {
	out := make([]byte, r.length)
	first := min(r.start+r.length, r.size)
	n := copy(out, r.buf[r.start:first])
	if n < r.length {
		copy(out[n:], r.buf)
	}
	return out
}
