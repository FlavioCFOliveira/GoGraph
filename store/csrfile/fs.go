package csrfile

import (
	"io"
	"os"
)

// File is the minimal open-file handle the csrfile writer needs. *os.File
// satisfies it directly, and the in-memory DST backend supplies an equivalent
// handle, so the same write path serves both without a branch.
//
// It is exported so an external filesystem backend (the deterministic-
// simulation harness) can name it as the return type of its Create method and
// thereby satisfy the unexported [fs] interface; production code never
// references it directly.
//
// Concurrency: a File is used by a single writer goroutine for the duration of
// one [WriteToFile] call and is not safe for concurrent use; the concurrency
// guarantees of any implementation are the implementation's own.
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// fs is the filesystem seam csrfile writes and reads through. The default
// backend ([osFS]) calls today's os.* operations verbatim, so the production
// path is byte-identical to the pre-seam code; the deterministic-simulation
// harness (internal/sim) supplies an in-memory backend so it can crash mid
// write and during the publish rename.
//
// The interface is intentionally unexported: production callers reach csrfile
// through [WriteToFile] / [Open], which use [osFS]; only the simulator passes
// an alternate backend via the *With constructors.
type fs interface {
	// Create opens path for writing, creating it 0o600 and truncating any
	// existing content. It mirrors the historical
	// os.OpenFile(O_CREATE|O_WRONLY|O_TRUNC, 0o600) call.
	Create(path string) (File, error)
	// Truncate resizes the file at path to size bytes.
	Truncate(path string, size int64) error
	// Rename atomically moves oldPath onto newPath.
	Rename(oldPath, newPath string) error
	// Remove deletes path; a best-effort cleanup, errors are ignored by callers.
	Remove(path string) error
	// ReadFile reads the whole file at path into a []byte whose base address is
	// 8-byte aligned, so the zero-copy reinterpretation in [Reader] is sound.
	ReadFile(path string) ([]byte, error)
	// ParentDirSync makes a preceding rename of childPath durable by fsyncing
	// its parent directory. No-op on platforms without a directory-fsync
	// primitive.
	ParentDirSync(childPath string) error
}

// osFS is the production filesystem backend: every method delegates verbatim to
// the os.* call (or the existing build-tagged helper) that csrfile used before
// the seam was introduced, so the published-file bytes and the durability
// ordering are unchanged.
type osFS struct{}

var _ fs = osFS{}

func (osFS) Create(path string) (File, error) {
	// Create the temp file mode 0600: the CSR payload contains full edge and
	// weight data, so it must not be world- or group-readable. os.Rename
	// preserves the mode, so the published file is 0600 too.
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // caller-supplied path
}

func (osFS) Truncate(path string, size int64) error { return os.Truncate(path, size) }

func (osFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (osFS) Remove(path string) error { return os.Remove(path) }

func (osFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // caller-supplied path
}

func (osFS) ParentDirSync(childPath string) error { return parentDirFsync(childPath) }

// isOS reports whether fsys is the production OS backend, so the read path
// can keep its byte-identical mmap behaviour for [osFS] and fall back to a
// read-into-[]byte for any alternate (in-memory) backend.
func isOS(fsys fs) bool {
	_, ok := fsys.(osFS)
	return ok
}
