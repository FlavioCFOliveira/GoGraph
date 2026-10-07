package wal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

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

// ErrSegmentsUnsupported is returned by [Writer.MarkCheckpoint] and
// [Writer.ReclaimSegments] on a Writer created by [OpenWith]: a single-file
// test writer has no control file and no segments to reclaim.
var ErrSegmentsUnsupported = errors.New("wal: operation requires a segmented writer (use Open or OpenFS, not OpenWith)")

// ErrDurabilityFailed marks every error a POISONED writer returns: the write-ahead
// log could not be made durable, the un-synced suffix has been discarded, and this
// writer will refuse every further append and sync.
//
// It wraps the underlying I/O error, so `errors.Is(err, ErrDurabilityFailed)`
// identifies the class and `errors.Unwrap` still reaches the cause.
//
// # Why it exists, and what it is NOT
//
// It is NOT retriable, and that is the whole point of naming it. A group commit is
// FAIL-ALL: when the leader's fsync fails, every member's frames and OpCommit
// markers are discarded together, so a transaction that did nothing wrong fails
// because another transaction's I/O failed. A caller needs to tell "MY
// transaction lost a conflict, retry it" from "the storage substrate failed,
// everything in flight is gone, and retrying will not help" (rmp #2306).
//
// Fail-all is kept rather than softened, because the alternative is to acknowledge a
// commit whose durability is unknown, which the module's ACID mandate forbids
// outright. It is also the LENIENT end of the prior art: PostgreSQL does not fail the
// transaction, it fails the PROCESS — `issue_xlog_fsync` carries the comment "PANIC
// if failed to fsync" (postgres/postgres, master, read 2026-08-04 at commit
// 69ed7fd7e9da1cff2f04af04f630287971fe99fe; src/backend/access/transam/xlog.c).
// GoGraph is a library embedded in the caller's process, so the handle dies and
// says so, which is PostgreSQL's conclusion scoped to what a library owns.
var ErrDurabilityFailed = errors.New("wal: durability failed; the un-synced suffix was discarded and this writer is poisoned")

// Segment sizes (docs/design-wal-v2.md §2.3).
const (
	// DefaultSegmentSize is the segment size target when [Options.SegmentSize]
	// is 0: 16 MiB, PostgreSQL's default wal_segment_size.
	DefaultSegmentSize int64 = 16 << 20
	// MinSegmentSize is the smallest segment size target [Options] accepts.
	MinSegmentSize int64 = 1 << 20
)

// Options configures [OpenWithOptions] and [OpenFSWithOptions]. It is a plain
// configuration value: populate it before the call and do not mutate it
// while the call is in flight; read-only, it is safe for concurrent use.
type Options struct {
	// SegmentSize is the target size of a segment in bytes: a segment at or
	// above it is rolled over at the next run boundary, so one run larger
	// than the target still lands in one segment. 0 selects
	// [DefaultSegmentSize]; a value below [MinSegmentSize] is refused.
	SegmentSize int64
	// SyncLatency, when non-nil, delays every data fsync and directory fsync
	// of the writer; a testing instrument, see [SyncLatency].
	SyncLatency *SyncLatency
}

func (o Options) segmentSize() (int64, error) {
	switch {
	case o.SegmentSize == 0:
		return DefaultSegmentSize, nil
	case o.SegmentSize < MinSegmentSize:
		return 0, fmt.Errorf("wal: segment size %d is below the minimum %d", o.SegmentSize, MinSegmentSize)
	default:
		return o.SegmentSize, nil
	}
}

// Stats is a snapshot of a [Writer]'s lifetime counters. Counters
// are monotonic; subtract two snapshots to compute deltas. Values
// are read with [sync/atomic.LoadUint64], so they may race slightly
// behind in-flight operations but never observe a torn value. The four
// counters are loaded one at a time, so a Stats is a per-field snapshot
// rather than a single atomic view across all four.
//
// The value [Writer.Stats] returns is a detached copy of plain integers, so a
// Stats is safe for concurrent reads and [Writer.Stats] is safe to call
// concurrently with [Writer.Append] and [Writer.Sync].
type Stats struct {
	Frames uint64 // total frames appended
	Bytes  uint64 // total frame bytes appended (header + payload)
	Syncs  uint64 // total successful data syncs (commit path and rollover)
	// SyncFailed counts sync rounds that failed at the flush/fsync
	// I/O layer. Calls rejected because the writer was already
	// poisoned by an earlier failure are not counted (mirroring how
	// context-cancelled calls are not counted).
	SyncFailed uint64
}

// segMeta is one retained segment as the writer tracks it.
type segMeta struct {
	no uint64
	// start is the logical position of the segment's first frame (for the
	// active segment before its first frame: the position it will start at).
	start int64
	// prevFramePos is the position of the frame preceding start, -1 for none.
	prevFramePos int64
}

