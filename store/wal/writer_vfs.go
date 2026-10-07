package wal

import (
	"fmt"
	"io"
	"os"
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
// and never reference this type directly; tests and the simulator reach for
// [OpenWith] / [OpenFS].
//
// Concurrency: a WALFile is used by a single [Writer] whose own mutex
// serialises every access; any implementation's further guarantees are its own.
type WALFile interface {
	io.Writer
	// Reader lets the writer scan the files it reopens (the legacy log it
	// seals, the tail segment whose torn tail it discards).
	io.Reader
	io.Seeker
	// Sync flushes OS write buffers to durable storage.
	Sync() error
	// Truncate resizes the file to size bytes.
	Truncate(size int64) error
	// Close releases underlying OS resources.
	Close() error
}

// walFS is the path-based filesystem surface a segmented [Writer] works
// through: opening files by path (with os.O_EXCL for a new segment), renaming
// and removing them, listing and creating the segment directory, and fsyncing
// a parent directory.
//
// The interface is intentionally unexported, mirroring the snapshot/recovery
// seams: production callers use [Open] (which installs [osWALFS]), while the
// deterministic-simulation harness (internal/sim) supplies an in-memory backend
// over its SimDisk via [OpenFS], so every segment creation, control-file write
// and segment unlink runs against the simulated disk and can be crashed.
type walFS interface {
	// OpenFile opens (or creates, per flag) the file at path. With os.O_EXCL
	// it fails with an error satisfying errors.Is(err, os.ErrExist) when the
	// file exists; a missing file yields os.ErrNotExist.
	OpenFile(path string, flag int) (WALFile, error)
	// Rename atomically moves oldPath onto newPath.
	Rename(oldPath, newPath string) error
	// Remove deletes path.
	Remove(path string) error
	// ParentDirSync fsyncs the parent directory of childPath.
	ParentDirSync(childPath string) error
	// ReadDir lists the entry names of dir; os.ErrNotExist when it is absent.
	ReadDir(dir string) ([]string, error)
	// MkdirAll creates dir and any missing parent.
	MkdirAll(dir string) error
}

// osWALFS is the production WAL filesystem backend over the os package.
type osWALFS struct{}

var _ walFS = osWALFS{}

// OpenFile opens path with the os flags the caller passes, at mode 0o600, so a
// WAL file is never world-readable.
func (osWALFS) OpenFile(path string, flag int) (WALFile, error) {
	// walNoFollow refuses a symlinked final component (ELOOP) instead of
	// truncating or overwriting an arbitrary process-writable file (CWE-59).
	return os.OpenFile(path, flag|walNoFollow, 0o600) //nolint:gosec // caller-supplied WAL path is by-design
}

func (osWALFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (osWALFS) Remove(path string) error { return os.Remove(path) }

func (osWALFS) ParentDirSync(childPath string) error { return parentDirFsync(childPath) }

func (osWALFS) ReadDir(dir string) ([]string, error) { return readDirNames(dir) }

// MkdirAll creates dir with mode 0o700: the segments carry the graph mutation
// stream.
func (osWALFS) MkdirAll(dir string) error { return os.MkdirAll(dir, 0o700) }

// OpenFS opens or creates the segmented write-ahead log at walPath over a
// caller-supplied filesystem backend; see [OpenFSWithOptions].
func OpenFS(fsys walFS, walPath string) (*Writer, error) {
	return OpenFSWithOptions(fsys, walPath, Options{})
}

// OpenFSWithOptions is [OpenWithOptions] over a caller-supplied filesystem
// backend. It is the seam the deterministic-simulation harness (internal/sim)
// uses to run the full snapshot + WAL + checkpoint stack against its in-memory
// disk; production code uses [Open].
//
// It differs from [OpenWithOptions] in two ways. It takes no OS lock (flock has
// no analogue on an injected backend, and callers are single-writer by
// contract), and it prepares the next segment synchronously at rollover rather
// than in a background goroutine, so the backend sees a deterministic sequence
// of operations. Directory fsyncs go through fsys.ParentDirSync, delayed by
// opts.SyncLatency when set.
func OpenFSWithOptions(fsys walFS, walPath string, opts Options) (*Writer, error) {
	if fsys == nil {
		return nil, fmt.Errorf("wal: OpenFS: nil filesystem")
	}
	if walPath == "" {
		return nil, fmt.Errorf("wal: OpenFS: empty path")
	}
	segSize, err := opts.segmentSize()
	if err != nil {
		return nil, err
	}
	dirFsync := fsys.ParentDirSync
	if lat := opts.SyncLatency; lat != nil {
		dirFsync = func(p string) error {
			lat.wait()
			return fsys.ParentDirSync(p)
		}
	}
	w, err := openSegmented(walPath, fsys, dirFsync, opts.SyncLatency, segSize, false)
	if err != nil {
		return nil, fmt.Errorf("wal: OpenFS %q: %w", walPath, err)
	}
	return w, nil
}
