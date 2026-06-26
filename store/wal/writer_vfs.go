package wal

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
)

// WALFile is the minimal open-handle interface that [Writer] requires of its
// underlying file. *os.File and *testfs.FaultFile both satisfy it, which lets
// fault-injection tests substitute a synthetic file without touching
// production paths.
//
// It is exported so an external filesystem backend (the deterministic-
// simulation harness, internal/sim) can name it as the return type of its
// [walFS].OpenFile method and thereby satisfy the unexported walFS interface,
// exactly as [github.com/FlavioCFOliveira/GoGraph/store/snapshot.File] is
// exported for the snapshot seam. Production callers open WAL files via [Open]
// (which wraps *os.File) and never reference this type directly; tests and the
// simulator reach for [OpenWith] / [OpenFS].
//
// Concurrency: a WALFile is used by a single [Writer] whose own mutex
// serialises every access; any implementation's further guarantees are its own.
type WALFile interface {
	io.Writer
	// Reader is required by [Writer.TruncatePrefix], which reads the
	// surviving suffix of the file (the frames committed after the
	// captured watermark) before atomically replacing the file with a
	// suffix-only copy. *os.File and *testfs.FaultFile both satisfy it.
	io.Reader
	io.Seeker
	// Sync flushes OS write buffers to durable storage.
	Sync() error
	// Truncate resizes the file to size bytes.
	Truncate(size int64) error
	// Close releases underlying OS resources.
	Close() error
}

// OpenWith builds a [Writer] over an already-open file handle. The
// caller transfers ownership: [Writer.Close] will call f.Close().
//
// This constructor exists primarily for tests that inject a
// *testfs.FaultFile; production code should use [Open].
func OpenWith(f WALFile) (*Writer, error) {
	if f == nil {
		return nil, fmt.Errorf("wal: OpenWith: nil file")
	}
	// Seek to the end so Append is truly append-only even when the
	// caller opened the file without O_APPEND. The resulting position
	// doubles as the durable baseline: bytes already in the file at
	// open time are presumed durable, so a later sync failure rolls
	// the file back exactly here.
	pos, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: OpenWith: seek to end: %w", err)
	}
	w := &Writer{
		f:            f,
		dirFsync:     parentDirFsync,
		bw:           bufio.NewWriterSize(f, 64*1024),
		durableSize:  pos,
		appendedSize: pos,
	}
	w.fsys = osWALFS{}
	w.groupCond = sync.NewCond(&w.mu)
	return w, nil
}

// walFS is the minimal path-based filesystem surface that
// [Writer.TruncatePrefix] requires to perform its crash-safe prefix
// truncation: it writes the surviving WAL suffix to a sibling temp file,
// atomically renames it over the WAL path, fsyncs the parent directory, and
// reopens the new inode. Those four operations (open-by-path, rename, remove,
// parent-dir fsync) are the only filesystem touches in the truncation that are
// not already expressed through the open-handle [WALFile] interface.
//
// The interface is intentionally unexported, mirroring the snapshot/recovery
// seams: production callers use [Open] (which installs [osWALFS], so the
// truncation is byte-identical to the pre-seam code), while the
// deterministic-simulation harness (internal/sim) supplies an in-memory
// backend over its [SimDisk] via [OpenFS] so a Checkpointer-driven WAL
// truncation runs entirely against the simulated disk and can be crashed
// across the truncate boundary.
//
// ParentDirSync mirrors the [Writer.dirFsync] field's contract (fsync the
// parent directory of childPath); [OpenFS] wires dirFsync to fsys.ParentDirSync
// so the post-rename directory fsync also routes through the injected backend.
type walFS interface {
	// OpenFile opens (or creates, per flag) the file at path and returns a
	// handle satisfying [WALFile]. The temp-file write and the post-rename
	// reopen both go through this call.
	OpenFile(path string, flag int) (WALFile, error)
	// Rename atomically moves oldPath onto newPath (the suffix-temp publish).
	Rename(oldPath, newPath string) error
	// Remove deletes path (best-effort cleanup of a partial temp file).
	Remove(path string) error
	// ParentDirSync fsyncs the parent directory of childPath, making the
	// suffix-temp rename durable.
	ParentDirSync(childPath string) error
}

// osWALFS is the production WAL filesystem backend: every method delegates
// verbatim to the os.* call (with the exact same flags and 0o600 mode) or the
// build-tagged parentDirFsync that [Writer.TruncatePrefix] used before the seam
// was introduced, so the published bytes and the truncation sequence are
// byte-identical to the pre-seam path.
type osWALFS struct{}

var _ walFS = osWALFS{}

// OpenFile opens path with the os flags the caller passes, at mode 0o600 — the
// same restrictive mode [Open] and the pre-seam writeSuffixTmp/reopen used, so
// the WAL temp and the reopened inode are never world-readable.
func (osWALFS) OpenFile(path string, flag int) (WALFile, error) {
	return os.OpenFile(path, flag, 0o600) //nolint:gosec // caller-supplied WAL path is by-design
}

func (osWALFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (osWALFS) Remove(path string) error { return os.Remove(path) }

func (osWALFS) ParentDirSync(childPath string) error { return parentDirFsync(childPath) }

// OpenFS builds a path-backed [Writer] over a caller-supplied filesystem
// backend. Unlike [OpenWith] (which produces a path-less Writer that rejects
// [Writer.TruncatePrefix] with [ErrPrefixTruncateUnsupported]), OpenFS records
// the path and routes the truncation's temp-write/rename/remove/parent-dir-fsync
// through fsys, so a Checkpointer can reclaim the WAL prefix over the injected
// filesystem. The caller transfers ownership of the opened handle: [Writer.Close]
// will close it.
//
// OpenFS is the seam the deterministic-simulation harness (internal/sim) uses to
// run the full snapshot + WAL + checkpoint stack against its in-memory
// [SimDisk]; production code uses [Open].
//
// Unlike [Open], OpenFS does NOT acquire the OS-level WAL directory lock
// (flock(2)/O_EXCL has no analogue on an injected backend, and OpenFS callers
// are single-writer by contract) and does NOT scan for or discard a benign
// torn tail. The caller MUST therefore pre-truncate any benign torn tail to the
// last durable frame boundary (recovery reports it as
// [github.com/FlavioCFOliveira/GoGraph/store/recovery.ReplayResult.WALTailOffset])
// BEFORE calling OpenFS, exactly as the production [Open] does internally via
// discardTornTail; appending after un-discarded torn junk would strand every
// new frame behind bytes that every reader stops at — a Durability violation.
func OpenFS(fsys walFS, path string) (*Writer, error) {
	if fsys == nil {
		return nil, fmt.Errorf("wal: OpenFS: nil filesystem")
	}
	if path == "" {
		return nil, fmt.Errorf("wal: OpenFS: empty path")
	}
	f, err := fsys.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return nil, fmt.Errorf("wal: OpenFS: open %q: %w", path, err)
	}
	// Seek to the end so Append is append-only and the durable baseline is the
	// current (already torn-tail-free, per the contract above) file size: bytes
	// present at open time are presumed durable, so a later sync failure rolls
	// the file back exactly here. Mirrors [OpenWith] and [Open].
	pos, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: OpenFS: seek to end: %w", err)
	}
	w := &Writer{
		f:            f,
		path:         path,
		fsys:         fsys,
		dirFsync:     fsys.ParentDirSync,
		bw:           bufio.NewWriterSize(f, 64*1024),
		durableSize:  pos,
		appendedSize: pos,
	}
	w.groupCond = sync.NewCond(&w.mu)
	return w, nil
}
