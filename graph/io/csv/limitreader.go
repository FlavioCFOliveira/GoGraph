package csv

import "io"

// limitReader wraps an [io.Reader] and fails with [ErrInputTooLarge]
// once total consumption would exceed maxBytes. Unlike [io.LimitReader],
// which reports a clean EOF at the limit (silently truncating the input),
// limitReader permits exactly maxBytes bytes and returns the error on the
// byte that crosses the ceiling. This lets streaming decoders abort
// before a single oversized token is fully buffered, keeping allocation
// bounded.
//
// maxBytes must be greater than zero; callers gate the wrap on that condition.
type limitReader struct {
	r         io.Reader
	remaining int64 // bytes still permitted before the cap is exceeded
}

// newLimitReader returns a [limitReader] permitting up to maxBytes bytes from r.
func newLimitReader(r io.Reader, maxBytes int64) *limitReader {
	return &limitReader{r: r, remaining: maxBytes}
}

// Read implements [io.Reader]. It reads from the wrapped reader and
// returns [ErrInputTooLarge] as soon as the cumulative byte count would
// exceed the configured ceiling, surfacing any bytes read up to that
// point alongside the error.
func (l *limitReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, ErrInputTooLarge
	}
	// Read at most one byte beyond the remaining budget so that an input
	// exactly equal to the cap succeeds, while the first excess byte is
	// detected and rejected.
	if int64(len(p)) > l.remaining+1 {
		p = p[:l.remaining+1]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return n, ErrInputTooLarge
	}
	return n, err
}
