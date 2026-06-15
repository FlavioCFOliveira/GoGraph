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

// ErrWALLocked is returned by [Open] when another process already holds
// the exclusive OS-level lock on the WAL directory. It signals that the
// WAL is in active use and the caller must not open a second writer
// against it — doing so would silently interleave frames and corrupt the
// log.
var ErrWALLocked = errors.New("wal: WAL directory is locked by another process")

// Stats is a snapshot of a [Writer]'s lifetime counters. Counters
// are monotonic; subtract two snapshots to compute deltas. Values
// are read with [sync/atomic.LoadUint64], so they may race slightly
// behind in-flight operations but never observe a torn value.
type Stats struct {
	Frames uint64 // total frames appended
	Bytes  uint64 // total bytes appended (header + payload)
	Syncs  uint64 // total Sync calls
	// SyncFailed counts Sync calls that failed at the flush/fsync
	// I/O layer. Calls rejected because the writer was already
	// poisoned by an earlier failure are not counted (mirroring how
	// context-cancelled calls are not counted).
	SyncFailed uint64
}

// Writer is a single-writer append-only log file. Callers append
// frames with [Writer.Append] and durably commit them with
// [Writer.Sync]; group-commit is achieved by appending several
// frames before a single Sync.
//
// Writer is safe for concurrent calls to [Writer.Append] / Sync /
// Stats; all mutations serialise on an internal mutex.
//
// A Writer fail-stops on commit failure: the first flush or fsync
// error in [Writer.Sync] permanently poisons the writer. The
// un-synced suffix of the file — which may hold the flushed frames
// (including the commit marker) of the very transaction whose Sync
// just failed — is physically discarded, and every subsequent
// Append/Sync returns the original error. Without the poison, a
// later transaction's successful fsync would make the failed
// transaction's frames durable even though its commit was never
// acknowledged, and recovery would replay it: a phantom commit
// violating Atomicity and Durability. A poisoned Writer accepts only
// [Writer.Close]; the owner must discard it and re-open the WAL,
// which re-validates the tail.
type Writer struct {
	mu     sync.Mutex
	f      walFile
	bw     *bufio.Writer
	closed atomic.Bool

	// lockFile is the open handle of the WAL directory LOCK file whose
	// flock(2) / O_EXCL lifetime is tied to this Writer. It is non-nil
	// only for Writers created via [Open] (not [OpenWith], which is used
	// exclusively by tests that supply synthetic walFile implementations).
	// Released by Close via releaseLock.
	lockFile *os.File

	// syncErr is the sticky poison error: set under mu by the first
	// flush/fsync failure in SyncCtx and never cleared. While non-nil
	// every AppendCtx/SyncCtx call returns it without touching the
	// file.
	syncErr error
	// durableSize is the file size, in bytes, covered by the last
	// successful fsync (or the size observed at open). Guarded by mu.
	// It is the truncation target when a sync failure must discard
	// the un-synced suffix.
	durableSize int64
	// appendedSize is durableSize plus every frame byte accepted by
	// AppendCtx since the last successful fsync — the logical file
	// size once the buffer is flushed. Guarded by mu. Tracking it
	// incrementally keeps the commit hot path free of size-probing
	// seek syscalls.
	appendedSize int64

	// --- group-commit coordination (Writer-owned, all under mu) ---
	//
	// SyncGroup implements PostgreSQL-XLogFlush-style commit coalescing: a
	// committer records the watermark (appendedSize) covering its last
	// appended frame, then either fsyncs the whole buffered suffix once as
	// the group LEADER or, if a leader is already flushing, waits on
	// groupCond until a fsync covers its watermark. One fsync therefore
	// makes many committers' frames durable, amortising the ~per-commit fsync
	// cost across the group. The watermark is the Writer's own appendedSize /
	// durableSize, taken under mu, so the coordinator never tracks byte
	// offsets independently of the file (the load-bearing audit invariant).

	// groupCond signals waiters when a sync round completes (durableSize
	// advanced) or the writer is poisoned. Its locker is &mu.
	groupCond *sync.Cond
	// leaderActive is true while one committer is performing the group
	// flush+fsync. While set, no other committer starts a competing fsync;
	// arriving committers wait on groupCond. It guarantees a single leader
	// per round so two goroutines never flush the same Writer concurrently.
	leaderActive bool

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

	// Acquire an exclusive OS-level lock on the WAL directory before
	// touching any WAL data. The lock is held for the lifetime of this
	// Writer and released by Close. Without it two processes opening the
	// same path would silently interleave WAL frames, corrupting the log.
	//
	// acquireLock creates (or opens) a "LOCK" sentinel file in the same
	// directory as path and calls flock(2)/O_EXCL on it; see lock_unix.go
	// and lock_other.go for the per-platform implementation.
	lockPath := path + ".lock"
	lockFile, err := acquireLock(lockPath)
	if err != nil {
		metrics.IncCounter("store.wal.Open.errors", 1)
		return nil, err // ErrWALLocked or a wrapped OS error
	}

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
		releaseLock(lockFile)
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
			releaseLock(lockFile)
			metrics.IncCounter("store.wal.Open.errors", 1)
			return nil, fmt.Errorf("wal: fsync parent dir of %q: %w", path, syncErr)
		}
	} else if err := discardTornTail(f); err != nil {
		_ = f.Close()
		releaseLock(lockFile)
		metrics.IncCounter("store.wal.Open.errors", 1)
		return nil, fmt.Errorf("wal: open %q: discard torn tail: %w", path, err)
	}
	// Record the opening size as the durable baseline: every byte
	// already in the file at open time is presumed durable (a benign
	// torn tail was discarded and fsynced above), so a later sync
	// failure rolls the file back exactly here.
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		releaseLock(lockFile)
		metrics.IncCounter("store.wal.Open.errors", 1)
		return nil, fmt.Errorf("wal: open %q: probe size: %w", path, err)
	}
	w := &Writer{
		f:            f,
		lockFile:     lockFile,
		bw:           bufio.NewWriterSize(f, 64*1024),
		durableSize:  size,
		appendedSize: size,
	}
	w.groupCond = sync.NewCond(&w.mu)
	return w, nil
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
//
// On a writer poisoned by an earlier Sync failure, Append rejects
// the frame and returns the original sync error; see the [Writer]
// type documentation.
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
	if w.syncErr != nil {
		// Poisoned by an earlier Sync failure: accepting more frames
		// would buffer them after a discarded (never-acknowledged)
		// suffix. Fail-stop with the original error.
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
		return w.syncErr
	}
	n, err := Encode(w.bw, Frame{Payload: payload})
	if err != nil {
		// A partial frame may now sit in the buffer (and, when the
		// buffer spilled, partially in the file). bufio's sticky error
		// guarantees the next Flush fails, so SyncCtx poisons the
		// writer and discards the partial bytes before any later sync
		// could acknowledge them.
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
		return err
	}
	w.appendedSize += int64(n)
	w.frames.Add(1)
	w.bytes.Add(uint64(n))
	return nil
}

