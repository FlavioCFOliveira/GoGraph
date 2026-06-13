package graphml

import "io"

// limitReader wraps an [io.Reader] and fails with [ErrInputTooLarge]
// once total consumption would exceed maxBytes. Unlike [io.LimitReader],
// which reports a clean EOF at the limit (silently truncating the input),
// limitReader permits exactly maxBytes bytes and returns the error on the
// byte that crosses the ceiling.
//
// The cap bounds bytes drawn from the source, not the decoder's working
// set: [encoding/xml] may buffer a single unterminated token up to
// maxBytes before the limit trips, so peak transient RAM is a multiple of
// maxBytes (see [DefaultMaxBytes]). The cap's role is to stop a hostile
// stream from being read without end, not to make per-token allocation
// equal to the cap.
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
//
// When the final Read call arrives after the budget is exactly consumed,
// the underlying reader is probed once: a clean EOF passes through as
// (0, io.EOF), while any remaining data returns (0, ErrInputTooLarge).
func (l *limitReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		// Budget exhausted: probe one byte to distinguish "fits exactly"
		// (io.EOF → pass through) from "exceeds cap" (ErrInputTooLarge).
		var probe [1]byte
		n, err := l.r.Read(probe[:])
		if n == 0 && err == io.EOF {
			return 0, io.EOF
		}
		return 0, ErrInputTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}
