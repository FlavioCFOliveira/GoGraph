package wal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Reader iterates the frames of a WAL file. It is read-only and
// stops cleanly at the first torn or corrupted frame, reporting the
// byte offset where the cut occurred via [Reader.TailOffset].
//
// A Reader opened by [OpenReader] on the base path of a segmented log
// iterates that store's whole log instead: see [OpenReader].
//
// Reader is not safe for concurrent use; create one Reader per
// goroutine that wishes to iterate.
type Reader struct {
	src       io.Reader
	closer    io.Closer
	tailErr   error
	bufr      *bufio.Reader
	tail      int64
	totalRead int64
	// log, when non-nil, makes this Reader iterate a segmented store's log.
	log *Log
}

// OpenReader opens path for read-only frame iteration.
//
// When path is the base path of a segmented log (a control file exists at
// [ControlPath](path)), the Reader iterates the store's log: the frames of the
// legacy single-file log except its seal, then the segment frames from the
// oldest retained segment, validated as [Log.Frames] validates them; and
// [Reader.TailOffset] reports the end position of the log. Otherwise it reads
// path as one file of frames.
func OpenReader(path string) (*Reader, error) {
	defer metrics.Time("store.wal.OpenReader").Stop()
	if _, err := os.Stat(ControlPath(path)); err == nil {
		l, err := OpenLog(path)
		if err != nil {
			metrics.IncCounter("store.wal.OpenReader.errors", 1)
			return nil, fmt.Errorf("wal: open %q: %w", path, err)
		}
		return &Reader{log: l}, nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|walNoFollow, 0) //nolint:gosec // caller-supplied path is by-design; walNoFollow rejects a symlinked final component (CWE-59)
	if err != nil {
		metrics.IncCounter("store.wal.OpenReader.errors", 1)
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
	if r.log != nil {
		return r.log.Close()
	}
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// TailOffset returns the byte offset (from the start of the input)
// where iteration stopped. After a successful iteration to EOF this
// equals the file size; after a torn frame this equals the start of
// the torn frame.
func (r *Reader) TailOffset() int64 {
	if r.log != nil {
		return r.log.TailOffset()
	}
	return r.tail
}

// TailError returns the error that ended iteration (typically
// [ErrTornFrame], [ErrCRCMismatch], or [ErrBadMagic]), or nil when
// iteration ended at clean EOF.
func (r *Reader) TailError() error {
	if r.log != nil && r.tailErr == nil {
		return r.log.TailError()
	}
	return r.tailErr
}

// Frames returns an iterator over every frame in the WAL. The
// iterator stops at the first error; call [Reader.TailError] /
// [Reader.TailOffset] after iteration to inspect why.
func (r *Reader) Frames() iter.Seq[Frame] {
	if r.log != nil {
		return r.logFrames()
	}
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
			r.totalRead += int64(FrameSize(frame))
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
	defer metrics.Time("store.wal.Replay").Stop()
	for f := range r.Frames() {
		if err := apply(f); err != nil {
			metrics.IncCounter("store.wal.Replay.errors", 1)
			return err
		}
	}
	if tErr := r.TailError(); tErr != nil && !errors.Is(tErr, ErrTornFrame) {
		metrics.IncCounter("store.wal.Replay.errors", 1)
		return tErr
	}
	return nil
}

// logFrames iterates a segmented store's log: the legacy file's frames except
// its seal, then the segment frames.
func (r *Reader) logFrames() iter.Seq[Frame] {
	return func(yield func(Frame) bool) {
		lr, err := r.log.LegacyReader()
		if err != nil {
			r.tailErr = err
			return
		}
		if lr != nil {
			for f := range lr.Frames() {
				if _, seal := DecodeLegacySeal(f.Payload); seal {
					continue
				}
				if !yield(f) {
					_ = lr.Close()
					return
				}
			}
			_ = lr.Close()
			if err := lr.TailError(); err != nil {
				r.tailErr = err
				return
			}
		}
		for f := range r.log.Frames() {
			if !yield(f) {
				return
			}
		}
	}
}