// Writer appends frames to a store's write-ahead log. Callers append frames
// with [Writer.Append] / [Writer.AppendRun] and durably commit them with
// [Writer.Sync] / [Writer.SyncGroup]; group-commit is achieved by appending
// several frames before a single sync.
//
// A Writer created by [Open] or [OpenFS] writes a segmented log
// (docs/design-wal-v2.md): a control file, numbered segments under
// walPath+".d", and every frame stamped with its logical position, a link to its
// predecessor and the store id. Positions never reset; [Writer.DurableOffset]
// and the watermark [Writer.AppendRun] returns are positions. A segment at or
// above the target size is rolled over at the next run boundary, after it has
// been made durable, so no transaction spans two segments and every non-tail
// segment is fully durable before its successor receives a byte. A checkpoint
// reclaims space by unlinking whole segments ([Writer.MarkCheckpoint],
// [Writer.ReclaimSegments]) without taking the append lock.
//
// A Writer created by [OpenWith] writes the same frames into one file with no
// control file and no rollover; it is a test writer.
//
// # Concurrency
//
// Writer is safe for concurrent use by any number of goroutines. Appends,
// syncs and rollover serialise on one internal mutex; a group-commit leader
// releases it across its fsync. [Writer.MarkCheckpoint] and
// [Writer.ReclaimSegments] never take that mutex: they touch only the control
// file and segments the writer no longer appends to, under their own locks.
// The writer of an [Open]ed log may run one short-lived background goroutine
// that prepares the next segment; [Writer.Close] waits for it.
//
// # Fail-stop
//
// A Writer fail-stops on commit failure: the first flush or fsync error
// permanently poisons it. The un-synced suffix of the active segment — which
// may hold the flushed frames (including the commit marker) of the very
// transaction whose sync just failed — is physically discarded, and every
// subsequent Append/Sync returns the original error. Without the poison, a
// later transaction's successful fsync would make the failed transaction's
// frames durable even though its commit was never acknowledged: a phantom
// commit violating Atomicity and Durability. A poisoned Writer accepts only
// [Writer.Close]; the owner must discard it and re-open the WAL.
//
// # Which method answers "is this Writer healthy" — rmp #2525
//
// [Writer.Poisoned] does, and it is the only member that does. The set splits
// this way:
//
//   - They return the sticky poison error — the IDENTICAL error value that
//     [Writer.Poisoned] reports: [Writer.Append], [Writer.AppendCtx],
//     [Writer.AppendRun], [Writer.Sync], [Writer.SyncCtx], [Writer.Truncate]
//     and [Writer.Close].
//   - They can return nil WHILE the Writer is poisoned: [Writer.SyncBuffered]
//     always does, because the poison rewinds the accepted position to the
//     durable one and the already-durable fast path fires; [Writer.SyncGroup]
//     does for a watermark that was already durable before the failing round,
//     the deliberate durability-first rule of rmp #2322.
//   - They do not consult the poison: [Writer.MarkCheckpoint] and
//     [Writer.ReclaimSegments] act on durable state only, [Writer.Stats] and
//     [Writer.DurableOffset] have no error channel.
//
// After [Writer.Close] every method in the first two groups returns
// [ErrWriterClosed] instead of the sticky error, while [Writer.Poisoned] still
// reports the sticky error.
type Writer struct {
	f WALFile

	// syncErr is the sticky poison error: set under mu by the first
	// flush/fsync failure and never cleared.
	syncErr error

	// fsys is the path-based filesystem backend: [Open] installs [osWALFS],
	// [OpenFS] the caller's backend (the simulator's in-memory disk in DST).
	fsys walFS

	// lockFile is the open handle of the LOCK file whose flock(2) lifetime is
	// tied to this Writer; non-nil only for [Open]. Released by Close.
	lockFile *os.File

	bw *bufio.Writer
	// hdr is the frame-header scratch buffer appendLocked encodes into; guarded
	// by mu.
	hdr [HeaderSizeV2]byte

	// groupCond signals waiters when a sync round completes (durablePos
	// advanced) or the writer is poisoned. Its locker is &mu.
	groupCond *sync.Cond

	// dirFsync fsyncs the parent directory of its argument. Never nil after a
	// constructor runs; [OpenWithOptions] wraps it with the SyncLatency delay.
	dirFsync func(string) error

	// syncLatency, when non-nil, delays every data fsync. Set once at
	// construction and never mutated, so it is read without a lock.
	syncLatency *SyncLatency

	// path is the WAL base path (dir/wal); empty for [OpenWith].
	path string

	// segmented is false only for [OpenWith].
	segmented bool
	// asyncPrepare selects the background segment preparer ([Open]); [OpenFS]
	// prepares the next segment synchronously at rollover, which keeps the
	// simulator's disk operations deterministic.
	asyncPrepare bool
	storeID      uint64
	segSize      int64

	// Active-segment geometry, guarded by mu. A position p of the active
	// segment lives at file offset fileBase + (p - segStart).
	segNo    uint64
	segStart int64
	fileBase int64

	// durablePos is the position covered by the last successful fsync; the
	// truncation target of a poison. appendedPos is durablePos plus every frame
	// byte accepted since. Both guarded by mu.
	durablePos  int64
	appendedPos int64
	// lastFramePos is the position of the last accepted frame (-1: none), and
	// durableLastFramePos the same for the durable prefix. Guarded by mu.
	lastFramePos        int64
	durableLastFramePos int64
	// segFramesAtOpen reports that the open-time scan found segment frame
	// bytes (a frame, or corruption); set by attachSegments, read by
	// settleLegacy, never written afterwards.
	segFramesAtOpen bool

	frames     atomic.Uint64
	bytes      atomic.Uint64
	syncs      atomic.Uint64
	syncFailed atomic.Uint64

	mu     sync.Mutex
	closed atomic.Bool

	// leaderActive is true while one committer is performing the group
	// flush+fsync; guarded by mu.
	leaderActive bool

	// segMu guards segs, the retained segments oldest first, the active last.
	// Lock order: mu, then segMu.
	segMu sync.Mutex
	segs  []segMeta

	// ctlMu serialises the checkpoint side — control writes and segment
	// unlinks — and guards ctl, the control file as last written. It is never
	// taken under mu.
	ctlMu sync.Mutex
	ctl   Control

	// spareMu guards the prepared spare segment and the preparer state.
	spareMu     sync.Mutex
	spare       WALFile
	spareNo     uint64
	prepRunning bool
	prepWG      sync.WaitGroup
}

// Open opens or creates the write-ahead log at walPath (dir/wal in a store
// directory) for appending; see [OpenWithOptions].
func Open(walPath string) (*Writer, error) {
	return OpenWithOptions(walPath, Options{})
}

// OpenWithOptions opens or creates the write-ahead log at walPath for
// appending.
//
// It takes an exclusive OS lock on walPath+".lock" for the Writer's lifetime
// ([ErrWALLocked] when another process holds it). Then:
//
//   - A fresh directory gets a control file holding a new random store id, a
//     first segment, and a one-frame seal stub at walPath, so a build that
//     predates the segmented format refuses the directory instead of ignoring
//     its segments.
//   - A directory holding a legacy single-file log at walPath and no control
//     file is migrated: a control file is written with a new store id and
//     the legacy-pending flag, the first segment is created, and a seal frame
//     is appended to the legacy file and fsynced. The legacy file's history is
//     kept and replays as before; its benign torn tail is discarded first, and
//     a legacy file whose scan stops at corruption is refused.
//   - An existing segmented log is reopened: interrupted spare creations and
//     segments wholly below the oldest retained position are deleted, the
//     tail segment's benign torn tail is truncated and fsynced so new frames
//     are never appended behind junk, and appending resumes at the end of the
//     last valid frame. A tail that stops at genuine corruption is left
//     byte-for-byte intact for recovery to report.
//
// Every created file has mode 0o600. [Open] is OpenWithOptions with zero
// Options.
func OpenWithOptions(walPath string, opts Options) (*Writer, error) {
	defer metrics.Time("store.wal.Open").Stop()
	segSize, err := opts.segmentSize()
	if err != nil {
		metrics.IncCounter("store.wal.Open.errors", 1)
		return nil, err
	}
	lockFile, err := acquireLock(walPath + ".lock")
	if err != nil {
		metrics.IncCounter("store.wal.Open.errors", 1)
		return nil, err // ErrWALLocked or a wrapped OS error
	}
	dirFsync := parentDirFsync
	if lat := opts.SyncLatency; lat != nil {
		dirFsync = func(p string) error {
			lat.wait()
			return parentDirFsync(p)
		}
	}
	w, err := openSegmented(walPath, osWALFS{}, dirFsync, opts.SyncLatency, segSize, true)
	if err != nil {
		releaseLock(lockFile)
		metrics.IncCounter("store.wal.Open.errors", 1)
		return nil, fmt.Errorf("wal: open %q: %w", walPath, err)
	}
	w.lockFile = lockFile
	return w, nil
}

// openSegmented is the body shared by every segmented constructor.
func openSegmented(walPath string, fsys walFS, dirFsync func(string) error, lat *SyncLatency, segSize int64, asyncPrepare bool) (*Writer, error) {
	w := &Writer{
		fsys:                fsys,
		dirFsync:            dirFsync,
		syncLatency:         lat,
		path:                walPath,
		segmented:           true,
		asyncPrepare:        asyncPrepare,
		segSize:             segSize,
		fileBase:            segHeaderSize,
		lastFramePos:        -1,
		durableLastFramePos: -1,
	}
	w.groupCond = sync.NewCond(&w.mu)
	lfs := walLogFS{fsys: fsys}
	c, ok, err := readControl(lfs, walPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		if c, err = w.initControl(lfs); err != nil {
			return nil, err
		}
	}
	w.ctl, w.storeID = c, c.StoreID
	if err := w.attachSegments(lfs); err != nil {
		w.closeFiles()
		return nil, err
	}
	if err := w.settleLegacy(); err != nil {
		w.closeFiles()
		return nil, err
	}
	w.bw = bufio.NewWriterSize(w.f, 64*1024)
	w.startPreparer(w.segNo + 1)
	return w, nil
}