// Sync flushes the buffered frames to the OS and then calls
// [os.File.Sync] so the data reaches durable storage before
// returning. It must be invoked at every transaction commit
// boundary.
//
// The first flush or fsync failure permanently poisons the writer:
// the un-synced suffix of the file is discarded and every subsequent
// Append/Sync returns the original error; see the [Writer] type
// documentation.
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
	if w.syncErr != nil {
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return w.syncErr
	}
	if err := w.bw.Flush(); err != nil {
		w.poison(err)
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.poison(err)
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return err
	}
	w.durableSize = w.appendedSize
	w.syncs.Add(1)
	// Wake any group-commit waiter: a direct Sync (e.g. the checkpointer)
	// advances durableSize past their watermark, so they are now durable
	// without electing their own leader.
	if w.groupCond != nil {
		w.groupCond.Broadcast()
	}
	return nil
}

// SyncGroup durably commits the caller's already-appended frames, coalescing
// the fsync with those of every other committer whose frames are buffered at
// the same time — PostgreSQL-XLogFlush-style group commit. It returns nil only
// after a single os.File.Sync has made durable every byte up to and including
// the caller's last appended frame (its OpCommit marker); a caller therefore
// acknowledges its commit only once the marker is on stable storage, exactly
// as [Writer.Sync] does, but without paying a private fsync per commit.
//
// # Contract
//
// SyncGroup must be called AFTER the caller has appended all of its frames via
// [Writer.Append] / [Writer.AppendCtx] and is the durability barrier for those
// frames. It captures the current appendedSize as the caller's watermark, then:
//
//   - If the writer is already poisoned, it returns the sticky error
//     immediately (the un-synced suffix, including this caller's frames, was
//     discarded by an earlier failed sync).
//   - If a previous sync has already advanced durableSize past the caller's
//     watermark (a concurrent leader covered it), it returns nil without any
//     I/O — the follower fast path.
//   - Otherwise, if no leader is flushing, the caller becomes the LEADER:
//     it flushes the buffer and fsyncs once, covering its own and every other
//     buffered committer's frames, publishes the new durableSize, and wakes the
//     followers. If a leader is already flushing, the caller waits on the group
//     condition until durableSize covers its watermark or the writer poisons.
//
// # Durability, atomicity, and failure semantics
//
//   - DURABILITY: success is returned only after the fsync covering the
//     caller's marker completes. Because all appends serialise (the buffer is
//     FIFO and O_APPEND lands every write at EOF), a marker whose end offset
//     is <= the flushed appendedSize is made durable by that flush's fsync;
//     there is no prefix-only fsync on a local file system.
//   - ATOMICITY: the on-disk frame stream is unchanged from the per-commit
//     path — each transaction's ops are contiguous and followed by its
//     OpCommit marker — so a crash mid-leader-fsync recovers each fully-marked
//     transaction and discards the unmarked tail exactly as before.
//   - FAIL-ALL: if the leader's flush or fsync fails, [Writer.poison] discards
//     the entire un-synced suffix (every group member's frames and markers) and
//     broadcasts; every waiter then observes the sticky error and fails its own
//     commit. No member may believe it committed when the shared fsync failed.
//
// # Cancellation
//
// SyncGroup is intentionally NOT context-aware. Once a committer's frames are
// in the shared buffer they cannot be un-appended (later committers' frames sit
// after them, and the transaction sequence is consumed), so abandoning the wait
// while the group still fsyncs the frames would make the transaction durable —
// recovery replays a fully-marked transaction — while returning an error to the
// caller, risking a double apply on retry. The caller's deadline is honoured
// earlier, at the cancellable single-writer acquire ([Store.BeginCtx]); after
// the append point the commit is in-flight-durable and the wait for its covering
// fsync runs to completion (the marker either becomes durable or the writer
// poisons and fails it). This matches PostgreSQL: a backend cannot un-write WAL
// it has already inserted.
//
// Concurrency: safe for concurrent calls; it serialises on the same internal
// mutex as Append/Sync and guarantees a single leader per fsync round.
func (w *Writer) SyncGroup() error {
	defer metrics.Time("store.wal.SyncGroup")()
	if w.closed.Load() {
		metrics.IncCounter("store.wal.SyncGroup.errors", 1)
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.syncErr != nil {
		metrics.IncCounter("store.wal.SyncGroup.errors", 1)
		return w.syncErr
	}
	// The caller's durability watermark: every byte the writer has accepted so
	// far, which includes the caller's just-appended frames and OpCommit marker.
	// Taken under mu from the Writer's own appendedSize, so the coordinator
	// never tracks offsets independently of the file.
	target := w.appendedSize

	for {
		if w.syncErr != nil {
			metrics.IncCounter("store.wal.SyncGroup.errors", 1)
			return w.syncErr
		}
		if w.durableSize >= target {
			// A leader's fsync already covered this watermark: the follower
			// fast path. The commit is durable with no I/O of our own.
			metrics.IncCounter("store.wal.SyncGroup.coalesced", 1)
			return nil
		}
		if !w.leaderActive {
			// Become the leader for this round and fsync the whole buffered
			// suffix once.
			return w.leadGroupSyncLocked()
		}
		// A leader is flushing; wait for durableSize to advance (success) or a
		// poison to wake us (failure). groupCond's locker is &w.mu, so Wait
		// atomically releases mu while parked and re-acquires on wake.
		w.groupCond.Wait()
	}
}

// leadGroupSyncLocked performs one group flush+fsync as the elected leader.
// The caller holds w.mu, is not poisoned, and has set neither leaderActive nor
// observed durableSize past its target. It flushes the buffer and fsyncs once,
// covering its own and every concurrently-buffered committer's frames, then
// publishes the advanced durableSize and wakes the followers.
//
// The fsync runs while w.mu is RELEASED so other committers may keep appending
// into the buffer (their frames land after the flushed snapshot and are
// captured by the next round); bufio.Flush has already drained the leader's
// snapshot out of the buffer, so a concurrent Append touches only in-memory
// buffer bytes while f.Sync syncs the file — the two do not race on the file.
// leaderActive excludes a second concurrent flush of the same Writer for the
// duration. The publish (durableSize update) and the success/failure decision
// are performed under w.mu, with the just-flushed snapshot as the watermark, so
// no waiter ever observes a stale durableSize from a prior round.
func (w *Writer) leadGroupSyncLocked() error {
	w.leaderActive = true
	// Snapshot the watermark this fsync will make durable. Everything buffered
	// now will be flushed; frames appended after this point belong to the next
	// round. Captured under mu before any unlock, per the Writer-owned-watermark
	// invariant.
	flushed := w.appendedSize

	// Flush the buffer to the OS under mu so it does not race a concurrent
	// Append's Encode(w.bw, ...). After this the buffer is empty and the
	// leader's snapshot is in the file's OS page cache.
	if err := w.bw.Flush(); err != nil {
		// Clear leaderActive BEFORE poison so poison's broadcast (the single
		// wakeup) finds the flag already cleared: a Close waiter parked on
		// `for w.leaderActive` then wakes and proceeds, and every SyncGroup
		// follower wakes to the sticky syncErr.
		w.leaderActive = false
		w.poison(err)
		metrics.IncCounter("store.wal.SyncGroup.errors", 1)
		return err
	}

	// Release mu for the slow fsync so followers can append into the (now
	// empty) buffer while we wait on durable storage. The file bytes being
	// synced are already written; concurrent Appends only mutate the in-memory
	// bufio buffer, never the file, so f.Sync and those Appends do not race.
	w.mu.Unlock()
	syncErr := w.f.Sync()
	w.mu.Lock()

	w.leaderActive = false
	if syncErr != nil {
		// fsync failed: discard the entire un-synced suffix (this leader's and
		// every follower's frames and markers) and fail every member. leaderActive
		// is already cleared above, so poison's broadcast is the single, correct
		// wakeup for both the Close waiter and every SyncGroup follower.
		w.poison(syncErr)
		metrics.IncCounter("store.wal.SyncGroup.errors", 1)
		return syncErr
	}
	// Publish: the fsync made every byte up to `flushed` durable. Advance
	// durableSize to the snapshot we actually flushed (not the live
	// appendedSize, which a concurrent Append may have grown past what this
	// fsync covered) so a waiter never concludes a not-yet-synced frame is
	// durable.
	if flushed > w.durableSize {
		w.durableSize = flushed
	}
	w.syncs.Add(1)
	metrics.IncCounter("store.wal.SyncGroup.leader", 1)
	// Wake the followers: those whose watermark <= durableSize return success;
	// those appended after our snapshot re-evaluate and elect the next leader.
	w.groupCond.Broadcast()
	return nil
}

// poison marks the writer permanently failed after a commit-path
// flush or fsync error and physically discards the un-synced suffix
// of the file. Callers must hold w.mu.
//
// The truncation is the load-bearing step: after a failed fsync the
// kernel may keep or drop the dirty pages (post-"fsyncgate" both
// behaviours exist in the wild), and the flushed-but-unacknowledged
// frames — including the failed transaction's commit marker — would
// otherwise be made durable by the next successful fsync of this
// file, resurrecting a transaction whose commit was rolled back by
// the caller. Truncating to the last durably-synced size makes both
// kernel behaviours equivalent: the failed suffix can never reach a
// reader.
//
// After the truncation we issue a best-effort fsync so that the
// reduced file size — not just the data below the new EOF — is
// recorded on durable storage. Without this fsync a host crash
// between the ftruncate(2) syscall and the kernel's writeback of the
// updated inode metadata could leave the file at its pre-truncation
// length on the next mount (POSIX only guarantees that ftruncate
// modifies the in-memory inode; the metadata write is not itself
// synchronous). The fsync is issued AFTER the truncation, not before,
// which avoids any risk of resurrecting data above the new EOF: we
// are shrinking the file, so the fsync can only confirm the reduced
// inode size.
func (w *Writer) poison(err error) {
	w.syncFailed.Add(1)
	// Best-effort truncation: if the device is in distress the truncate
	// may fail, but the sticky syncErr below still fail-stops every future
	// Append/Sync on this writer so the un-synced suffix is never
	// acknowledged through this handle.
	_ = w.f.Truncate(w.durableSize)
	// Best-effort fsync after truncation: makes the reduced inode metadata
	// durable so a host crash cannot revert the file to its pre-truncation
	// size on the next mount. Non-actionable on failure — the writer is
	// already poisoned and no further data will be committed through it.
	_ = w.f.Sync()
	// Drop any buffered bytes (and bufio's own sticky error): nothing
	// further will be written through this writer.
	w.bw.Reset(w.f)
	w.appendedSize = w.durableSize
	w.syncErr = err
	// Wake every group-commit waiter so each observes the sticky syncErr and
	// fails its own commit: the un-synced suffix (every group member's frames
	// and OpCommit markers) was just discarded, so no member may believe it
	// committed. Safe even when groupCond is nil (a zero-value/never-grouped
	// Writer): the field is set in every constructor.
	if w.groupCond != nil {
		w.groupCond.Broadcast()
	}
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
	// The file is physically empty from this point on; keep the
	// durable/appended bookkeeping in lockstep even if a later step in
	// this method fails, so a subsequent sync-failure rollback never
	// truncates to a stale (pre-Truncate) size.
	w.durableSize = 0
	w.appendedSize = 0
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
//
// On a writer poisoned by an earlier Sync failure, Close skips the
// flush (which would buffer new data) but performs a second-chance
// truncation followed by a best-effort fsync. The truncation to
// durableSize discards any suffix that poison() may have failed to
// discard on a device in transient distress. The subsequent fsync
// makes the reduced inode metadata durable; it is safe to issue
// because the Truncate has already shrunk the file — the fsync can
// only confirm the reduced EOF, never resurrect data above it.
func (w *Writer) Close() error {
	defer metrics.Time("store.wal.Close")()
	if !w.closed.CompareAndSwap(false, true) {
		metrics.IncCounter("store.wal.Close.errors", 1)
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// Self-safe against an in-flight group-commit leader: leadGroupSyncLocked
	// releases w.mu across its f.Sync() and re-acquires it to publish, so a
	// Close that grabbed w.mu in that window must NOT flush+close the file while
	// the leader is mid-fsync (that would race the leader's f.Sync and could
	// make un-acknowledged frames durable). Wait until the leader finishes and
	// clears leaderActive. The store-layer quiesce (RunUnderCommitLock's inflight
	// drain) is the primary guard; this is defence-in-depth so Close is correct
	// even when called without that drain. groupCond's locker is w.mu, so Wait
	// atomically releases it while parked.
	for w.leaderActive {
		w.groupCond.Wait()
	}
	if w.syncErr != nil {
		// Second-chance discard: poison's own truncate may have failed
		// on a device in transient distress. Best-effort — the sticky
		// error is returned regardless.
		_ = w.f.Truncate(w.durableSize)
		// Best-effort fsync after the second-chance truncation: makes
		// the reduced inode metadata durable. Safe to issue here because
		// the Truncate above has already removed the failed suffix; this
		// fsync can only confirm the reduced EOF, not acknowledge it.
		_ = w.f.Sync()
		_ = w.f.Close() // best-effort: the poison error takes precedence
		if w.lockFile != nil {
			releaseLock(w.lockFile)
		}
		metrics.IncCounter("store.wal.Close.errors", 1)
		return w.syncErr
	}
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close() // best-effort: already on error path, flush err preserved
		if w.lockFile != nil {
			releaseLock(w.lockFile)
		}
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close() // best-effort: already on error path, sync err preserved
		if w.lockFile != nil {
			releaseLock(w.lockFile)
		}
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	if err := w.f.Close(); err != nil {
		if w.lockFile != nil {
			releaseLock(w.lockFile)
		}
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	if w.lockFile != nil {
		releaseLock(w.lockFile)
	}
	return nil
}
