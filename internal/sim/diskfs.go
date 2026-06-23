package sim

import (
	"io/fs"
	"os"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// This file wires a [SimDisk] into the filesystem seams of store/snapshot,
// store/csrfile, store/recovery, and store/checkpoint so the deterministic
// simulation can back the WHOLE persistence stack — not just the WAL-only path
// — with the in-memory disk. Each adapter satisfies one package's (unexported)
// filesystem interface by structural typing; the satisfaction check runs here,
// in the package that owns the concrete backend, exactly as wal.OpenWith
// resolves *SimFileHandle at the call site.
//
// All adapters share one *SimDisk, so a crash (drop the in-memory engine, then
// SimDisk.Crash to revoke not-yet-fsync'd dirents) and a reopen via real
// recovery observe one coherent durable image across the WAL and the snapshot.

// simSnapshotFS adapts a [SimDisk] to the store/snapshot filesystem seam.
type simSnapshotFS struct{ disk *SimDisk }

func (s simSnapshotFS) MkdirAll(dir string, _ fs.FileMode) error { return s.disk.MkdirAll(dir, 0) }

func (s simSnapshotFS) Create(path string) (snapshot.File, error) {
	return s.disk.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
}

func (s simSnapshotFS) OpenComponent(path string) (snapshot.ReadFile, error) {
	return s.disk.OpenFile(path, os.O_RDONLY)
}

func (s simSnapshotFS) Open(path string) (snapshot.ReadFile, error) {
	return s.disk.OpenFile(path, os.O_RDONLY)
}

func (s simSnapshotFS) Rename(oldPath, newPath string) error { return s.disk.Rename(oldPath, newPath) }

func (s simSnapshotFS) Remove(path string) error { return s.disk.Remove(path) }

func (s simSnapshotFS) RemoveAll(path string) error { return s.disk.RemoveAll(path) }

func (s simSnapshotFS) Stat(path string) (fs.FileInfo, error) { return s.disk.Stat(path) }

func (s simSnapshotFS) DirSync(path string) error { return s.disk.DirSync(path) }

func (s simSnapshotFS) ParentDirSync(childPath string) error { return s.disk.ParentDirSync(childPath) }

// simCSRFS adapts a [SimDisk] to the store/csrfile filesystem seam. It is a
// distinct type from [simSnapshotFS] because csrfile's Create returns
// csrfile.File whereas snapshot's returns snapshot.File — one Go type cannot
// carry both Create signatures.
type simCSRFS struct{ disk *SimDisk }

func (s simCSRFS) Create(path string) (csrfile.File, error) {
	return s.disk.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
}

func (s simCSRFS) Truncate(path string, size int64) error { return s.disk.TruncatePath(path, size) }

func (s simCSRFS) Rename(oldPath, newPath string) error { return s.disk.Rename(oldPath, newPath) }

func (s simCSRFS) Remove(path string) error { return s.disk.Remove(path) }

func (s simCSRFS) ReadFile(path string) ([]byte, error) { return s.disk.ReadFile(path) }

func (s simCSRFS) ParentDirSync(childPath string) error { return s.disk.ParentDirSync(childPath) }

// simRecoveryFS adapts a [SimDisk] to the store/recovery filesystem seam. Its
// LoadSnapshot forwards to snapshot.LoadSnapshotFullFS with a [simSnapshotFS]
// over the same disk, so the satisfaction check for the snapshot seam happens
// in this package (where the concrete adapter is named).
type simRecoveryFS struct{ disk *SimDisk }

func (s simRecoveryFS) Stat(path string) (fs.FileInfo, error) { return s.disk.Stat(path) }

func (s simRecoveryFS) Rename(oldPath, newPath string) error { return s.disk.Rename(oldPath, newPath) }

func (s simRecoveryFS) RemoveAll(path string) error { return s.disk.RemoveAll(path) }

func (s simRecoveryFS) ParentDirSync(childPath string) error { return s.disk.ParentDirSync(childPath) }

func (s simRecoveryFS) OpenWALReader(path string) (*wal.Reader, error) {
	rh, err := s.disk.OpenFile(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	return wal.NewReader(rh, rh), nil
}

func (s simRecoveryFS) LoadSnapshot(snapDir string) (snapshot.LoadedSnapshot, error) {
	return snapshot.LoadSnapshotFullFS(simSnapshotFS(s), snapDir)
}

// simCheckpointBackend adapts a [SimDisk] to the store/checkpoint snapshot
// backend seam, routing the snapshot write and the manifest read-back through
// the in-memory disk via [simSnapshotFS]. It is typed on the store's key/weight
// (string/float64) to match the checkpointer.
type simCheckpointBackend struct{ disk *SimDisk }

func (s simCheckpointBackend) WriteSnapshot(snapDir string, cs *csr.CSR[float64], g *lpg.Graph[string, float64], codec txn.Codec[string], constraints []snapshot.ConstraintSpec) error {
	sfs := simSnapshotFS(s)
	if codec != nil {
		return snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(sfs, snapDir, cs, g, codec, constraints)
	}
	// No codec configured: the simulator always supplies the string codec, but
	// honour the nil case for completeness by falling back to the string-keyed
	// constraint-aware writer over the same backend. The FS-aware string-only
	// writer reuses the codec writer with a string codec to stay self-sufficient.
	return snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(sfs, snapDir, cs, g, txn.NewStringCodec(), constraints)
}

func (s simCheckpointBackend) ReadManifest(path string) (snapshot.Manifest, error) {
	return snapshot.ReadManifestFileFS(simSnapshotFS(s), path)
}
