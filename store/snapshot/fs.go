package snapshot

import (
	"io"
	"io/fs"
	"os"
)

// File is the minimal handle the snapshot writers need of an open component
// file. *os.File satisfies it directly; the in-memory DST backend supplies an
// equivalent handle. Stat is required by the per-index writer, which records
// the file's on-disk size in the manifest.
//
// It is exported so an external filesystem backend (the deterministic-
// simulation harness) can name it as the return type of its Create method and
// thereby satisfy the unexported [fileSystem] interface; production code never
// references it directly.
//
// Concurrency: a File is used by a single writer goroutine while one snapshot
// component is written and is not safe for concurrent use; any implementation's
// concurrency guarantees are its own.
type File interface {
	io.Writer
	Sync() error
	Close() error
	Stat() (fs.FileInfo, error)
}

// ReadFile is the minimal handle the snapshot readers need of an open
// component file. *os.File satisfies it directly; the in-memory backend
// supplies an equivalent read handle. It is exported for the same reason as
// [File].
//
// Concurrency: a ReadFile is consumed by a single reader goroutine while one
// snapshot component is read and is not safe for concurrent use; any
// implementation's concurrency guarantees are its own.
type ReadFile interface {
	io.Reader
	Close() error
}

// fileSystem is the filesystem seam the snapshot package writes and reads
// through. The default backend ([osBackend]) calls today's os.* operations
// and the existing build-tagged helpers (createSnapshotFile,
// openSnapshotComponent, dirFsync, parentDirFsync) verbatim, so the
// published snapshot bytes and the durability ordering are byte-identical to
// the pre-seam code. The deterministic-simulation harness (internal/sim)
// supplies an in-memory backend so it can crash mid-snapshot publish and
// during the WAL prefix-truncate that follows a checkpoint.
//
// The interface is intentionally unexported: production callers reach the
// snapshot package through the existing exported functions, which use
// [osBackend]; only the simulator passes an alternate backend via the *With
// constructors.
type fileSystem interface {
	// MkdirAll creates dir and any missing parents with perm.
	MkdirAll(dir string, perm fs.FileMode) error
	// Create opens path for writing as a snapshot component (0o600,
	// create+truncate). It mirrors createSnapshotFile.
	Create(path string) (File, error)
	// OpenComponent opens path read-only for a snapshot component, with the
	// O_NOFOLLOW symlink-escape guard on the OS backend (see
	// openSnapshotComponent).
	OpenComponent(path string) (ReadFile, error)
	// Open opens path read-only with a plain open (the csr.bin read in the
	// legacy [Open] path, which historically used os.Open without
	// O_NOFOLLOW).
	Open(path string) (ReadFile, error)
	// Rename atomically moves oldPath onto newPath.
	Rename(oldPath, newPath string) error
	// Remove deletes a single file at path.
	Remove(path string) error
	// RemoveAll recursively removes path (staging/backup cleanup).
	RemoveAll(path string) error
	// Stat returns file info for path; used to probe for the manifest.
	Stat(path string) (fs.FileInfo, error)
	// DirSync fsyncs the directory at path, making its dirents durable
	// (staging-directory fsync before publish, indexes/ dir fsync). No-op on
	// platforms without a directory-fsync primitive.
	DirSync(path string) error
	// ParentDirSync fsyncs the parent directory of childPath, making a
	// preceding rename durable. No-op on platforms without a directory-fsync
	// primitive.
	ParentDirSync(childPath string) error
}

// osBackend is the production filesystem backend: every method delegates
// verbatim to the os.* call or the existing build-tagged helper that the
// snapshot package used before the seam was introduced, so the published
// bytes and durability ordering are unchanged.
type osBackend struct{}

var _ fileSystem = osBackend{}

func (osBackend) MkdirAll(dir string, perm fs.FileMode) error { return os.MkdirAll(dir, perm) }

func (osBackend) Create(path string) (File, error) { return createSnapshotFile(path) }

func (osBackend) OpenComponent(path string) (ReadFile, error) { return openSnapshotComponent(path) }

func (osBackend) Open(path string) (ReadFile, error) {
	return os.Open(path) //nolint:gosec // caller-supplied path (legacy csr.bin read path)
}

func (osBackend) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (osBackend) Remove(path string) error { return os.Remove(path) }

func (osBackend) RemoveAll(path string) error { return os.RemoveAll(path) }

func (osBackend) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

func (osBackend) DirSync(path string) error { return dirFsync(path) }

func (osBackend) ParentDirSync(childPath string) error { return parentDirFsync(childPath) }
