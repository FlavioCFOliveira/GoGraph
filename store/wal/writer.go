package wal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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
	f      walFile
	bw     *bufio.Writer
	closed atomic.Bool

	frames     atomic.Uint64
	bytes      atomic.Uint64
	syncs      atomic.Uint64
	syncFailed atomic.Uint64
}

// Open opens or creates the WAL file at path for append-only
// writing. The file is created with mode 0o600 (owner read/write
// only) if it does not already exist; existing complete frames are
// preserved and new frames are appended after them. The restrictive
// mode keeps the full graph mutation stream from being world-readable.
//
// When the existing file ends in a benign torn frame ([ErrTornFrame] —
// the crash-mid-write-after-last-fsync case), Open truncates the file
// to the last durable frame boundary and fsyncs it before returning,
// so new frames are never appended after torn junk that every reader
// would stop at; see discardTornTail. Files whose scan stops at
// genuine corruption (for example [ErrCRCMismatch] or [ErrBadMagic])
// are left byte-for-byte intact.
func Open(path string) (*Writer, error) {
	defer metrics.Time("store.wal.Open")()
	// Detect whether this call creates the file. A newly-created WAL file
	// needs a parent-directory fsync so its directory entry is durable;
	// without it, a crash inside the kernel writeback window could lose the
	// entire WAL even after a committed Sync — a Durability violation on the
	// first commit (audit gap F4, docs/acid-audit.md). The stat/open window
	// is benign: if a racing opener creates the file between the stat and the
	// OpenFile we merely skip a redundant directory fsync (the other opener
	// performs it), and WAL files are single-writer per this constructor's
	// contract.
	created := false
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		created = true
	}
	// 0o600: the WAL carries the full graph mutation stream and must not
	// be world-readable (audit finding L2). Append/sync/durability flags
	// are unchanged; only the create mode is tightened.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // caller-supplied path is by-design
	if err != nil {
		metrics.IncCounter("store.wal.Open.errors", 1)
		return nil, fmt.Errorf("wal: open %q: %w", path, err)
	}
	if created {
		// fsync the parent directory once so the new file's directory entry
		// is durable. Done only on create: appends mutate the inode (made
		// durable by Writer.Sync), not the directory entry, so a per-Sync
		// directory fsync would be wasted work on the commit hot path.
		if syncErr := parentDirFsync(path); syncErr != nil {
			_ = f.Close()
			metrics.IncCounter("store.wal.Open.errors", 1)
			return nil, fmt.Errorf("wal: fsync parent dir of %q: %w", path, syncErr)
		}
	} else if err := discardTornTail(f); err != nil {
		_ = f.Close()
		metrics.IncCounter("store.wal.Open.errors", 1)
		return nil, fmt.Errorf("wal: open %q: discard torn tail: %w", path, err)
	}
	return &Writer{
		f:  f,
		bw: bufio.NewWriterSize(f, 64*1024),
	}, nil
}

