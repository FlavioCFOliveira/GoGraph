package wal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// ErrWriterClosed is returned by methods on a [Writer] that has
// already been closed.
var ErrWriterClosed = errors.New("wal: writer is closed")

// Stats is a snapshot of a [Writer]'s lifetime counters. Counters
// are monotonic; subtract two snapshots to compute deltas. Values
// are read with [sync/atomic.LoadUint64], so they may race slightly
// behind in-flight operations but never observe a torn value.
type Stats struct {
	Frames     uint64 // total frames appended
	Bytes      uint64 // total bytes appended (header + payload)
	Syncs      uint64 // total Sync calls
	SyncFailed uint64 // Sync calls that returned an error
}

// Writer is a single-writer append-only log file. Callers append
// frames with [Writer.Append] and durably commit them with
// [Writer.Sync]; group-commit is achieved by appending several
// frames before a single Sync.
//
// Writer is safe for concurrent calls to [Writer.Append] / Sync /
// Stats; all mutations serialise on an internal mutex.
type Writer struct {
	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	closed atomic.Bool

	frames     atomic.Uint64
	bytes      atomic.Uint64
	syncs      atomic.Uint64
	syncFailed atomic.Uint64
}

// Open opens or creates the WAL file at path for append-only
// writing. The file is created with mode 0o644 if it does not
// already exist; existing data is preserved and new frames are
// appended.
func Open(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644) //nolint:gosec // caller-supplied path is by-design
	if err != nil {
		return nil, fmt.Errorf("wal: open %q: %w", path, err)
	}
	return &Writer{
		f:  f,
		bw: bufio.NewWriterSize(f, 64*1024),
	}, nil
}

// Append writes one frame with the given opaque payload to the
// underlying file. The frame is buffered in process memory; call
// [Writer.Sync] to durably commit.
func (w *Writer) Append(payload []byte) error {
	if w.closed.Load() {
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := Encode(w.bw, Frame{Payload: payload})
	if err != nil {
		return err
	}
	w.frames.Add(1)
	w.bytes.Add(uint64(n))
	return nil
}

// Sync flushes the buffered frames to the OS and then calls
// [os.File.Sync] so the data reaches durable storage before
// returning. It must be invoked at every transaction commit
// boundary.
func (w *Writer) Sync() error {
	if w.closed.Load() {
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		w.syncFailed.Add(1)
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.syncFailed.Add(1)
		return err
	}
	w.syncs.Add(1)
	return nil
}

// Stats returns a snapshot of the writer's lifetime counters.
func (w *Writer) Stats() Stats {
	return Stats{
		Frames:     w.frames.Load(),
		Bytes:      w.bytes.Load(),
		Syncs:      w.syncs.Load(),
		SyncFailed: w.syncFailed.Load(),
	}
}

// Truncate empties the WAL: flushes any buffered frames, truncates
// the underlying file to zero bytes, and fsyncs the result so the
// empty state is durable on disk before returning. Subsequent
// [Writer.Append] calls write to offset 0 of the freshly-empty file.
//
// Truncate is intended to be called by the checkpointer after a
// snapshot covering all WAL frames has been durably persisted; on
// success every frame previously durable in the WAL is logically
// folded into the snapshot.
//
// Lifetime counters in [Writer.Stats] are NOT reset; the returned
// int64 reports the number of bytes that were in the file at the
// moment of truncation (after the in-memory buffer was flushed),
// which is the canonical measure of WAL bytes freed by this call.
//
// On error the WAL may be in a partially-truncated state; callers
// should not continue using the Writer.
func (w *Writer) Truncate() (int64, error) {
	if w.closed.Load() {
		return 0, ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		return 0, err
	}
	sz, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if err := w.f.Truncate(0); err != nil {
		return sz, err
	}
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return sz, err
	}
	if err := w.f.Sync(); err != nil {
		return sz, err
	}
	w.bw.Reset(w.f)
	return sz, nil
}

// Close flushes any buffered frames, calls Sync once, and releases
// the underlying file.
func (w *Writer) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()
		return err
	}
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}