// closeFiles releases the handles a failed open acquired.
func (w *Writer) closeFiles() {
	if w.f != nil {
		_ = w.f.Close()
	}
	w.prepWG.Wait()
	if w.spare != nil {
		_ = w.spare.Close()
	}
}

// initControl creates the control file of a directory that has none: a fresh
// store, or a legacy single-file store being migrated.
func (w *Writer) initControl(lfs LogFS) (Control, error) {
	names, err := w.fsys.ReadDir(SegmentDir(w.path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Control{}, fmt.Errorf("wal: list segments: %w", err)
	}
	for _, n := range names {
		if _, isSeg := parseSegmentName(n); isSeg {
			return Control{}, ErrMissingControl
		}
	}
	migrating, err := legacyHasBytes(lfs, w.path)
	if err != nil {
		return Control{}, err
	}
	id, err := newStoreID()
	if err != nil {
		return Control{}, err
	}
	c := Control{
		StoreID:          id,
		PrevFramePosAtOR: NoFramePos,
		CreatedUnixNano:  uint64(time.Now().UnixNano()),
	}
	if migrating {
		c.Flags = ControlLegacyV1Pending
		metrics.IncCounter("store.wal.migrate.started", 1)
	}
	b := encodeControl(c)
	if err := writeFileDurably(w.fsys, w.dirFsync, ControlPath(w.path), b[:],
		"wal.control.tmp-written-pre-rename", "wal.control.renamed-pre-dirfsync"); err != nil {
		return Control{}, err
	}
	return c, nil
}

// legacyHasBytes reports whether the legacy single-file log exists and is
// non-empty.
func legacyHasBytes(lfs LogFS, walPath string) (bool, error) {
	rc, err := lfs.Open(walPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("wal: probe legacy log: %w", err)
	}
	defer func() { _ = rc.Close() }()
	var b [1]byte
	n, _ := rc.Read(b[:])
	return n > 0, nil
}

// attachSegments enumerates the segments, deletes interrupted spares and
// leftovers below OR, establishes the end of the log from a full validating
// scan, truncates a benign torn tail, and opens the active segment.
func (w *Writer) attachSegments(lfs LogFS) error {
	if err := w.fsys.MkdirAll(SegmentDir(w.path)); err != nil {
		return fmt.Errorf("wal: create segment directory: %w", err)
	}
	if err := w.dirFsync(SegmentDir(w.path)); err != nil {
		return fmt.Errorf("wal: fsync parent of segment directory: %w", err)
	}
	log, err := OpenLogFS(lfs, w.path)
	if err != nil {
		return err
	}
	list := log.list
	removed := false
	for _, p := range append(list.unfinished, list.leftovers...) {
		if err := w.fsys.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("wal: remove stale segment %q: %w", p, err)
		}
		removed = true
	}
	retained := list.segs[list.start:]
	if len(list.segs) == 0 {
		retained = nil
	}
	for range log.Frames() {
	}
	tailErr := log.TailError()
	end := log.TailOffset()
	w.appendedPos, w.durablePos = end, end
	w.lastFramePos = log.LastFramePos()
	w.durableLastFramePos = w.lastFramePos
	w.segFramesAtOpen = log.frames > 0 || (tailErr != nil && !errors.Is(tailErr, ErrTornFrame))

	// The active segment is the last one holding a frame byte; with none, the
	// first retained segment (a fresh store, or one whose retained segments are
	// all empty), created when there is none.
	active := -1
	for i, s := range retained {
		if s.firstPos >= 0 || s.partial {
			active = i
		}
	}
	if active < 0 && len(retained) > 0 {
		active = 0
	}
	// Trailing empty segments past active+1 are surplus spares: delete them,
	// and adopt active+1 as the spare.
	for i := active + 2; i < len(retained); i++ {
		if err := w.fsys.Remove(retained[i].path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("wal: remove surplus segment %q: %w", retained[i].path, err)
		}
		removed = true
	}
	if removed {
		if err := w.dirFsync(SegmentPath(w.path, 0)); err != nil {
			return fmt.Errorf("wal: fsync segment directory: %w", err)
		}
	}
	if active < 0 {
		f, err := w.createSegment(1)
		if err != nil {
			return err
		}
		w.f, w.segNo, w.segStart = f, 1, end
		w.segs = []segMeta{{no: 1, start: end, prevFramePos: w.lastFramePos}}
		return nil
	}
	act := retained[active]
	if errors.Is(tailErr, ErrTornFrame) {
		// Discard the benign torn tail so new frames are never appended
		// behind junk every reader stops at. The torn bytes were never part of
		// an acknowledged commit, so discarding them loses nothing.
		keep := int64(segHeaderSize)
		if no, off := log.TailSegment(); no == act.no {
			keep = off
		}
		if err := truncateDurably(w.fsys, act.path, keep); err != nil {
			return fmt.Errorf("wal: discard torn tail of segment %d: %w", act.no, err)
		}
		metrics.IncCounter("store.wal.Open.tornTailDiscarded", 1)
	}
	f, err := w.fsys.OpenFile(act.path, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return fmt.Errorf("wal: open segment %d: %w", act.no, err)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: probe segment %d: %w", act.no, err)
	}
	w.f, w.segNo = f, act.no
	if act.firstPos >= 0 {
		w.segStart = act.firstPos
	} else {
		w.segStart = end
	}
	// A tail that stopped at corruption is left intact; appends then land at
	// the physical end with positions recovery never reaches, exactly as the
	// single-file writer appended after a corrupt frame.
	w.fileBase = size - (end - w.segStart)
	w.segs = make([]segMeta, 0, active+1)
	for i := 0; i <= active; i++ {
		s := retained[i]
		m := segMeta{no: s.no, start: s.firstPos, prevFramePos: -1}
		if s.firstPos >= 0 && s.firstPrevLen != 0 {
			m.prevFramePos = s.firstPos - int64(s.firstPrevLen)
		}
		if i == active && s.firstPos < 0 {
			m.start, m.prevFramePos = w.segStart, w.lastFramePos
		}
		w.segs = append(w.segs, m)
	}
	if active+1 < len(retained) {
		sf, err := w.fsys.OpenFile(retained[active+1].path, os.O_RDWR|os.O_APPEND)
		if err != nil {
			return fmt.Errorf("wal: open spare segment: %w", err)
		}
		if _, err := sf.Seek(0, io.SeekEnd); err != nil {
			_ = sf.Close()
			return fmt.Errorf("wal: probe spare segment: %w", err)
		}
		w.spare, w.spareNo = sf, retained[active+1].no
	}
	return nil
}

