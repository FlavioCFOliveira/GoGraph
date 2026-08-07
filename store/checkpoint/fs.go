package checkpoint

import (
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// snapshotBackend is the seam the checkpointer publishes and probes snapshots
// through. The checkpointer itself performs NO direct filesystem call: it
// writes the snapshot via the snapshot package and reads back the manifest to
// verify self-sufficiency, and the WAL fsync/truncate it does go through the
// injected [github.com/FlavioCFOliveira/GoGraph/store/wal.Writer] (which is
// already backed by the simulator's in-memory disk in DST). This interface
// abstracts only the two snapshot-package calls so the simulator can route
// them through its in-memory filesystem.
//
// The default backend ([osSnapshotBackend]) calls
// snapshot.WriteSnapshotFullWith* / snapshot.ReadManifestFile verbatim, so the
// production checkpoint path is byte-identical to the pre-seam code. The
// deterministic-simulation harness supplies an in-memory backend via
// [WithSnapshotFS].
//
// The type parameters mirror the checkpointer's so the writer can take the
// live typed graph and CSR without boxing.
type snapshotBackend[N comparable, W any] interface {
	// CaptureGraph serialises every live-graph-derived snapshot component of g
	// into an atomic in-memory image, emitting mapper.bin via codec (nil
	// selects the string-only mapper). at is the MVCC instant every component is
	// resolved at: the checkpointer opens it in phase 1 under the commit
	// serialisation and calls this in phase 1b with the lock RELEASED, so the
	// image is a single transaction-boundary instant while writers commit
	// throughout (rmp #2310).
	CaptureGraph(cs *csr.CSR[W], g *lpg.Graph[N, W], codec txn.Codec[N], at *lpg.Snapshot) (*snapshot.Capture[W], error)
	// WriteCapture publishes a capture taken by CaptureGraph to snapDir, adding
	// constraints.bin from constraints (nil/empty emits none) and indexdefs.bin
	// from indexDefs (nil/empty emits none). It touches no graph, so the
	// checkpointer runs it LOCK-FREE in phase 2 without any component observing
	// a later state than the capture.
	WriteCapture(snapDir string, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec) error
	// ReadManifest reads the manifest at path (used to verify snapshot
	// self-sufficiency before truncating the WAL).
	ReadManifest(path string) (snapshot.Manifest, error)
}

// osSnapshotBackend is the production backend: it delegates to the snapshot
// package's OS-backed writers and reader, so the published snapshot bytes and
// the manifest read are byte-identical to the pre-seam checkpointer.
type osSnapshotBackend[N comparable, W any] struct{}

func (osSnapshotBackend[N, W]) CaptureGraph(cs *csr.CSR[W], g *lpg.Graph[N, W], codec txn.Codec[N], at *lpg.Snapshot) (*snapshot.Capture[W], error) {
	if codec != nil {
		return snapshot.CaptureGraph[N, W](g, cs, codec, at)
	}
	// No codec: mapper.bin is emitted for string-keyed graphs only, keeping the
	// historical v2 fallback for every other key type.
	return snapshot.CaptureGraph[N, W](g, cs, nil, at)
}

func (osSnapshotBackend[N, W]) WriteCapture(snapDir string, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec) error {
	return snapshot.WriteCapture(snapDir, capt, constraints, indexDefs)
}

func (osSnapshotBackend[N, W]) ReadManifest(path string) (snapshot.Manifest, error) {
	return snapshot.ReadManifestFile(path)
}

// manifestPath returns the manifest.json path inside a snapshot directory.
func manifestPath(dir string) string { return filepath.Join(dir, "manifest.json") }