// discardTornTail scans an existing WAL file from offset 0 and, when
// the scan stops at a benign torn frame ([ErrTornFrame]) before the
// physical end of the file, truncates the file to the last durable
// frame boundary and fsyncs the result so the discard is itself
// durable.
//
// Without this, reopening a WAL whose previous writer crashed mid-frame
// would append new frames AFTER the torn junk: O_APPEND writes land at
// the physical end of the file, but every reader stops at the first
// torn frame, so each transaction committed after the reopen — whose
// Sync already acknowledged durability — would be permanently
// unreachable on the next replay. That is a Durability violation, and
// truncating first closes it: the torn bytes were never part of any
// acknowledged commit, so discarding them loses nothing.
//
// Genuine corruption inside an already-durable frame ([ErrCRCMismatch],
// [ErrBadMagic], [ErrUnsupportedVersion], [ErrFrameTooLarge]) is left
// untouched: the bytes are preserved for diagnosis, and the recovery
// layer (store/recovery) fail-stops with the same sentinel before any
// well-behaved caller reaches Open for append.
//
// The scan reads the whole file once; Open is called once at startup,
// never on the commit hot path.
func discardTornTail(f *os.File) error {
	r := NewReader(f, nil) // nil closer: Open retains ownership of f
	//nolint:revive // empty-block: the loop intentionally discards every
	// frame — iterating to exhaustion is what populates TailOffset and
	// TailError, which are the only outputs this scan needs.
	for range r.Frames() {
	}
	if tErr := r.TailError(); tErr != nil && !errors.Is(tErr, ErrTornFrame) {
		// Not a benign torn tail: preserve the bytes and let the recovery
		// layer surface the corruption. Reposition at the end so the
		// handle's offset matches the append position.
		_, err := f.Seek(0, io.SeekEnd)
		return err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	tail := r.TailOffset()
	if tail >= size {
		return nil // every byte is a complete frame; nothing to discard
	}
	if err := f.Truncate(tail); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	// Reposition at the new end of file. O_APPEND writes always land at
	// the physical end regardless of the handle's offset; the seek keeps
	// the offset consistent for non-append operations such as
	// [Writer.Truncate]'s size probe.
	_, err = f.Seek(tail, io.SeekStart)
	return err
}

// Append writes one frame with the given opaque payload to the
// underlying file. The frame is buffered in process memory; call
// [Writer.Sync] to durably commit.
func (w *Writer) Append(payload []byte) error {
	defer metrics.Time("store.wal.Append")()
	err := w.AppendCtx(context.Background(), payload)
	if err != nil {
		metrics.IncCounter("store.wal.Append.errors", 1)
	}
	return err
}

// AppendCtx is the context-aware variant of [Writer.Append]. ctx.Err()
// is checked before acquiring the internal mutex and again before
// writing; on cancellation returns the wrapped ctx.Err.
func (w *Writer) AppendCtx(ctx context.Context, payload []byte) error {
	defer metrics.Time("store.wal.AppendCtx")()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
		return err
	}
	if w.closed.Load() {
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
		return err
	}
	n, err := Encode(w.bw, Frame{Payload: payload})
	if err != nil {
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
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
	defer metrics.Time("store.wal.Sync")()
	err := w.SyncCtx(context.Background())
	if err != nil {
		metrics.IncCounter("store.wal.Sync.errors", 1)
	}
	return err
}

// SyncCtx is the context-aware variant of [Writer.Sync]. ctx.Err()
// is checked before acquiring the internal mutex; on cancellation
// returns the wrapped ctx.Err without flushing.
func (w *Writer) SyncCtx(ctx context.Context) error {
	defer metrics.Time("store.wal.SyncCtx")()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return err
	}
	if w.closed.Load() {
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return err
	}
	if err := w.bw.Flush(); err != nil {
		w.syncFailed.Add(1)
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.syncFailed.Add(1)
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
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
	defer metrics.Time("store.wal.Truncate")()
	if w.closed.Load() {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return 0, ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return 0, err
	}
	sz, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return 0, err
	}
	if err := w.f.Truncate(0); err != nil {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return sz, err
	}
	// Crash-injection point: the file has just been shrunk to zero on
	// disk but the truncation has not yet been fully finalised (seek +
	// metadata sync + buffer reset). A crash here models a tear in the
	// middle of a checkpoint's WAL truncation; recovery must reconstruct
	// the full state from the (already durable, self-sufficient)
	// snapshot alone, since the WAL is now empty. No-op in production
	// (GOGRAPH_CRASH_AT unset). The hook lives here because os.File
	// truncation is where the WAL prefix is physically discarded; see
	// store/checkpoint.runCheckpoint for the surrounding sequence.
	crashpoint.Breakpoint("checkpoint.mid-truncate")
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return sz, err
	}
	if err := w.f.Sync(); err != nil {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return sz, err
	}
	w.bw.Reset(w.f)
	return sz, nil
}

// Close flushes any buffered frames, calls Sync once, and releases
// the underlying file.
func (w *Writer) Close() error {
	defer metrics.Time("store.wal.Close")()
	if !w.closed.CompareAndSwap(false, true) {
		metrics.IncCounter("store.wal.Close.errors", 1)
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close() // best-effort: already on error path, flush err preserved
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close() // best-effort: already on error path, sync err preserved
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	if err := w.f.Close(); err != nil {
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	return nil
}
