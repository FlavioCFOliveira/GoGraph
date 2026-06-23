package recovery

import (
	"io/fs"
	"os"

	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// recoveryFS is the filesystem seam the recovery package performs its
// snapshot-side operations through: the interrupted-publish repair (stat +
// rename + remove + parent-dir fsync), the WAL probe/open, and the snapshot
// load. The default backend ([osBackend]) delegates verbatim to today's os.*
// calls, the build-tagged parentDirFsync, the path-based wal.OpenReader, and
// snapshot.LoadSnapshotFull, so the production recovery path is byte-identical
// to the pre-seam code. The deterministic-simulation harness (internal/sim)
// supplies an in-memory backend so it can recover a snapshot + WAL image
// entirely from its in-memory disk and crash across the snapshot-promote
// boundary.
//
// LoadSnapshot is a method on the backend (rather than recovery forwarding a
// snapshot filesystem itself) so the unexported-interface satisfaction check
// for the snapshot package's own seam happens inside the package that owns the
// concrete backend (internal/sim), exactly as wal.OpenWith resolves
// *SimFileHandle at the sim call site.
//
// The interface is intentionally unexported, mirroring
// [github.com/FlavioCFOliveira/GoGraph/store/wal.OpenWith]; production callers
// use [Open] / [OpenCtx], which supply [osBackend].
type recoveryFS interface {
	// Stat returns file info for path; used to probe for snapshot manifests
	// and the WAL.
	Stat(path string) (fs.FileInfo, error)
	// Rename atomically moves oldPath onto newPath (snapshot.bak promotion).
	Rename(oldPath, newPath string) error
	// RemoveAll recursively removes path (stale staging cleanup).
	RemoveAll(path string) error
	// ParentDirSync fsyncs the parent directory of childPath, making the
	// snapshot-backup promotion rename durable.
	ParentDirSync(childPath string) error
	// OpenWALReader opens the WAL at path for replay.
	OpenWALReader(path string) (*wal.Reader, error)
	// LoadSnapshot loads the snapshot rooted at snapDir.
	LoadSnapshot(snapDir string) (snapshot.LoadedSnapshot, error)
}

// osBackend is the production recovery filesystem backend: every method
// delegates verbatim to the os.* call, the build-tagged parentDirFsync,
// wal.OpenReader, or snapshot.LoadSnapshotFull that recovery used before the
// seam was introduced.
type osBackend struct{}

var _ recoveryFS = osBackend{}

func (osBackend) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

func (osBackend) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (osBackend) RemoveAll(path string) error { return os.RemoveAll(path) }

func (osBackend) ParentDirSync(childPath string) error { return parentDirFsync(childPath) }

func (osBackend) OpenWALReader(path string) (*wal.Reader, error) { return wal.OpenReader(path) }

func (osBackend) LoadSnapshot(snapDir string) (snapshot.LoadedSnapshot, error) {
	return snapshot.LoadSnapshotFull(snapDir)
}
