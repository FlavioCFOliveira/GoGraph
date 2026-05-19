package wal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
)

// Reader iterates the frames of a WAL file. It is read-only and
// stops cleanly at the first torn or corrupted frame, reporting the
// byte offset where the cut occurred via [Reader.TailOffset].
//
// Reader is not safe for concurrent use; create one Reader per
// goroutine that wishes to iterate.
type Reader struct {
	src       io.Reader
	closer    io.Closer
	bufr      *bufio.Reader
	tail      int64
	tailErr   error
	totalRead int64
}

// OpenReader opens path for read-only frame iteration.
func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied path is by-design
	if err != nil {
		return nil, fmt.Errorf("wal: open %q: %w", path, err)
	}
	return NewReader(f, f), nil
}

// NewReader builds a Reader over an io.Reader. closer may be nil if
// the caller owns the resource.
func NewReader(r io.Reader, closer io.Closer) *Reader {
	return &Reader{
		src:    r,
		closer: closer,
		bufr:   bufio.NewReaderSize(r, 64*1024),
	}
}

// Close releases any underlying resource passed to [NewReader] or
// [OpenReader].
func (r *Reader) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// TailOffset returns the byte offset (from the start of the input)
// where iteration stopped. After a successful iteration to EOF this
// equals the file size; after a torn frame this equals the start of
// the torn frame.
func (r *Reader) TailOffset() int64 { return r.tail }

// TailError returns the error that ended iteration (typically
// [ErrTornFrame], [ErrCRCMismatch], or [ErrBadMagic]), or nil when
// iteration ended at clean EOF.
func (r *Reader) TailError() error { return r.tailErr }

// Frames returns an iterator over every frame in the WAL. The
// iterator stops at the first error; call [Reader.TailError] /
// [Reader.TailOffset] after iteration to inspect why.
func (r *Reader) Frames() iter.Seq[Frame] {
	return func(yield func(Frame) bool) {
		for {
			beforeRead := r.totalRead
			frame, err := r.decodeOne()
			if err != nil {
				r.tail = beforeRead
				if errors.Is(err, io.EOF) {
					r.tailErr = nil
				} else {
					r.tailErr = err
				}
				return
			}
			r.totalRead += int64(HeaderSize + len(frame.Payload))
			if !yield(frame) {
				r.tail = r.totalRead
				return
			}
		}
	}
}

// decodeOne reads one frame and returns either it and nil error or a
// zero Frame and a clean-EOF/torn/corrupted error.
func (r *Reader) decodeOne() (Frame, error) {
	// Peek one byte to distinguish clean EOF from a torn frame.
	if _, err := r.bufr.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}
		return Frame{}, err
	}
	return Decode(r.bufr)
}

// Replay applies apply to every frame in the WAL in order. If apply
// returns an error, replay stops with that error returned to the
// caller. After Replay returns, TailOffset/TailError describe where
// and why iteration stopped (frame-level errors).
func (r *Reader) Replay(apply func(Frame) error) error {
	for f := range r.Frames() {
		if err := apply(f); err != nil {
			return err
		}
	}
	if r.tailErr != nil && !errors.Is(r.tailErr, ErrTornFrame) {
		return r.tailErr
	}
	return nil
}
