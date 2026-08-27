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

// simWALFS adapts a [SimDisk] to the store/wal path-based filesystem seam
// (wal.OpenFS), so the WAL writer's crash-safe prefix truncation
// (temp-write -> rename -> parent-dir fsync -> reopen) runs entirely against
// the in-memory disk. OpenFile returns a *SimFileHandle, which satisfies the
// wal package's open-handle interface (Write/Read/Seek/Sync/Truncate/Close);
// the structural satisfaction check for wal's unexported walFS happens at the
// wal.OpenFS call site (OpenSimStore), exactly as wal.OpenWith resolves
// *SimFileHandle there.
type simWALFS struct{ disk *SimDisk }

func (s simWALFS) OpenFile(path string, flag int) (wal.WALFile, error) {
	return s.disk.OpenFile(path, flag)
}

func (s simWALFS) Rename(oldPath, newPath string) error { return s.disk.Rename(oldPath, newPath) }

func (s simWALFS) Remove(path string) error { return s.disk.Remove(path) }

func (s simWALFS) ParentDirSync(childPath string) error { return s.disk.ParentDirSync(childPath) }

// simCheckpointBackend adapts a [SimDisk] to the store/checkpoint snapshot
// backend seam, routing the snapshot write and the manifest read-back through
// the in-memory disk via [simSnapshotFS].
//
// It is generic over the store's key/weight pair (rmp #2473) so the codec
// matrix can publish snapshots for any key type; [SimStore] instantiates it at
// [string, float64]. Nothing about the publish protocol varies with the type
// parameters — only which mapper.bin layout [snapshot.CaptureGraph] ends up
// emitting, which is the point.
type simCheckpointBackend[N comparable, W any] struct{ disk *SimDisk }

func (s simCheckpointBackend[N, W]) CaptureGraph(cs *csr.CSR[W], g *lpg.Graph[N, W], codec txn.Codec[N], wcodec txn.WeightCodec[W], at *lpg.Snapshot) (*snapshot.Capture[W], error) {
	if codec == nil {
		// No codec configured: the simulator always supplies one, but honour the
		// nil case for completeness. For string keys substitute the canonical
		// string codec so the snapshot stays self-sufficient; for any other key
		// type the assertion fails and nil is passed through, which is exactly
		// what the production backend (checkpoint.osSnapshotBackend) does.
		if sc, ok := any(txn.NewStringCodec()).(txn.Codec[N]); ok {
			codec = sc
		}
	}
	// A nil txn.WeightCodec must reach the snapshot package as an untyped nil,
	// or its own nil check sees a non-nil interface holding a nil value and it
	// calls through it (rmp #2526).
	if wcodec != nil {
		return snapshot.CaptureGraphWithWeightCodec[N, W](g, cs, codec, wcodec, at)
	}
	return snapshot.CaptureGraph[N, W](g, cs, codec, at)
}

func (s simCheckpointBackend[N, W]) WriteCapture(snapDir string, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec) error {
	return snapshot.WriteCaptureFS(simSnapshotFS(s), snapDir, capt, constraints, indexDefs)
}

func (s simCheckpointBackend[N, W]) ReadManifest(path string) (snapshot.Manifest, error) {
	return snapshot.ReadManifestFileFS(simSnapshotFS(s), path)
}