// truncateDurably truncates the file at path to size and fsyncs it.
func truncateDurably(fsys walFS, path string, size int64) error {
	f, err := fsys.OpenFile(path, os.O_RDWR)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// settleLegacy completes the legacy single-file log's part of the layout: a
// pending migration seals it, and a store with no legacy history gets the seal
// stub that makes an older build refuse the directory.
//
// A store that is not migrating but whose legacy file has bytes while the
// segments hold no frame has its legacy file sealed too, unless it already
// ends in a seal: a crash between initControl and the stub write leaves no
// legacy file, an older build may then start a v1 log there, and segment
// frames appended after it would make recovery refuse with
// [ErrLegacyNotSealed].
func (w *Writer) settleLegacy() error {
	seal := LegacySeal{StoreID: w.storeID}
	migrating := w.ctl.Flags&ControlLegacyV1Pending != 0
	if !migrating {
		has, err := legacyHasBytes(walLogFS{fsys: w.fsys}, w.path)
		if err != nil {
			return err
		}
		if !has {
			return writeFileDurably(w.fsys, w.dirFsync, w.path, sealFrameBytes(seal), "", "")
		}
		if w.segFramesAtOpen {
			return nil
		}
	}
	// Read through a read-only handle: the position of an O_APPEND handle is
	// not guaranteed to start at 0 on every walFS (the simulator's starts at
	// the end), and the frames must be read from the first byte.
	// An absent file reads as empty and is created by the seal below.
	r := NewReader(eofReader{}, nil)
	switch rc, err := (walLogFS{fsys: w.fsys}).Open(w.path); {
	case err == nil:
		r = NewReader(rc, rc)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("wal: open legacy log: %w", err)
	}
	sealed := false
	for fr := range r.Frames() {
		_, sealed = DecodeLegacySeal(fr.Payload)
		if migrating && sealed && fr.StoreID != w.storeID {
			_ = r.Close()
			return fmt.Errorf("%w: legacy seal names store %016x", ErrForeignStore, fr.StoreID)
		}
	}
	_ = r.Close()
	if tErr := r.TailError(); tErr != nil && !errors.Is(tErr, ErrTornFrame) {
		if !migrating {
			// Not a migration: the file is left as found, and recovery
			// reports the corruption whenever it reads the legacy history.
			return nil
		}
		return fmt.Errorf("wal: legacy log is corrupt, refusing to migrate: %w", tErr)
	}
	if sealed {
		return nil
	}
	f, err := w.fsys.OpenFile(w.path, os.O_RDWR|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return fmt.Errorf("wal: open legacy log: %w", err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if tail := r.TailOffset(); tail < size {
		if err := f.Truncate(tail); err != nil {
			return fmt.Errorf("wal: discard torn tail of legacy log: %w", err)
		}
	}
	crashpoint.Breakpoint("wal.migrate.control-written-pre-seal")
	if _, err := f.Write(sealFrameBytes(seal)); err != nil {
		return fmt.Errorf("wal: seal legacy log: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("wal: fsync legacy seal: %w", err)
	}
	metrics.IncCounter("store.wal.migrate.sealed", 1)
	crashpoint.Breakpoint("wal.migrate.sealed-pre-first-v2-frame")
	return nil
}

// eofReader is an empty input.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// createSegment creates segment no durably: exclusive create, header, fsync,
// directory fsync. A crash part-way leaves a short or invalid trailing
// segment, which readers ignore and the next open deletes.
func (w *Writer) createSegment(no uint64) (WALFile, error) {
	p := SegmentPath(w.path, no)
	f, err := w.fsys.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_APPEND)
	if err != nil {
		return nil, fmt.Errorf("wal: create segment %d: %w", no, err)
	}
	hdr := encodeSegmentHeader(w.storeID, no)
	if _, err := f.Write(hdr[:]); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: write segment %d header: %w", no, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: fsync segment %d: %w", no, err)
	}
	crashpoint.Breakpoint("wal.segment.spare-created-pre-dirfsync")
	if err := w.dirFsync(p); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: fsync segment directory: %w", err)
	}
	metrics.IncCounter("store.wal.segment.created", 1)
	return f, nil
}

// startPreparer starts the background preparer for segment no when the writer
// prepares asynchronously and no spare exists or is being prepared. The
// preparer is short-lived: it creates one spare and exits; [Writer.Close]
// waits for it.
func (w *Writer) startPreparer(no uint64) {
	if !w.asyncPrepare || w.closed.Load() {
		return
	}
	w.spareMu.Lock()
	if w.prepRunning || w.spare != nil {
		w.spareMu.Unlock()
		return
	}
	w.prepRunning = true
	w.prepWG.Add(1)
	w.spareMu.Unlock()
	go pprof.Do(context.Background(), pprof.Labels("gograph", "wal-segment-preparer"), func(context.Context) {
		defer w.prepWG.Done()
		f, err := w.createSegment(no)
		w.spareMu.Lock()
		defer w.spareMu.Unlock()
		w.prepRunning = false
		if err != nil {
			// The rollover slow path creates the segment synchronously and
			// poisons the writer if that fails too.
			metrics.IncCounter("store.wal.segment.prepareErrors", 1)
			return
		}
		w.spare, w.spareNo = f, no
	})
}

// takeSpare returns segment no ready for appending: the prepared spare, or one
// created synchronously (the slow path). The caller holds mu; the preparer
// never takes mu, so waiting for it here cannot deadlock.
func (w *Writer) takeSpare(no uint64) (WALFile, error) {
	w.prepWG.Wait()
	w.spareMu.Lock()
	f, fno := w.spare, w.spareNo
	w.spare = nil
	w.spareMu.Unlock()
	if f != nil && fno == no {
		return f, nil
	}
	if f != nil {
		_ = f.Close()
	}
	if w.asyncPrepare {
		metrics.IncCounter("store.wal.segment.syncPrepared", 1)
	}
	return w.createSegment(no)
}

// OpenWith builds a single-file test [Writer] over an already-open file handle.
// The caller transfers ownership: [Writer.Close] will call f.Close().
//
// The Writer appends [CurrentVersion] frames with store id 0 after the file's
// existing bytes, never rolls over and has no control file; positions equal
// file offsets. It exists for tests that inject a *testfs.FaultFile;
// production code uses [Open].
func OpenWith(f WALFile) (*Writer, error) {
	if f == nil {
		return nil, fmt.Errorf("wal: OpenWith: nil file")
	}
	// Seek to the end so Append is truly append-only even when the
	// caller opened the file without O_APPEND. The resulting position
	// doubles as the durable baseline.
	pos, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: OpenWith: seek to end: %w", err)
	}
	w := &Writer{
		f:                   f,
		fsys:                osWALFS{},
		dirFsync:            parentDirFsync,
		bw:                  bufio.NewWriterSize(f, 64*1024),
		segStart:            pos,
		fileBase:            pos,
		durablePos:          pos,
		appendedPos:         pos,
		lastFramePos:        -1,
		durableLastFramePos: -1,
	}
	w.groupCond = sync.NewCond(&w.mu)
	return w, nil
}

// StoreID returns the store id stamped on every frame, or 0 for an [OpenWith]
// writer. Safe for concurrent use.
func (w *Writer) StoreID() uint64 { return w.storeID }

// Append writes one frame with the given opaque payload. The frame is
// buffered in process memory; call [Writer.Sync] to durably commit.
//
// On a writer poisoned by an earlier Sync failure, Append rejects
// the frame and returns the original sync error; see the [Writer]
// type documentation.
func (w *Writer) Append(payload []byte) error {
	defer metrics.Time("store.wal.Append").Stop()
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
	defer metrics.Time("store.wal.AppendCtx").Stop()
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
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
		return w.syncErr
	}
	if err := w.maybeRolloverLocked(); err != nil {
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
		return err
	}
	if err := w.appendLocked(payload); err != nil {
		metrics.IncCounter("store.wal.AppendCtx.errors", 1)
		return err
	}
	return nil
}

