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
// abstracts only the snapshot-package calls so the simulator can route them
// through its in-memory filesystem.
//
// The default backend ([osSnapshotBackend]) calls
// snapshot.WriteSnapshotFullWith* / snapshot.ReadManifestFile /
// snapshot.LoadSnapshotFull / snapshot.VerifyMapperDecodable verbatim, so the
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
	// wcodec, when non-nil, is adopted by the capture and used to serialise the
	// CSR weights column for weight types the fixed-width layout cannot size
	// (rmp #2526). Nil keeps the fixed-width-only behaviour, under which a
	// capture holding such weights REFUSES to publish rather than publishing a
	// weightless image the WAL truncation would then make permanent.
	CaptureGraph(cs *csr.CSR[W], g *lpg.Graph[N, W], codec txn.Codec[N], wcodec txn.WeightCodec[W], at *lpg.Snapshot) (*snapshot.Capture[W], error)
	// WriteCapture publishes a capture taken by CaptureGraph to snapDir, adding
	// constraints.bin from constraints (nil/empty emits none) and indexdefs.bin
	// from indexDefs (nil/empty emits none). It touches no graph, so the
	// checkpointer runs it LOCK-FREE in phase 2 without any component observing
	// a later state than the capture.
	WriteCapture(snapDir string, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec) error
	// ReadManifest reads the manifest at path (used to verify snapshot
	// self-sufficiency before truncating the WAL).
	ReadManifest(path string) (snapshot.Manifest, error)
	// VerifySnapshotReadable establishes that the snapshot published at snapDir
	// can actually be READ BACK AND APPLIED by recovery, returning nil only when
	// every component the manifest declares was opened and parsed AND every
	// codec-encoded mapper key in it decoded through codec.
	//
	// An implementation MUST perform BOTH halves, each with the same call
	// recovery makes, and MUST NOT substitute a cheaper proxy for either:
	//
	//   - THE PARSE — snapshot.LoadSnapshotFull, matching
	//     store/recovery.osBackend.LoadSnapshot. Not stat-ing the files, not
	//     re-reading the manifest, not checking a CRC alone (rmp #2749).
	//   - THE DECODE — snapshot.VerifyMapperDecodable over the mapper readback
	//     the parse produced, matching the decode step of
	//     snapshot.ApplyMapperToGraphWithCodec, which is what recovery calls
	//     next. Not a spot check of one key, not a length or count comparison,
	//     not an inference from the format version (rmp #2780).
	//
	// The two halves are not interchangeable. snapshot.ReadMapperBytes validates
	// magic, format version, record framing and the per-key length cap and then
	// returns the key bytes verbatim, asking no codec anything — so an image the
	// parse accepts can still be one whose keys the codec refuses, and recovery
	// then fails on the very snapshot this approved.
	//
	// codec is the store's node-identifier codec (the checkpointer passes its
	// own, the one that wrote the mapper). It may be nil, in which case no
	// version-2 mapper can have been published and there is nothing to decode.
	//
	// The checkpointer calls this before it truncates the WAL prefix, so a
	// weaker check in either half is a Durability defect: it would discard the
	// only other copy of the data behind an image recovery cannot open.
	VerifySnapshotReadable(snapDir string, codec txn.Codec[N]) error
}

// osSnapshotBackend is the production backend: it delegates to the snapshot
// package's OS-backed writers and reader, so the published snapshot bytes and
// the manifest read are byte-identical to the pre-seam checkpointer.
type osSnapshotBackend[N comparable, W any] struct{}

func (osSnapshotBackend[N, W]) CaptureGraph(cs *csr.CSR[W], g *lpg.Graph[N, W], codec txn.Codec[N], wcodec txn.WeightCodec[W], at *lpg.Snapshot) (*snapshot.Capture[W], error) {
	// A nil txn.Codec / txn.WeightCodec interface value must be passed on as an
	// untyped nil: handing a typed-nil interface to the snapshot package would
	// make its own nil check false and it would call through a nil codec.
	if codec != nil {
		if wcodec != nil {
			return snapshot.CaptureGraphWithWeightCodec[N, W](g, cs, codec, wcodec, at)
		}
		return snapshot.CaptureGraph[N, W](g, cs, codec, at)
	}
	// No codec: mapper.bin is emitted for string-keyed graphs only, keeping the
	// historical v2 fallback for every other key type.
	if wcodec != nil {
		return snapshot.CaptureGraphWithWeightCodec[N, W](g, cs, nil, wcodec, at)
	}
	return snapshot.CaptureGraph[N, W](g, cs, nil, at)
}

func (osSnapshotBackend[N, W]) WriteCapture(snapDir string, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec) error {
	return snapshot.WriteCapture(snapDir, capt, constraints, indexDefs)
}

func (osSnapshotBackend[N, W]) ReadManifest(path string) (snapshot.Manifest, error) {
	return snapshot.ReadManifestFile(path)
}

// VerifySnapshotReadable performs the production readback in the two steps
// recovery performs, in recovery's order:
//
//  1. snapshot.LoadSnapshotFull — the SAME call store/recovery makes to load a
//     snapshot at startup (recovery.osBackend.LoadSnapshot), so a snapshot this
//     accepts is one recovery's reader can parse (rmp #2749).
//  2. snapshot.VerifyMapperDecodable — the decode step of the SAME call
//     recovery makes next on the readback it just obtained
//     (snapshot.ApplyMapperToGraphWithCodec, store/recovery/recovery.go), so a
//     snapshot this accepts is also one whose mapper keys recovery's codec can
//     decode (rmp #2780).
//
// Step 2 adds no I/O: it decodes the mapper bytes step 1 already read into
// memory. It is not folded into step 1 because the snapshot reader has no codec
// of its own — the version-2 layout's key bytes are opaque to it by design, and
// the codec belongs to the store.
//
// The parsed image is discarded once both steps agree: only the error is
// load-bearing, and holding the readback alive would double the checkpoint's
// peak footprint for no gain.
func (osSnapshotBackend[N, W]) VerifySnapshotReadable(snapDir string, codec txn.Codec[N]) error {
	loaded, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		return err
	}
	return snapshot.VerifyMapperDecodable[N](loaded.Mapper, codec)
}

// manifestPath returns the manifest.json path inside a snapshot directory.
func manifestPath(dir string) string { return filepath.Join(dir, "manifest.json") }
