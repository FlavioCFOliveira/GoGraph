package sim

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

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

// simBulkImportFS adapts a [SimDisk] to the store/bulkimport filesystem seam
// ([bulkimport.PublishFS], [bulkimport.ImportIntoFS]): the store/snapshot seam
// plus the directory listing the importer's empty-directory check needs
// (rmp #2518).
type simBulkImportFS struct{ simSnapshotFS }

func (s simBulkImportFS) ReadDir(dir string) ([]fs.DirEntry, error) { return s.disk.ReadDir(dir) }

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

func (s simRecoveryFS) OpenWALLog(walPath string) (*wal.Log, error) {
	return wal.OpenLogFS(simLogFS(s), walPath)
}

// simLogFS adapts a [SimDisk] to the read-only WAL log seam ([wal.LogFS]), so
// recovery reads the control file, the segments and the legacy log off the
// in-memory disk.
type simLogFS struct{ disk *SimDisk }

func (s simLogFS) Open(path string) (io.ReadCloser, error) { return s.disk.OpenFile(path, os.O_RDONLY) }

func (s simLogFS) ReadDir(dir string) ([]string, error) { return simReadDirNames(s.disk, dir) }

// simReadDirNames lists the entry names of dir on disk.
func simReadDirNames(disk *SimDisk, dir string) ([]string, error) {
	ents, err := disk.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

func (s simRecoveryFS) WritePrefixMarker(walPath string) error {
	return wal.WritePrefixMarkerFS(simWALFS(s), walPath)
}

func (s simRecoveryFS) LoadSnapshot(snapDir string) (snapshot.LoadedSnapshot, error) {
	return snapshot.LoadSnapshotFullFS(simSnapshotFS(s), snapDir)
}

// simWALFS adapts a [SimDisk] to the store/wal path-based filesystem seam
// (wal.OpenFS), so the segmented WAL writer's control-file writes, segment
// creation and checkpoint segment unlinks run entirely against the in-memory
// disk. OpenFile returns a *SimFileHandle, which satisfies the
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

func (s simWALFS) ReadDir(dir string) ([]string, error) { return simReadDirNames(s.disk, dir) }

func (s simWALFS) MkdirAll(dir string) error { return s.disk.MkdirAll(dir, 0) }

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

// VerifySnapshotReadable mirrors the production readback
// ([checkpoint.osSnapshotBackend.VerifySnapshotReadable]) step for step, off the
// in-memory disk:
//
//  1. it parses the published snapshot with the SAME full reader the simulated
//     recovery uses ([simRecoveryFS.LoadSnapshot] ->
//     snapshot.LoadSnapshotFullFS), so the checkpointer's pre-truncation
//     readback (rmp #2749) is exercised inside DST with the simulator's fault
//     injection applied to it, exactly as it is in production;
//  2. it then decodes every codec-encoded mapper key with the store's codec,
//     the step the simulated recovery performs next
//     (snapshot.ApplyMapperToGraphWithCodec), so DST also exercises the
//     applier-side half of the guarantee (rmp #2780).
//
// Both halves belong here and not only in production: the codec matrix
// publishes snapshots for key types whose mapper.bin is the version-2 (codec)
// layout, which is the only layout step 2 has anything to check. Extending one
// seam and not the other would leave the simulator testing a weaker guarantee
// than the module ships.
func (s simCheckpointBackend[N, W]) VerifySnapshotReadable(snapDir string, codec txn.Codec[N]) error {
	loaded, err := snapshot.LoadSnapshotFullFS(simSnapshotFS(s), snapDir)
	if err != nil {
		return err
	}
	return snapshot.VerifyMapperDecodable[N](loaded.Mapper, codec)
}

// simWALSegments returns the segment files of the WAL at walPath on disk,
// oldest first; none for a store with no segment directory.
func simWALSegments(disk *SimDisk, walPath string) []string {
	names, err := simReadDirNames(disk, wal.SegmentDir(walPath))
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if strings.HasSuffix(n, ".wal") && len(n) == 16+len(".wal") {
			out = append(out, wal.SegmentDir(walPath)+"/"+n)
		}
	}
	slices.Sort(out) // fixed-width hex names sort numerically
	return out
}

// simWALSegmentHeader is the size of a segment file's header.
const simWALSegmentHeader = 32

// simWALFrameImage returns the frame bytes of the WAL at walPath: every
// segment's bytes past its header, concatenated oldest first — a frame stream
// [wal.NewReader] decodes, whose offsets are log positions relative to the
// oldest retained segment. A store with no segments (a legacy single-file log)
// returns that file's bytes. read selects the visible or the durable image of
// a file.
func simWALFrameImage(disk *SimDisk, walPath string, read func(string) ([]byte, error)) ([]byte, error) {
	segs := simWALSegments(disk, walPath)
	if len(segs) == 0 {
		return read(walPath)
	}
	var out []byte
	for _, p := range segs {
		b, err := read(p)
		if err != nil {
			return nil, err
		}
		if len(b) > simWALSegmentHeader {
			out = append(out, b[simWALSegmentHeader:]...)
		}
	}
	return out, nil
}

// simWALTailSegment returns the newest segment of walPath holding a frame byte,
// and false when there is none.
func simWALTailSegment(disk *SimDisk, walPath string) (string, bool) {
	segs := simWALSegments(disk, walPath)
	for i := len(segs) - 1; i >= 0; i-- {
		b, err := disk.ReadFile(segs[i])
		if err == nil && len(b) > simWALSegmentHeader {
			return segs[i], true
		}
	}
	return "", false
}

// simWALLogImage returns every file of the WAL at walPath — the legacy file,
// the control file and each segment — concatenated, for a check that nothing in
// the log changed.
func simWALLogImage(disk *SimDisk, walPath string) ([]byte, error) {
	var out []byte
	for _, p := range append([]string{walPath, wal.ControlPath(walPath)}, simWALSegments(disk, walPath)...) {
		b, err := disk.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out = append(out, []byte(fmt.Sprintf("%s:%d\n", p, len(b)))...)
		out = append(out, b...)
	}
	return out, nil
}

// simWALReplayable returns how many frame bytes of the WAL of the store under
// dir a recovery would still replay: the bytes at or above the redo position
// when the control file records the published snapshot's checkpoint (the
// prefix-truncated flag and a checkpoint redo position equal to the snapshot's
// wal_redo_pos), and every retained frame byte otherwise. It is the segmented
// log's measure of "the checkpoint emptied the WAL": a checkpoint discards
// whole segments only, but recovery starts from the recorded redo position.
func simWALReplayable(disk *SimDisk, dir string) (int64, error) {
	walPath := walPathFor(dir)
	log, err := wal.OpenLogFS(simLogFS{disk: disk}, walPath)
	if err != nil {
		return 0, err
	}
	var first int64 = -1
	for f := range log.Frames() {
		if first < 0 {
			first = int64(f.Pos)
		}
	}
	end := log.TailOffset()
	ctl, segmented := log.Control()
	if !segmented {
		b, err := simWALFrameImage(disk, walPath, disk.ReadFile)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return int64(len(b)), err
	}
	from := first
	if from < 0 {
		from = end
	}
	if m, merr := snapshot.ReadManifestFileFS(simSnapshotFS{disk: disk}, dir+"/"+simSnapshotName+"/manifest.json"); merr == nil &&
		m.WALFormat == snapshot.WALFormatSegmented && ctl.Flags&wal.ControlPrefixTruncated != 0 &&
		ctl.CheckpointRedoPos == m.WALRedoPos {
		from = int64(m.WALRedoPos)
	}
	return end - from, nil
}