// AppendRun appends every frame fn emits as ONE CONTIGUOUS RUN: no other
// appender's frame can land between them, and a run never spans two segments.
//
// # Why this exists — rmp #2302, audit finding E5
//
// Crash recovery commits the ops carrying a marker's own TxnSeq and discards the
// buffered prefix as orphaned, and that reading is correct ONLY IF a
// transaction's frames are contiguous. Contiguity therefore lives in the
// component that owns the log.
//
// # The lock this holds
//
// w.mu is taken once, before fn, and released after it — so fn runs with the
// writer exclusively held. Group commit is unaffected: [Writer.SyncGroup]
// coalesces on sync, not on append. Rollover happens at the start of the run,
// before fn, so the run lands whole in one segment.
//
// # Contract
//
// The emit closure handed to fn is valid ONLY for the duration of the call;
// retaining it and calling it later panics. fn MUST NOT call any other method
// on this Writer — w.mu is not re-entrant and doing so deadlocks.
//
// An error from fn is returned unchanged, and frames already emitted stay in the
// buffer: they are an un-marked, incomplete transaction, which recovery discards
// for atomicity. An error from append itself is the same fail-stop as
// [Writer.AppendCtx]'s.
//
// Prior art: PostgreSQL's XLogInsertRecord does the expensive work (assembling
// and CRCing the record) outside its insertion lock and holds it only for the
// copy (postgres/postgres, master, src/backend/access/transam/xlog.c).
//
// # Return value — the run's own durability watermark
//
// AppendRun returns the log position immediately after the run's last frame.
// That position is the run's OWN watermark, and it is what the caller must hand
// to [Writer.SyncGroup] to make this run durable. The caller cannot derive it
// afterwards: the accepted position is shared mutable state that another
// appender advances and a poison REWINDS (rmp #2322).
func (w *Writer) AppendRun(fn func(emit func([]byte) error) error) (int64, error) {
	defer metrics.Time("store.wal.AppendRun").Stop()
	if w.closed.Load() {
		metrics.IncCounter("store.wal.AppendRun.errors", 1)
		return 0, ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.syncErr != nil {
		metrics.IncCounter("store.wal.AppendRun.errors", 1)
		return 0, w.syncErr
	}
	if err := w.maybeRolloverLocked(); err != nil {
		metrics.IncCounter("store.wal.AppendRun.errors", 1)
		return w.appendedPos, err
	}
	// done guards the closure against use after fn returns.
	done := false
	err := fn(func(payload []byte) error {
		if done {
			panic("wal: AppendRun's emit closure used after the run returned")
		}
		if aerr := w.appendLocked(payload); aerr != nil {
			return aerr
		}
		// Crash-injection point: MID-APPEND of one transaction's run, with w.mu
		// held and every other writer queued behind it. Elided to nothing in
		// every build without the gograph_crashinject tag.
		crashpoint.Breakpoint("wal.appendrun.frame-emitted")
		return nil
	})
	done = true
	if err != nil {
		metrics.IncCounter("store.wal.AppendRun.errors", 1)
	}
	// Read under w.mu, so the watermark is the one this run produced. Returned
	// even on error so the caller can still reach a poisoned writer's sticky
	// error through SyncGroup.
	return w.appendedPos, err
}

// appendLocked encodes one frame at the current position into the buffer. The
// caller holds w.mu and has checked closed/syncErr. It is the body
// [Writer.AppendCtx] and [Writer.AppendRun] share.
func (w *Writer) appendLocked(payload []byte) error {
	if len(payload) > maxFrameSize {
		// A frame this large could be written but never read back (rmp #2742).
		metrics.IncCounter("store.wal.Encode.errors", 1)
		return ErrFrameTooLarge
	}
	pos := w.appendedPos
	var prevLen uint32
	if w.lastFramePos >= 0 {
		//nolint:gosec // G115: the distance is one frame, at most HeaderSizeV2 + maxFrameSize (< 2^31)
		prevLen = uint32(pos - w.lastFramePos)
	}
	// The header is built in the writer's own buffer, guarded by w.mu, so a
	// frame costs no allocation; bufio copies it before writeFrame returns.
	//nolint:gosec // G115: bounded by the maxFrameSize check above; positions are non-negative
	putHeaderV2(&w.hdr, uint32(len(payload)), uint64(pos), prevLen, w.storeID, crc32.Update(0, castagnoli, payload))
	n, err := writeFrame(w.bw, w.hdr[:], payload)
	if err != nil {
		// A partial frame may now sit in the buffer; bufio's sticky error
		// guarantees the next Flush fails, so the next sync poisons the writer
		// and discards the partial bytes before anything acknowledges them.
		return err
	}
	w.lastFramePos = pos
	w.appendedPos += int64(n)
	w.frames.Add(1)
	//nolint:gosec // G115: io.Writer forbids a negative n
	w.bytes.Add(uint64(n))
	return nil
}

// maybeRolloverLocked rolls the active segment over when it is at or above the
// target size. The caller holds w.mu and has checked closed/syncErr.
func (w *Writer) maybeRolloverLocked() error {
	if !w.segmented || w.fileBase+(w.appendedPos-w.segStart) < w.segSize {
		return nil
	}
	return w.rolloverLocked()
}

// rolloverLocked makes the active segment fully durable and switches to the
// next one (docs/design-wal-v2.md §2.3). The caller holds w.mu.
//
// Invariant: every non-tail segment is fully durable before any byte lands in
// its successor, so a torn frame in a non-tail segment is corruption. Leaving
// the old segment's fsync to the next group leader was rejected: it is
// indistinguishable from external truncation of an acknowledged segment.
func (w *Writer) rolloverLocked() error {
	// Wait out an in-flight group leader, which fsyncs w.f with mu released.
	for w.leaderActive {
		w.groupCond.Wait()
	}
	if w.syncErr != nil {
		return w.syncErr
	}
	if err := w.bw.Flush(); err != nil {
		w.poison(err)
		return w.syncErr
	}
	crashpoint.Breakpoint("wal.rollover.old-flushed-pre-fsync")
	if err := w.dataSyncFile(); err != nil {
		w.poison(err)
		return w.syncErr
	}
	w.durablePos, w.durableLastFramePos = w.appendedPos, w.lastFramePos
	w.syncs.Add(1)
	w.groupCond.Broadcast()
	next, err := w.takeSpare(w.segNo + 1)
	if err != nil {
		// The old segment is already durable, so the poison loses nothing.
		w.poison(err)
		return w.syncErr
	}
	old := w.f
	w.f = next
	w.bw.Reset(next)
	_ = old.Close() // best-effort: every byte of it is durable
	w.segNo++
	w.segStart, w.fileBase = w.appendedPos, segHeaderSize
	w.segMu.Lock()
	w.segs = append(w.segs, segMeta{no: w.segNo, start: w.appendedPos, prevFramePos: w.lastFramePos})
	w.segMu.Unlock()
	metrics.IncCounter("store.wal.segment.rollover", 1)
	crashpoint.Breakpoint("wal.rollover.switched-pre-first-frame")
	w.startPreparer(w.segNo + 1)
	return nil
}

// Sync flushes the buffered frames to the OS and then issues the
// per-commit data sync (fdatasync(2) on Linux, [os.File.Sync] / fsync
// elsewhere; see dataSync) so the appended frames and the grown file
// size reach durable storage before returning.
//
// The first flush or fsync failure permanently poisons the writer; see the
// [Writer] type documentation.
func (w *Writer) Sync() error {
	defer metrics.Time("store.wal.Sync").Stop()
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
	defer metrics.Time("store.wal.SyncCtx").Stop()
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
	// A direct sync must not race a group leader's fsync of the same file.
	for w.leaderActive {
		w.groupCond.Wait()
	}
	if w.syncErr != nil {
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return w.syncErr
	}
	if err := w.bw.Flush(); err != nil {
		w.poison(err)
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		// The wrapped CLASS, not the bare cause (rmp #2306).
		return w.syncErr
	}
	if err := w.dataSyncFile(); err != nil {
		w.poison(err)
		metrics.IncCounter("store.wal.SyncCtx.errors", 1)
		return w.syncErr
	}
	w.durablePos, w.durableLastFramePos = w.appendedPos, w.lastFramePos
	w.syncs.Add(1)
	// Wake any group-commit waiter: a direct Sync advances durablePos past
	// their watermark, so they are durable without electing their own leader.
	w.groupCond.Broadcast()
	return nil
}

// SyncGroup durably commits the caller's already-appended frames, coalescing
// the fsync with those of every other committer whose frames are buffered at
// the same time — PostgreSQL-XLogFlush-style group commit. It returns nil only
// after a data sync has made durable every byte up to and including the
// caller's last appended frame (its OpCommit marker).
//
// # Contract
//
// SyncGroup must be called AFTER the caller has appended all of its frames, with
// target set to the watermark [Writer.AppendRun] returned for that run. Then:
//
//   - If a previous sync has already advanced the durable position to target,
//     it returns nil without any I/O — the follower fast path.
//   - Otherwise, if the writer is poisoned, it returns the sticky error.
//   - Otherwise, if no leader is flushing, the caller becomes the LEADER: it
//     flushes the buffer and fsyncs once, covering every buffered committer's
//     frames, publishes the new durable position, and wakes the followers. If
//     a leader is already flushing, the caller waits until the durable
//     position covers its watermark or the writer poisons.
//
// A run never spans segments and a rollover makes the old segment durable
// before switching, so the one fsync of the active segment covers every
// buffered frame.
//
// # Failure semantics
//
// FAIL-ALL: if the leader's flush or fsync fails, [Writer.poison] discards the
// entire un-synced suffix (every group member's frames and markers) and
// broadcasts; every waiter then observes the sticky error and fails its own
// commit.
//
// # Cancellation
//
// SyncGroup is intentionally NOT context-aware. Once a committer's frames are
// in the shared buffer they cannot be un-appended, so abandoning the wait while
// the group still fsyncs them would make the transaction durable while
// returning an error. This matches PostgreSQL: a backend cannot un-write WAL it
// has already inserted.
//
// Concurrency: safe for concurrent calls; it serialises on the same internal
// mutex as Append/Sync and guarantees a single leader per fsync round.
func (w *Writer) SyncGroup(target int64) error {
	defer metrics.Time("store.wal.SyncGroup").Stop()
	if w.closed.Load() {
		metrics.IncCounter("store.wal.SyncGroup.errors", 1)
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncToLocked(target)
}

// SyncBuffered makes durable everything the writer has accepted at the moment of
// the call, coalescing with any concurrent group round exactly as
// [Writer.SyncGroup] does.
//
// It is a FLUSH, not a commit acknowledgement: the accepted position is shared
// mutable state, so a committer must use [Writer.AppendRun]'s watermark with
// [Writer.SyncGroup] to learn whether its own frames are durable (rmp #2322).
//
// # A nil return does NOT mean the Writer is healthy — rmp #2525
//
// On a POISONED Writer SyncBuffered returns nil: the poison rewinds the
// accepted position to the durable one, so the already-durable fast path fires
// before the sticky error is tested. [Writer.Poisoned] is the health probe.
// After [Writer.Close] SyncBuffered returns [ErrWriterClosed].
func (w *Writer) SyncBuffered() error {
	defer metrics.Time("store.wal.SyncBuffered").Stop()
	if w.closed.Load() {
		metrics.IncCounter("store.wal.SyncBuffered.errors", 1)
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncToLocked(w.appendedPos)
}

// syncToLocked is the group-commit wait shared by [Writer.SyncGroup] and
// [Writer.SyncBuffered]. The caller holds w.mu and has checked closed.
//
// # Why durability is tested BEFORE the poison — rmp #2322
//
// A committer's frames can be durable AND the writer poisoned, because the
// poison belongs to a LATER round that failed after a leader had already
// fsynced this caller's marker. Testing the poison first failed such a
// committer while recovery replayed its transaction. Testing durability first
// cannot make the opposite error: target is the caller's OWN watermark and the
// durable position only advances through a completed fsync.
func (w *Writer) syncToLocked(target int64) error {
	for {
		if w.durablePos >= target {
			metrics.IncCounter("store.wal.SyncGroup.coalesced", 1)
			return nil
		}
		if w.syncErr != nil {
			metrics.IncCounter("store.wal.SyncGroup.errors", 1)
			return w.syncErr
		}
		if !w.leaderActive {
			return w.leadGroupSyncLocked()
		}
		w.groupCond.Wait()
	}
}

// leadGroupSyncLocked performs one group flush+fsync as the elected leader.
// The caller holds w.mu. The fsync runs while w.mu is RELEASED so other
// committers may keep appending into the buffer (their frames are captured by
// the next round); leaderActive excludes a second concurrent flush and a
// rollover for the duration. The publish runs under w.mu with the
// just-flushed snapshot as the watermark.
func (w *Writer) leadGroupSyncLocked() error {
	w.leaderActive = true
	flushed, flushedLast := w.appendedPos, w.lastFramePos
	if err := w.bw.Flush(); err != nil {
		// Clear leaderActive BEFORE poison so poison's broadcast (the single
		// wakeup) finds the flag already cleared.
		w.leaderActive = false
		w.poison(err)
		metrics.IncCounter("store.wal.SyncGroup.errors", 1)
		return w.syncErr
	}
	w.mu.Unlock()
	// Crash-injection point: MID-FSYNC of a group commit. The leader's snapshot
	// is flushed to the OS but nothing below it is durable yet. Elided to
	// nothing in every build without the gograph_crashinject tag.
	crashpoint.Breakpoint("wal.sync.pre-datasync")
	syncErr := w.dataSyncFile()
	w.mu.Lock()

	w.leaderActive = false
	if syncErr != nil {
		w.poison(syncErr)
		metrics.IncCounter("store.wal.SyncGroup.errors", 1)
		return w.syncErr
	}
	// Publish the snapshot actually flushed, never the live appendedPos a
	// concurrent append may have grown past what this fsync covered.
	if flushed > w.durablePos {
		w.durablePos, w.durableLastFramePos = flushed, flushedLast
	}
	w.syncs.Add(1)
	metrics.IncCounter("store.wal.SyncGroup.leader", 1)
	w.groupCond.Broadcast()
	return nil
}

// activeOffset returns the file offset of position p in the active segment.
func (w *Writer) activeOffset(p int64) int64 { return w.fileBase + (p - w.segStart) }

// poison marks the writer permanently failed after a commit-path flush or fsync
// error and physically discards the un-synced suffix of the active segment.
// Callers must hold w.mu.
//
// The truncation is the load-bearing step: after a failed fsync the kernel may
// keep or drop the dirty pages (post-"fsyncgate" both behaviours exist), and
// the flushed-but-unacknowledged frames would otherwise be made durable by the
// next successful fsync of this file. Truncating to the durable position makes
// both behaviours equivalent; the best-effort fsync after it makes the reduced
// size durable and can only confirm the reduced end. A rollover makes the old
// segment fully durable before switching, so the un-synced suffix always lies
// in the active segment.
func (w *Writer) poison(err error) {
	w.syncFailed.Add(1)
	_ = w.f.Truncate(w.activeOffset(w.durablePos))
	_ = w.f.Sync()
	w.bw.Reset(w.f)
	w.appendedPos, w.lastFramePos = w.durablePos, w.durableLastFramePos
	// Wrapped in [ErrDurabilityFailed] so every caller can identify the class.
	w.syncErr = fmt.Errorf("%w: %w", ErrDurabilityFailed, err)
	// Wake every group-commit waiter so each observes the sticky syncErr.
	w.groupCond.Broadcast()
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

// DurableOffset returns the log position covered by the last successful fsync:
// the end of every frame durably committed so far. It always lands on a frame
// boundary, because a transaction commits only after its marker has been
// appended and fsynced.
//
// It is the redo position a non-blocking checkpoint captures under the store's
// quiesce boundary ([txn.Store.RunUnderCommitLock], which drains in-flight group
// commits), where it equals the accepted position: exactly the prefix the
// checkpoint's snapshot folds.
//
// Concurrency: safe for concurrent use; it reads under the internal mutex.
func (w *Writer) DurableOffset() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.durablePos
}

// Poisoned reports the writer's fail-stop state: it returns the sticky
// commit-failure error when a prior flush or fsync has permanently poisoned the
// writer, and nil while the writer is healthy. It is the Writer's ONLY health
// probe (rmp #2525) and remains legible after [Writer.Close].
//
// It is the WAL-health probe a non-blocking checkpoint consults under the
// store's quiesce boundary BEFORE it captures and publishes a snapshot
// (rmp #1919): a writer observed poisoned there means a schema DDL whose commit
// failed may still be reflected in the engine's registry.
//
// Concurrency: safe for concurrent use; it reads syncErr under the internal
// mutex.
func (w *Writer) Poisoned() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncErr
}

// MarkCheckpoint records in the control file that the log may from now on
// begin at the segment holding redoPos: it sets [ControlPrefixTruncated], the
// oldest retained position OR to the first position of the segment containing
// redoPos, the prev-frame position at OR, and the checkpoint redo position. It
// is phase 2 of a truncating checkpoint, called after the snapshot covering
// redoPos is published and read back, and before [Writer.ReclaimSegments]
// unlinks anything: PostgreSQL's UpdateControlFile before RemoveOldXlogFiles.
//
// redoPos must be a value [Writer.DurableOffset] returned. The control file is
// written temp, fsync, rename, directory fsync, so a crash leaves the old or
// the new one, and both are consistent with the segments on disk.
//
// It never takes the append lock and never stalls a commit. Safe for concurrent
// use; control writes serialise among themselves.
func (w *Writer) MarkCheckpoint(redoPos int64) error {
	if !w.segmented {
		return ErrSegmentsUnsupported
	}
	if w.closed.Load() {
		return ErrWriterClosed
	}
	w.mu.Lock()
	durable := w.durablePos
	w.mu.Unlock()
	if redoPos < 0 || redoPos > durable {
		return fmt.Errorf("wal: MarkCheckpoint: redo position %d out of range [0, %d]", redoPos, durable)
	}
	w.ctlMu.Lock()
	defer w.ctlMu.Unlock()
	or, prev := w.segmentStartAt(redoPos)
	c := w.ctl
	if uint64(or) < c.OldestRetainedPos { //nolint:gosec // G115: positions are non-negative
		or, prev = int64(c.OldestRetainedPos), int64(c.PrevFramePosAtOR) //nolint:gosec // G115: positions are bounded
		if c.PrevFramePosAtOR == NoFramePos {
			prev = -1
		}
	}
	c.Flags |= ControlPrefixTruncated
	c.OldestRetainedPos = uint64(or) //nolint:gosec // G115: positions are non-negative
	c.PrevFramePosAtOR = framePosToCtl(prev)
	c.CheckpointRedoPos = uint64(redoPos)
	b := encodeControl(c)
	if err := writeFileDurably(w.fsys, w.dirFsync, ControlPath(w.path), b[:],
		"checkpoint.control-tmp-pre-rename", "checkpoint.control-renamed-pre-dirfsync"); err != nil {
		metrics.IncCounter("store.wal.MarkCheckpoint.errors", 1)
		return err
	}
	w.ctl = c
	return nil
}

func framePosToCtl(p int64) uint64 {
	if p < 0 {
		return NoFramePos
	}
	return uint64(p)
}

// segmentStartAt returns the first position of the last retained segment that
// starts at or below pos, and the position of the frame preceding it.
func (w *Writer) segmentStartAt(pos int64) (start, prevFramePos int64) {
	w.segMu.Lock()
	defer w.segMu.Unlock()
	start, prevFramePos = w.segs[0].start, w.segs[0].prevFramePos
	for _, s := range w.segs {
		if s.start <= pos {
			start, prevFramePos = s.start, s.prevFramePos
		}
	}
	return start, prevFramePos
}

// ReclaimSegments unlinks every segment whose frames all lie below the oldest
// retained position the control file records (never the active segment),
// oldest first, and fsyncs the segment directory. When the control file still
// marks the legacy single-file log as pending, it then replaces that file with
// the one-frame seal stub and clears the flag. It returns the number of log
// bytes (positions) reclaimed.
//
// It is phase 3 of a truncating checkpoint and needs NO commit lock: the writer
// never touches a non-active segment, and every frame it removes lies below
// the redo position the published snapshot covers. It never takes the append
// lock. Safe for concurrent use; it serialises with [Writer.MarkCheckpoint].
func (w *Writer) ReclaimSegments() (int64, error) {
	if !w.segmented {
		return 0, ErrSegmentsUnsupported
	}
	if w.closed.Load() {
		return 0, ErrWriterClosed
	}
	w.ctlMu.Lock()
	defer w.ctlMu.Unlock()
	or := int64(w.ctl.OldestRetainedPos) //nolint:gosec // G115: positions are bounded
	w.segMu.Lock()
	var victims []segMeta
	for i := 0; i+1 < len(w.segs) && w.segs[i+1].start <= or; i++ {
		victims = append(victims, w.segs[i])
	}
	w.segMu.Unlock()
	var reclaimed int64
	if len(victims) > 0 {
		for i, s := range victims {
			if err := w.fsys.Remove(SegmentPath(w.path, s.no)); err != nil && !errors.Is(err, os.ErrNotExist) {
				metrics.IncCounter("store.wal.ReclaimSegments.errors", 1)
				return reclaimed, fmt.Errorf("wal: unlink segment %d: %w", s.no, err)
			}
			if i == 0 {
				crashpoint.Breakpoint("checkpoint.unlink-partial")
			}
		}
		crashpoint.Breakpoint("checkpoint.unlink-done-pre-dirfsync")
		if err := w.dirFsync(SegmentPath(w.path, victims[0].no)); err != nil {
			metrics.IncCounter("store.wal.ReclaimSegments.errors", 1)
			return reclaimed, fmt.Errorf("wal: fsync segment directory: %w", err)
		}
		w.segMu.Lock()
		w.segs = w.segs[len(victims):]
		reclaimed = w.segs[0].start - victims[0].start
		w.segMu.Unlock()
		metrics.IncCounter("store.wal.ReclaimSegments.segments", uint64(len(victims)))
	}
	if w.ctl.Flags&ControlLegacyV1Pending != 0 {
		if err := writeFileDurably(w.fsys, w.dirFsync, w.path, sealFrameBytes(LegacySeal{StoreID: w.storeID}),
			"", "checkpoint.legacy-stub-renamed-pre-dirfsync"); err != nil {
			return reclaimed, err
		}
		c := w.ctl
		c.Flags &^= ControlLegacyV1Pending
		b := encodeControl(c)
		if err := writeFileDurably(w.fsys, w.dirFsync, ControlPath(w.path), b[:], "", ""); err != nil {
			return reclaimed, err
		}
		w.ctl = c
		metrics.IncCounter("store.wal.migrate.legacyStubbed", 1)
	}
	return reclaimed, nil
}

// Truncate discards every frame written so far. It is a test and maintenance
// helper, not a checkpoint path; the caller must already hold a snapshot that
// covers every frame.
//
// On a segmented writer it rolls the active segment over (making it durable),
// writes the control file with [ControlPrefixTruncated] and OR at the current
// position, and unlinks every older segment. On an [OpenWith] writer it empties
// the file. It returns the number of log bytes discarded.
//
// A poisoned or closed writer returns its sticky error or [ErrWriterClosed]
// and touches nothing.
func (w *Writer) Truncate() (int64, error) {
	defer metrics.Time("store.wal.Truncate").Stop()
	if w.closed.Load() {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return 0, ErrWriterClosed
	}
	if !w.segmented {
		return w.truncateSingleFile()
	}
	w.mu.Lock()
	if w.syncErr != nil {
		err := w.syncErr
		w.mu.Unlock()
		return 0, err
	}
	if err := w.rolloverLocked(); err != nil {
		w.mu.Unlock()
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return 0, err
	}
	end := w.appendedPos
	w.mu.Unlock()
	w.ctlMu.Lock()
	old := int64(w.ctl.OldestRetainedPos) //nolint:gosec // G115: positions are bounded
	w.ctlMu.Unlock()
	if err := w.MarkCheckpoint(end); err != nil {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return 0, err
	}
	if _, err := w.ReclaimSegments(); err != nil {
		metrics.IncCounter("store.wal.Truncate.errors", 1)
		return 0, err
	}
	return end - old, nil
}

// truncateSingleFile is [Writer.Truncate] for an [OpenWith] writer.
func (w *Writer) truncateSingleFile() (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.syncErr != nil {
		return 0, w.syncErr
	}
	for w.leaderActive {
		w.groupCond.Wait()
	}
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
	// Positions never reset: the file now starts at the current position.
	w.durablePos, w.durableLastFramePos = w.appendedPos, w.lastFramePos
	w.segStart, w.fileBase = w.appendedPos, 0
	w.bw.Reset(w.f)
	return sz, nil
}

// Close flushes any buffered frames, fsyncs the active segment, waits for the
// segment preparer, and releases every file and the lock.
//
// On a writer poisoned by an earlier sync failure, Close skips the flush and
// performs a second-chance truncation of the un-synced suffix followed by a
// best-effort fsync, then returns the sticky error.
func (w *Writer) Close() error {
	defer metrics.Time("store.wal.Close").Stop()
	if !w.closed.CompareAndSwap(false, true) {
		metrics.IncCounter("store.wal.Close.errors", 1)
		return ErrWriterClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// Self-safe against an in-flight group-commit leader, which fsyncs with
	// w.mu released: never close the file under it.
	for w.leaderActive {
		w.groupCond.Wait()
	}
	w.prepWG.Wait()
	w.spareMu.Lock()
	if w.spare != nil {
		_ = w.spare.Close() // the prepared spare stays on disk as an empty tail segment
		w.spare = nil
	}
	w.spareMu.Unlock()
	release := func() {
		if w.lockFile != nil {
			releaseLock(w.lockFile)
		}
	}
	if w.syncErr != nil {
		_ = w.f.Truncate(w.activeOffset(w.durablePos))
		_ = w.f.Sync()
		_ = w.f.Close()
		release()
		metrics.IncCounter("store.wal.Close.errors", 1)
		return w.syncErr
	}
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()
		release()
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		release()
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	if err := w.f.Close(); err != nil {
		release()
		metrics.IncCounter("store.wal.Close.errors", 1)
		return err
	}
	release()
	return nil
}

// prefixMarkerSuffix is appended to the WAL path to name the legacy
// prefix-truncation marker; see [PrefixTruncatedMarkerPath].
const prefixMarkerSuffix = ".prefix-truncated"

// prefixMarkerBody is the marker's content. Only the file's presence is
// significant.
const prefixMarkerBody = "GoGraph WAL: the prefix of this log was truncated by a checkpoint; " +
	"recovery requires the snapshot that folded it.\n"

// PrefixTruncatedMarkerPath returns the path of the legacy prefix-truncation
// marker next to the WAL at walPath: the record a single-file log used to state
// that its history requires a snapshot (rmp #2990). Writers of the segmented
// format record that fact in the control file ([ControlPrefixTruncated]) and
// never write the marker; recovery still honours a marker it finds, and writes
// one for a legacy store loaded from a self-sufficient snapshot before that
// store is migrated (rmp #3002).
//
// It is a pure function of walPath and is safe for concurrent use.
func PrefixTruncatedMarkerPath(walPath string) string { return walPath + prefixMarkerSuffix }

// WritePrefixMarker makes the legacy prefix-truncation marker for the WAL at
// walPath durable on the operating-system filesystem (temp, fsync, rename,
// parent-directory fsync). Recovery uses it for a legacy store (rmp #3002).
//
// Safe for concurrent use with respect to other directories; two concurrent
// calls for the same walPath share a temp file and must not overlap.
func WritePrefixMarker(walPath string) error {
	return writePrefixMarkerFS(osWALFS{}, parentDirFsync, walPath)
}

// WritePrefixMarkerFS is [WritePrefixMarker] over a caller-supplied filesystem
// backend, whose ParentDirSync makes the rename durable. It is the seam the
// deterministic-simulation harness uses.
func WritePrefixMarkerFS(fsys walFS, walPath string) error {
	if fsys == nil {
		return fmt.Errorf("wal: WritePrefixMarkerFS: nil filesystem")
	}
	return writePrefixMarkerFS(fsys, fsys.ParentDirSync, walPath)
}

func writePrefixMarkerFS(fsys walFS, dirFsync func(string) error, walPath string) error {
	if err := writeFileDurably(fsys, dirFsync, PrefixTruncatedMarkerPath(walPath), []byte(prefixMarkerBody), "", ""); err != nil {
		return fmt.Errorf("wal: prefix marker: %w", err)
	}
	return nil
}
