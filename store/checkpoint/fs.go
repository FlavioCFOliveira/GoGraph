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
	// WriteSnapshot publishes a self-sufficient snapshot of g to snapDir,
	// emitting mapper.bin via codec (nil selects the string-only mapper) and
	// constraints.bin from constraints (nil/empty emits none).
	WriteSnapshot(snapDir string, cs *csr.CSR[W], g *lpg.Graph[N, W], codec txn.Codec[N], constraints []snapshot.ConstraintSpec) error
	// ReadManifest reads the manifest at path (used to verify snapshot
	// self-sufficiency before truncating the WAL).
	ReadManifest(path string) (snapshot.Manifest, error)
}

// osSnapshotBackend is the production backend: it delegates to the snapshot
// package's OS-backed writers and reader, so the published snapshot bytes and
// the manifest read are byte-identical to the pre-seam checkpointer.
type osSnapshotBackend[N comparable, W any] struct{}

func (osSnapshotBackend[N, W]) WriteSnapshot(snapDir string, cs *csr.CSR[W], g *lpg.Graph[N, W], codec txn.Codec[N], constraints []snapshot.ConstraintSpec) error {
	if codec != nil {
		return snapshot.WriteSnapshotFullWithMapperCodecAndConstraints(snapDir, cs, g, codec, constraints)
	}
	return snapshot.WriteSnapshotFullWithConstraints(snapDir, cs, g, constraints)
}

func (osSnapshotBackend[N, W]) ReadManifest(path string) (snapshot.Manifest, error) {
	return snapshot.ReadManifestFile(path)
}

// manifestPath returns the manifest.json path inside a snapshot directory.
func manifestPath(dir string) string { return filepath.Join(dir, "manifest.json") }
