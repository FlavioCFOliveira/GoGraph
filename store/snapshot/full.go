package snapshot

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// LoadedSnapshot is the result of [LoadSnapshotFull]: the parsed CSR
// arrays, the parsed labels readback (empty for v1 snapshots), the
// parsed properties readback (empty when properties.bin is absent),
// the parsed mapper readback (empty when mapper.bin is absent, e.g. a
// v1 CSR-only snapshot or a v2 snapshot written without a codec for a
// non-string key type), the optional per-index byte payloads (one
// entry per indexes/<name>.bin file referenced by the manifest), and
// the manifest that produced them.
//
// When mapper.bin is present, exactly one of [MapperReadback.Pairs]
// (version-1 string layout) and [MapperReadback.RawPairs] (version-2
// codec layout) is populated; see [MapperReadback].
//
// Each [IndexReadback].Bytes may be nil even when the manifest
// references the index — that signals the file was missing or its
// CRC32C did not validate. Callers must treat nil bytes as "rebuild
// from LPG" rather than as a fatal error; the corruption was already
// metered by [LoadIndexes] under `store.snapshot.indexes.corrupted`.
type LoadedSnapshot struct {
	Labels     LabelsReadback
	Properties PropertiesReadback
	Mapper     MapperReadback
	Indexes    []IndexReadback
	// Tombstones is the node-removal set restored from tombstones.bin. It
	// is empty for snapshots that carry no tombstones.bin entry (older
	// snapshots, or any snapshot of a graph that never removed a node) —
	// the backward-compatibility contract.
	Tombstones TombstonesReadback
	// EdgeHandles is the per-handle edge metadata restored from
	// edgehandles.bin (each parallel edge's per-CREATE relationship type and
	// properties keyed by its stable handle). It is empty for snapshots that
	// carry no edgehandles.bin entry (older snapshots, or any snapshot of a
	// graph that never used the handle-keyed metadata stores) — the
	// backward-compatibility contract.
	EdgeHandles EdgeHandlesReadback
	// Constraints is the durable schema constraint set restored from
	// constraints.bin. It is empty for snapshots that carry no constraints.bin
	// entry (older snapshots, or any snapshot taken with no constraints
	// declared) — the backward-compatibility contract.
	Constraints ConstraintsReadback
	// IndexDefs is the durable secondary-index definition set restored from
	// indexdefs.bin (each index's label/property/kind/name). It is empty for
	// snapshots that carry no indexdefs.bin entry (older snapshots, or any
	// snapshot taken with no indexes declared) — the backward-compatibility
	// contract. It is DISTINCT from [LoadedSnapshot.Indexes], which holds the
	// optional per-index byte payloads; the definition set is what recovery
	// rebuilds each index from (#1755).
	IndexDefs IndexDefsReadback
	CSR       CSRReadback
	Manifest  Manifest
}

// WriteSnapshotFull is the v2/v3 high-level helper: it lays out a
// snapshot directory containing csr.bin (legacy v1 component),
// labels.bin (v2 component), properties.bin (v2 component) and a
// manifest indexing them. When the underlying [graph.Mapper] is
// string-keyed (N=string) the writer additionally emits mapper.bin —
// the durable (NodeID -> natural key) interning table — and the
// manifest is stamped at [ManifestVersion] (v3). For any other N the
// writer falls back to the v2 layout (no mapper.bin) and the manifest
// records [manifestVersionV2]; recovery from a v2 snapshot continues
// to rely on WAL replay to re-intern keys.
//
// Atomic publication is achieved by assembling the snapshot under
// dir + ".tmp" and renaming it to dir on success — the same protocol
// used by [WriteSnapshotCSR].
//
// When g carries a non-nil [index.Manager] (set via
// [lpg.Graph.SetIndexManager]) with at least one registered index
// that implements [index.Serializer], an indexes/ sub-directory is
// also produced — one file per registered serializable index, each
// referenced from the manifest's Indexes field. Subscribers that do
// not implement [index.Serializer] are skipped (rebuild-on-restart).
//
// Callers that do not need durable LPG labels or properties can keep
// using [WriteSnapshotCSR]; it writes a v1-shaped directory that
// future readers (including this one) accept transparently.
func WriteSnapshotFull[N comparable, W any](dir string, c *csr.CSR[W], g *lpg.Graph[N, W]) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFull").Stop()
	err := WriteSnapshotFullCtx(context.Background(), dir, c, g)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFull.errors", 1)
	}
	return err
}

// WriteSnapshotFullWithMapperCodec is the codec-aware variant of
// [WriteSnapshotFull]: it threads codec (the same [txn.Codec] the store
// uses to serialise node identifiers onto the WAL) into the mapper.bin
// writer so the durable NodeID->key interning table is emitted for ANY
// comparable key type N, not just string. A snapshot written this way
// is self-sufficient on load for every key type, which lets the
// checkpointer truncate the WAL instead of retaining it unboundedly
// (audit gap F3).
//
// For string-keyed graphs the mapper bytes remain byte-identical to the
// version-1 layout (see [WriteMapper]), so this entry point is a safe
// drop-in for the existing [WriteSnapshotFull] on string stores too.
//
// codec must not be nil; pass the store's [txn.Store.Codec].
func WriteSnapshotFullWithMapperCodec[N comparable, W any](
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodec").Stop()
	err := WriteSnapshotFullWithMapperCodecCtx(context.Background(), dir, c, g, codec)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodec.errors", 1)
	}
	return err
}

// WriteSnapshotFullCtx is the context-aware variant of
// [WriteSnapshotFull]. ctx.Err() is checked at five stage boundaries:
// before the CSR write, before the labels write, before the
// properties write, before the manifest write, and before the
// atomic rename. On cancellation the temporary staging directory is
// cleaned up and the wrapped ctx.Err is returned.
func WriteSnapshotFullCtx[N comparable, W any](
	ctx context.Context,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullCtx").Stop()
	// No codec: the mapper is persisted only for string-keyed graphs
	// (the historical v3 behaviour). writeMapperIfStringKeyed performs
	// the string-only probe.
	return captureAndWrite[N, W](ctx, osBackend{}, dir, c, g, nil, nil, nil)
}

// WriteSnapshotFullWithMapperCodecCtx is the context-aware variant of
// [WriteSnapshotFullWithMapperCodec]. ctx cancellation is honoured at
// the same stage boundaries as [WriteSnapshotFullCtx]; the only
// difference is that the mapper.bin component is emitted for every key
// type via codec rather than for string alone.
func WriteSnapshotFullWithMapperCodecCtx[N comparable, W any](
	ctx context.Context,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodecCtx").Stop()
	if codec == nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecCtx.errors", 1)
		return errors.New("snapshot: nil mapper codec")
	}
	return captureAndWrite(ctx, osBackend{}, dir, c, g, codec, nil, nil)
}

// WriteSnapshotFullWithConstraints is [WriteSnapshotFull] plus a durable
// constraints.bin component carrying the engine's schema constraint set. It is
// the snapshot entry point a checkpointer must use when the engine has
// constraints declared: without the component a checkpoint that truncated the
// WAL prefix which first declared a constraint would lose that constraint
// (a durability defect). The mapper.bin component is emitted for string-keyed
// graphs exactly as [WriteSnapshotFull] does.
//
// constraints may be nil or empty, in which case no constraints.bin is written
// and the output is byte-identical to [WriteSnapshotFull].
func WriteSnapshotFullWithConstraints[N comparable, W any](
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	constraints []ConstraintSpec,
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithConstraints").Stop()
	err := captureAndWrite[N, W](context.Background(), osBackend{}, dir, c, g, nil, constraints, nil)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithConstraints.errors", 1)
	}
	return err
}

// WriteSnapshotFullWithConstraintsAndIndexDefs is [WriteSnapshotFullWithConstraints]
// plus a durable indexdefs.bin component carrying the engine's secondary-index
// definition set. It is the snapshot entry point a checkpointer must use when
// the engine has indexes declared: without the component a checkpoint that
// truncated the WAL prefix which first declared an index would lose that index
// definition (a durability defect — #1755), the index analogue of the
// constraints case.
//
// Both constraints and indexDefs may be nil or empty; when both are empty the
// output is byte-identical to [WriteSnapshotFull].
func WriteSnapshotFullWithConstraintsAndIndexDefs[N comparable, W any](
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	constraints []ConstraintSpec,
	indexDefs []IndexDefSpec,
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithConstraintsAndIndexDefs").Stop()
	err := captureAndWrite[N, W](context.Background(), osBackend{}, dir, c, g, nil, constraints, indexDefs)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithConstraintsAndIndexDefs.errors", 1)
	}
	return err
}

// WriteSnapshotFullWithMapperCodecAndConstraints is the codec-aware variant of
// [WriteSnapshotFullWithConstraints]: it threads codec into the mapper.bin
// writer (so the snapshot is self-sufficient for any key type) AND persists the
// constraint set. This is the entry point a checkpointer over a non-string
// store uses when constraints are declared.
//
// codec must not be nil. constraints may be nil or empty (no constraints.bin).
func WriteSnapshotFullWithMapperCodecAndConstraints[N comparable, W any](
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
	constraints []ConstraintSpec,
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints").Stop()
	if codec == nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints.errors", 1)
		return errors.New("snapshot: nil mapper codec")
	}
	err := captureAndWrite(context.Background(), osBackend{}, dir, c, g, codec, constraints, nil)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints.errors", 1)
	}
	return err
}

// WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs is the codec-aware
// variant of [WriteSnapshotFullWithConstraintsAndIndexDefs]: it threads codec
// into the mapper.bin writer (so the snapshot is self-sufficient for any key
// type) AND persists BOTH the constraint set and the index-definition set. This
// is the entry point a checkpointer over a non-string store uses when either
// constraints or indexes are declared.
//
// codec must not be nil. constraints and indexDefs may each be nil or empty
// (the corresponding component is then omitted).
func WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs[N comparable, W any](
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
	constraints []ConstraintSpec,
	indexDefs []IndexDefSpec,
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs").Stop()
	if codec == nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs.errors", 1)
		return errors.New("snapshot: nil mapper codec")
	}
	err := captureAndWrite(context.Background(), osBackend{}, dir, c, g, codec, constraints, indexDefs)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs.errors", 1)
	}
	return err
}

// WriteSnapshotFullWithMapperCodecAndConstraintsFS is the filesystem-seam
// variant of [WriteSnapshotFullWithMapperCodecAndConstraints]: it routes every
// filesystem operation of the snapshot publish through fsys instead of the
// default OS backend. It is the entry point the deterministic-simulation
// harness (internal/sim) uses to back a snapshot with an in-memory disk so it
// can crash mid-publish and during the WAL prefix-truncate that follows a
// checkpoint.
//
// The fsys parameter type ([fileSystem]) is intentionally unexported: an
// external package cannot name it but can still supply a value that satisfies
// it (mirroring wal.OpenWith). Passing osBackend{} reproduces
// [WriteSnapshotFullWithMapperCodecAndConstraints] byte-for-byte.
//
// codec must not be nil. constraints may be nil or empty (no constraints.bin).
func WriteSnapshotFullWithMapperCodecAndConstraintsFS[N comparable, W any](
	fsys fileSystem,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
	constraints []ConstraintSpec,
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints").Stop()
	if codec == nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints.errors", 1)
		return errors.New("snapshot: nil mapper codec")
	}
	err := captureAndWrite(context.Background(), fsys, dir, c, g, codec, constraints, nil)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecAndConstraints.errors", 1)
	}
	return err
}

// WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefsFS is the
// filesystem-seam variant of
// [WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs]: it routes every
// filesystem operation of the snapshot publish through fsys instead of the
// default OS backend, persisting BOTH the constraint set and the
// index-definition set. It is the entry point the deterministic-simulation
// harness (internal/sim) uses to back a snapshot with an in-memory disk so the
// simulated checkpoint also carries durable index definitions (#1755).
//
// The fsys parameter type ([fileSystem]) is intentionally unexported (mirroring
// wal.OpenWith). Passing osBackend{} reproduces
// [WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs] byte-for-byte.
//
// codec must not be nil. constraints and indexDefs may each be nil or empty.
func WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefsFS[N comparable, W any](
	fsys fileSystem,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
	constraints []ConstraintSpec,
	indexDefs []IndexDefSpec,
) error {
	defer metrics.Time("store.snapshot.WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs").Stop()
	if codec == nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs.errors", 1)
		return errors.New("snapshot: nil mapper codec")
	}
	err := captureAndWrite(context.Background(), fsys, dir, c, g, codec, constraints, indexDefs)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullWithMapperCodecConstraintsAndIndexDefs.errors", 1)
	}
	return err
}

// captureAndWrite is the shared implementation behind every
// WriteSnapshotFull* entry point: it takes an atomic [Capture] of g and then
// publishes it. Splitting the two makes the published snapshot an image of ONE
// instant by construction (rmp #2269) — see [Capture].
//
// These entry points capture and publish back to back, with no exclusion of
// their own, so they remain correct exactly where they always were: an offline
// or single-goroutine caller. A caller that publishes CONCURRENTLY with writers
// — the checkpointer — must take the Capture inside its own exclusion window
// and publish it with [WriteCapture]/[WriteCaptureFS].
func captureAndWrite[N comparable, W any](
	ctx context.Context,
	fsys fileSystem,
	dir string,
	c *csr.CSR[W],
	g *lpg.Graph[N, W],
	codec keyEncoder[N],
	constraints []ConstraintSpec,
	indexDefs []IndexDefSpec,
) error {
	// nil instant: this path is the full, one-shot snapshot writer, whose caller holds
	// its own exclusion and is reading the present. The CONCURRENT capture — the one
	// rmp #2310 built — goes through CaptureGraph with a snapshot instead.
	capt, err := CaptureGraph(g, c, codec, nil)
	if err != nil {
		return err
	}
	return writeCaptureCore(ctx, fsys, dir, capt, constraints, indexDefs)
}

// WriteCapture publishes a [Capture] taken earlier to dir, emitting
// constraints.bin from constraints and indexdefs.bin from indexDefs (each
// omitted when nil or empty). It touches no graph: every graph-derived byte was
// fixed when the Capture was taken, which is what lets a checkpointer publish
// LOCK-FREE while still writing a single-instant, transaction-boundary image
// (rmp #2269).
//
// constraints and indexDefs must be captured by the caller in the SAME
// exclusion window as capt, or the published snapshot mixes two instants again.
func WriteCapture[W any](dir string, capt *Capture[W], constraints []ConstraintSpec, indexDefs []IndexDefSpec) error {
	defer metrics.Time("store.snapshot.WriteCapture").Stop()
	err := writeCaptureCore(context.Background(), osBackend{}, dir, capt, constraints, indexDefs)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteCapture.errors", 1)
	}
	return err
}

// WriteCaptureFS is the filesystem-seam variant of [WriteCapture], routing
// every filesystem operation through fsys. It is what the
// deterministic-simulation harness (internal/sim) publishes a capture through
// so it can crash mid-publish. Passing osBackend{} reproduces [WriteCapture]
// byte-for-byte.
//
// The fsys parameter type ([fileSystem]) is intentionally unexported: an
// external package cannot name it but can still supply a value satisfying it
// (mirroring wal.OpenWith).
func WriteCaptureFS[W any](fsys fileSystem, dir string, capt *Capture[W], constraints []ConstraintSpec, indexDefs []IndexDefSpec) error {
	defer metrics.Time("store.snapshot.WriteCapture").Stop()
	err := writeCaptureCore(context.Background(), fsys, dir, capt, constraints, indexDefs)
	if err != nil {
		metrics.IncCounter("store.snapshot.WriteCapture.errors", 1)
	}
	return err
}

// writeCaptureCore lays out and atomically publishes a snapshot directory from
// an already-taken [Capture]. It performs pure I/O: the graph-derived component
// bytes, their sizes and their CRC32Cs were all fixed at capture time, so the
// published bytes are identical to those a streaming write of the same instant
// would have produced, and no component can observe a later state than another.
//
//nolint:gocyclo // snapshot publish: dir prep + CSR + labels + properties + mapper + constraints + indexdefs + manifest + atomic rename + ctx ticks
func writeCaptureCore[W any](
	ctx context.Context,
	fsys fileSystem,
	dir string,
	capt *Capture[W],
	constraints []ConstraintSpec,
	indexDefs []IndexDefSpec,
) error {
	if capt == nil {
		return errors.New("snapshot: nil capture")
	}
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := fsys.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	tmp := dir + ".tmp"
	if err := fsys.RemoveAll(tmp); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := fsys.MkdirAll(tmp, 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// csr.bin
	csrPath := filepath.Join(tmp, CSRFile)
	csrSize, csrCRC, err := writeAndSync(fsys, csrPath, func(w io.Writer) (int64, uint32, error) {
		return WriteCSR(w, capt.csr)
	})
	if err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// labels.bin
	labelsSize, labelsCRC := capt.labels.size, capt.labels.crc
	if err := writeCapturedComponent(fsys, filepath.Join(tmp, LabelsFile), capt.labels); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// properties.bin
	propsSize, propsCRC := capt.properties.size, capt.properties.crc
	if err := writeCapturedComponent(fsys, filepath.Join(tmp, PropertiesFile), capt.properties); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// mapper.bin — durable (NodeID -> natural key) table. Whether it is
	// present was decided at capture time: with a codec it is emitted for
	// every key type; without one, only for string-keyed graphs (the
	// historical v3 behaviour). When absent the snapshot stays v2 and
	// recovery rebuilds the mapper from the WAL — the documented v2 contract.
	mapperSize, mapperCRC, haveMapper := capt.mapper.size, capt.mapper.crc, capt.mapper.present
	if haveMapper {
		if err := writeCapturedComponent(fsys, filepath.Join(tmp, MapperFile), capt.mapper); err != nil {
			_ = fsys.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// tombstones.bin — the durable node-removal set. Emitted ONLY when the
	// graph currently has at least one tombstoned node, so a snapshot of a
	// graph that never deleted a node is byte-identical to one produced
	// before this component existed (no behaviour change for the common
	// case). Without this component a committed node deletion would not
	// survive WAL truncation: the tombstone lives only in memory, and the
	// CSR/labels/properties writers treat a removed node as a live,
	// label-stripped one (the durability defect this fixes).
	tombSize, tombCRC, haveTombstones := capt.tombstones.size, capt.tombstones.crc, capt.tombstones.present
	if haveTombstones {
		if err := writeCapturedComponent(fsys, filepath.Join(tmp, TombstonesFile), capt.tombstones); err != nil {
			_ = fsys.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// edgehandles.bin — the durable per-handle edge metadata (each parallel
	// edge's per-CREATE relationship type and properties keyed by its stable
	// handle). Emitted ONLY when the graph carries at least one such record,
	// so a snapshot of a graph that never used the handle-keyed stores is
	// byte-identical to one produced before this component existed. csr.bin
	// already carries the handle COLUMN that re-stamps each recovered edge's
	// identity; this component restores the per-handle TYPE/PROPERTIES that
	// labels.bin / properties.bin deliberately collapse to a per-pair union.
	// Without it a self-sufficient snapshot would recover parallel edges with
	// the right handles but the wrong (unioned) per-edge type.
	edgeHandleSize, edgeHandleCRC, haveEdgeHandles := capt.edgeHandles.size, capt.edgeHandles.crc, capt.edgeHandles.present
	if haveEdgeHandles {
		if err := writeCapturedComponent(fsys, filepath.Join(tmp, EdgeHandlesFile), capt.edgeHandles); err != nil {
			_ = fsys.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// constraints.bin — the durable schema constraint set. Emitted ONLY when
	// the caller supplies at least one constraint, so a snapshot of a graph
	// with no constraints is byte-identical to one produced before this
	// component existed. Without this component a constraint declared and
	// committed before a checkpoint would not survive WAL truncation: the WAL
	// op that declared it lives only in the truncated prefix, so the
	// checkpoint must carry the constraint forward itself.
	var constraintsSize int64
	var constraintsCRC uint32
	haveConstraints := len(constraints) > 0
	if haveConstraints {
		constraintsPath := filepath.Join(tmp, ConstraintsFile)
		constraintsSize, constraintsCRC, err = writeAndSync(fsys, constraintsPath, func(w io.Writer) (int64, uint32, error) {
			return WriteConstraints(w, constraints)
		})
		if err != nil {
			_ = fsys.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// indexdefs.bin — the durable secondary-index DEFINITION set. Emitted ONLY
	// when the caller supplies at least one index def, so a snapshot of a graph
	// with no indexes is byte-identical to one produced before this component
	// existed. It is DISTINCT from the indexes/ payload directory below: the
	// definition (label/property/kind/name) is the load-bearing component that
	// recovery rebuilds each index from, whereas indexes/<name>.bin is a
	// best-effort payload speed-up. Without this component an index declared and
	// committed before a checkpoint would not survive WAL truncation: the WAL op
	// that declared it lives only in the truncated prefix, so the checkpoint must
	// carry the definition forward itself (#1755, the index analogue of
	// constraints.bin).
	var indexDefsSize int64
	var indexDefsCRC uint32
	haveIndexDefs := len(indexDefs) > 0
	if haveIndexDefs {
		indexDefsPath := filepath.Join(tmp, IndexDefsFile)
		indexDefsSize, indexDefsCRC, err = writeAndSync(fsys, indexDefsPath, func(w io.Writer) (int64, uint32, error) {
			return WriteIndexDefs(w, indexDefs)
		})
		if err != nil {
			_ = fsys.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// indexes/<name>.bin — one file per registered index that
	// implements [index.Serializer]. Subscribers without serializer
	// support are silently skipped (rebuild-on-restart contract).
	var idxEntries []IndexFileEntry
	if len(capt.indexes) > 0 {
		entries, ierr := writeCapturedIndexes(fsys, tmp, capt.indexes)
		if ierr != nil {
			_ = fsys.RemoveAll(tmp)
			metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
			return ierr
		}
		idxEntries = entries
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	// Manifest version is v3 only when mapper.bin was emitted; non-
	// string-keyed graphs continue to produce v2 manifests so existing
	// recovery tests (which compare Manifest.Version against the
	// build's [ManifestVersion]) keep passing for every shape that
	// already worked before this change.
	manifestVersion := manifestVersionV2
	files := []FileEntry{
		{Name: CSRFile, Size: csrSize, CRC32C: csrCRC},
		{Name: LabelsFile, Size: labelsSize, CRC32C: labelsCRC},
		{Name: PropertiesFile, Size: propsSize, CRC32C: propsCRC},
	}
	if haveMapper {
		manifestVersion = ManifestVersion
		files = append(files, FileEntry{Name: MapperFile, Size: mapperSize, CRC32C: mapperCRC})
	}
	// The tombstones.bin entry is additive and does NOT change the manifest
	// version: it is an optional component (like indexes/<name>.bin),
	// present only when the graph has removed nodes. A reader that predates
	// it ignores the unknown file name; this reader restores the set.
	if haveTombstones {
		files = append(files, FileEntry{Name: TombstonesFile, Size: tombSize, CRC32C: tombCRC})
	}
	// The edgehandles.bin entry is likewise additive and does not change the
	// manifest version: present only when the graph carries per-handle edge
	// metadata. A reader that predates it ignores the unknown file name (and
	// reads parallel-edge types from the per-pair union); this reader restores
	// the per-handle types.
	if haveEdgeHandles {
		files = append(files, FileEntry{Name: EdgeHandlesFile, Size: edgeHandleSize, CRC32C: edgeHandleCRC})
	}
	// The constraints.bin entry is likewise additive and does not change the
	// manifest version: present only when constraints are declared. A reader
	// that predates it ignores the unknown file name; this reader restores the
	// set so a checkpoint + WAL truncate does not lose constraints.
	if haveConstraints {
		files = append(files, FileEntry{Name: ConstraintsFile, Size: constraintsSize, CRC32C: constraintsCRC})
	}
	// The indexdefs.bin entry is likewise additive and does not change the
	// manifest version: present only when indexes are declared. A reader that
	// predates it ignores the unknown file name; this reader restores the
	// definition set so a checkpoint + WAL truncate does not lose index
	// definitions (#1755).
	if haveIndexDefs {
		files = append(files, FileEntry{Name: IndexDefsFile, Size: indexDefsSize, CRC32C: indexDefsCRC})
	}

	// Persist the originating graph's directed/multigraph shape so
	// recovery reconstructs the same variant instead of hardcoding one.
	// The full writer always has a capture of the live graph in hand, so every
	// NEW full snapshot carries this; the legacy CSR-only writer cannot (it
	// has no graph) and omits it, falling back to the recovery default.
	cfg := capt.config
	m := Manifest{
		Version:   manifestVersion,
		CreatedAt: time.Now().UTC(),
		// The IMAGE's node count, not the captured CSR's vertex-array length
		// (rmp #2310). Under a concurrent capture the array is sized from the
		// present id space and so has slots for ids interned after the captured
		// instant; [Capture.Order] reports what the components actually carry, which
		// is what a consumer of this manifest gets back. Measured before the change:
		// manifest Order=1128 against Size=563, two slots belonging to a transaction
		// that had not committed at the instant.
		Order:   capt.Order(),
		Size:    capt.csr.Size(),
		Files:   files,
		Indexes: idxEntries,
		// The instant the capture was taken at, so recovery can derive the MVCC
		// clock from an image whose WAL prefix has been truncated away
		// (rmp #2309). Zero when the graph had no MVCC clock, which the
		// omitempty tag then keeps out of the file entirely.
		CommitTS: capt.commitTS,
		GraphConfig: &GraphConfig{
			Directed:   cfg.Directed,
			Multigraph: cfg.Multigraph,
			Weightless: cfg.Weightless,
		},
	}

	manifestPath := filepath.Join(tmp, "manifest.json")
	mf, err := fsys.Create(manifestPath)
	if err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := WriteManifest(mf, m); err != nil {
		_ = mf.Close()          // best-effort: already on error path, WriteManifest err preserved
		_ = fsys.RemoveAll(tmp) // best-effort: tmp dir cleanup, WriteManifest err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close()          // best-effort: already on error path, sync err preserved
		_ = fsys.RemoveAll(tmp) // best-effort: tmp dir cleanup, sync err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	if err := mf.Close(); err != nil {
		_ = fsys.RemoveAll(tmp) // best-effort: tmp dir cleanup, close err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = fsys.RemoveAll(tmp)
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return err
	}
	// Make the staging directory's own inode durable BEFORE the publish
	// rename: each component file was fsync'd individually, but fsync(2)
	// on a file does not guarantee that the dirent linking it into the
	// staging directory is durable. On a filesystem that does not flush a
	// renamed directory's child dirents as part of the rename, a crash
	// after the rename (and after the checkpointer truncates the WAL)
	// could otherwise leave the published snapshot directory present but
	// its components missing or zero-length — total loss of every
	// transaction folded into the checkpoint. The canonical crash-safe
	// ordering is therefore: write+fsync components -> fsync staging dir
	// -> rename -> fsync parent. No-op on platforms without a directory
	// fsync primitive (Windows). See [dirFsync].
	if err := fsys.DirSync(tmp); err != nil {
		_ = fsys.RemoveAll(tmp) // best-effort: staging cleanup, fsync err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: staging dir fsync: %w", err)
	}
	notePublishStep("staging-fsync", tmp)
	// Crash-atomic publish. The previous RemoveAll(dir) -> Rename(tmp, dir)
	// sequence had a fatal window: a crash between the two calls left NO
	// live snapshot on disk while the staging directory was stranded under
	// its .tmp name — and because an earlier checkpoint had already
	// truncated the WAL, recovery would silently rebuild an empty graph
	// (total loss of every checkpointed transaction). Instead, atomically
	// archive the live snapshot to dir+".bak", rename the staging
	// directory into place, and drop the backup only after the publish
	// rename has been made durable. Both renames are atomic, so at every
	// instant at least one complete snapshot exists on disk; recovery
	// promotes a stranded backup back to the live name (see
	// store/recovery).
	bak := dir + ".bak"
	// Clean up a stale backup from a prior interrupted publish (idempotent;
	// recovery may already have promoted or discarded it).
	_ = fsys.RemoveAll(bak) // best-effort: stale backup cleanup
	notePublishStep("archive", bak)
	// Atomically archive the current live snapshot. When dir does not yet
	// exist (first checkpoint), Rename fails with os.ErrNotExist — fine.
	if err := fsys.Rename(dir, bak); err != nil && !errors.Is(err, os.ErrNotExist) {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: archive live snapshot: %w", err)
	}
	notePublishStep("rename", tmp)
	if err := fsys.Rename(tmp, dir); err != nil {
		// Restore: undo the archive so the caller retries against an
		// intact live snapshot.
		_ = fsys.Rename(bak, dir) // best-effort: archive restore, rename err preserved
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: publish rename: %w", err)
	}
	// Make the rename durable: fsync the parent directory so the
	// new directory entry survives a crash within the journal
	// writeback window. No-op on platforms that lack a directory
	// fsync primitive (Windows).
	if err := fsys.ParentDirSync(dir); err != nil {
		metrics.IncCounter("store.snapshot.WriteSnapshotFullCtx.errors", 1)
		return fmt.Errorf("snapshot: publish parent fsync: %w", err)
	}
	// Drop the backup only AFTER the parent-dir fsync: a crash after the
	// publish rename but before the fsync may lose the new dirent, and the
	// backup is then the only surviving copy of the previous snapshot.
	_ = fsys.RemoveAll(bak) // best-effort: happy-path backup cleanup
	return nil
}

// writeCapturedComponent writes one already-serialised component to path and
// fsyncs it. The bytes, size and CRC32C were fixed when the [Capture] was
// taken, so this step cannot observe a different graph state than any other
// component — the property that makes a concurrently-published snapshot an
// image of a single instant (rmp #2269).
//
// It re-declares the capture's own size and CRC to [writeAndSync] rather than
// recomputing them, so a short write surfaces as an error instead of silently
// producing a component whose manifest entry disagrees with its bytes.
func writeCapturedComponent(fsys fileSystem, path string, c component) error {
	_, _, err := writeAndSync(fsys, path, func(w io.Writer) (int64, uint32, error) {
		n, werr := w.Write(c.bytes)
		if werr != nil {
			return 0, 0, werr
		}
		if int64(n) != c.size {
			return 0, 0, fmt.Errorf("snapshot: short write for %s: wrote %d of %d bytes", filepath.Base(path), n, c.size)
		}
		return c.size, c.crc, nil
	})
	return err
}

// writeCapturedIndexes writes the captured indexes/<name>.bin payloads under
// dir and returns their manifest entries. It mirrors [writeIndexesWith]'s
// on-disk layout and durability ordering exactly — one fsync per file, then a
// single fsync of the indexes/ directory so the dirents linking the names to
// their inodes are durable — differing only in that the payloads were
// serialised earlier, at capture time.
func writeCapturedIndexes(fsys fileSystem, dir string, idx []capturedIndex) ([]IndexFileEntry, error) {
	if len(idx) == 0 {
		return nil, nil
	}
	idxDir := filepath.Join(dir, IndexesDir)
	if err := fsys.MkdirAll(idxDir, 0o750); err != nil {
		metrics.IncCounter("store.snapshot.WriteIndexes.errors", 1)
		return nil, err
	}
	out := make([]IndexFileEntry, 0, len(idx))
	for _, ci := range idx {
		filename := filepath.Join(idxDir, ci.name+".bin")
		if err := writeCapturedComponent(fsys, filename, ci.comp); err != nil {
			_ = fsys.RemoveAll(idxDir)
			metrics.IncCounter("store.snapshot.WriteIndexes.errors", 1)
			return nil, fmt.Errorf("snapshot: index %q: %w", ci.name, err)
		}
		out = append(out, IndexFileEntry{Name: ci.name, Size: ci.comp.size, CRC32C: ci.comp.crc})
	}
	if err := fsys.DirSync(idxDir); err != nil {
		_ = fsys.RemoveAll(idxDir)
		metrics.IncCounter("store.snapshot.WriteIndexes.errors", 1)
		return nil, fmt.Errorf("snapshot: fsync indexes dir %q: %w", idxDir, err)
	}
	metrics.IncCounter("store.snapshot.indexes.loaded", uint64(len(out)))
	return out, nil
}

// writeAndSync creates path, hands the file handle to write, fsyncs
// and closes the file. It returns the (size, crc) tuple computed by
// write so the caller can record them in the manifest. The caller's
// path must reside under the staging .tmp directory; the function
// removes the file on any error (best effort) so a half-written
// component never lingers.
func writeAndSync(
	fsys fileSystem,
	path string,
	write func(io.Writer) (int64, uint32, error),
) (size int64, crc uint32, err error) {
	f, err := fsys.Create(path)
	if err != nil {
		return 0, 0, err
	}
	size, crc, werr := write(f)
	if werr != nil {
		_ = f.Close()         // best-effort: already on error path, write err preserved
		_ = fsys.Remove(path) // best-effort: partial file cleanup, write err preserved
		return 0, 0, werr
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()         // best-effort: already on error path, sync err preserved
		_ = fsys.Remove(path) // best-effort: partial file cleanup, sync err preserved
		return 0, 0, err
	}
	if err := f.Close(); err != nil {
		_ = fsys.Remove(path) // best-effort: partial file cleanup, close err preserved
		return 0, 0, err
	}
	return size, crc, nil
}

// LoadSnapshotFull verifies and loads the snapshot rooted at dir,
// returning the CSR, the labels readback, and the properties
// readback. v1 snapshots are accepted transparently: their manifest
// has no labels.bin or properties.bin entry, and the returned
// [LoadedSnapshot.Labels] / [LoadedSnapshot.Properties] are zero
// values (empty tables, no records). v2 snapshots may carry any
// combination of labels.bin and properties.bin; each component is
// CRC-validated only when its manifest entry is present.
//
// CSR CRC verification mirrors [Open]; labels and properties CRC
// verification use the same TeeReader pattern so a corrupted
// component surfaces as [ErrCorrupted].
func LoadSnapshotFull(dir string) (LoadedSnapshot, error) {
	return loadSnapshotFullWith(osBackend{}, dir)
}

// LoadSnapshotFullFS is the filesystem-seam variant of [LoadSnapshotFull]:
// it routes every read of the snapshot through fsys instead of the default
// OS backend. It is the entry point the deterministic-simulation harness
// (internal/sim) uses to load a snapshot backed by an in-memory disk.
// Passing osBackend{} reproduces [LoadSnapshotFull] exactly.
func LoadSnapshotFullFS(fsys fileSystem, dir string) (LoadedSnapshot, error) {
	return loadSnapshotFullWith(fsys, dir)
}

// loadSnapshotFullWith is the seam-threaded implementation behind
// [LoadSnapshotFull] and [LoadSnapshotFullFS]: every filesystem read goes
// through fsys, so the OS backend reproduces the historical behaviour
// byte-for-byte while the simulator can supply an in-memory disk.
func loadSnapshotFullWith(fsys fileSystem, dir string) (LoadedSnapshot, error) {
	defer metrics.Time("store.snapshot.LoadSnapshotFull").Stop()
	manifestPath := filepath.Join(dir, "manifest.json")
	m, err := readManifestFileWith(fsys, manifestPath)
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, err
	}

	csrEntry, labelsEntry, propsEntry, mapperEntry, tombEntry := findEntries(m.Files)
	if csrEntry == nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, fmt.Errorf("%w: manifest missing %q", ErrCorrupted, CSRFile)
	}

	csrParsed, err := readVerifiedCSR(fsys, filepath.Join(dir, CSRFile), csrEntry.CRC32C, csrEntry.Size)
	if err != nil {
		metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
		return LoadedSnapshot{}, err
	}

	var labelsParsed LabelsReadback
	if labelsEntry != nil {
		labelsParsed, err = readVerifiedLabels(fsys, filepath.Join(dir, LabelsFile), labelsEntry.CRC32C, labelsEntry.Size)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	var propsParsed PropertiesReadback
	if propsEntry != nil {
		propsParsed, err = readVerifiedProperties(fsys, filepath.Join(dir, PropertiesFile), propsEntry.CRC32C, propsEntry.Size)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	var mapperParsed MapperReadback
	if mapperEntry != nil {
		mapperPath := filepath.Join(dir, MapperFile)
		ver, verr := peekMapperVersion(fsys, mapperPath)
		if verr != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, verr
		}
		// Version 1 carries string keys (Pairs); version 2 carries
		// codec-encoded key bytes (RawPairs) that the recovery layer
		// decodes with the matching codec. Reading the version up front
		// lets a single LoadSnapshotFull serve both layouts without a
		// codec of its own.
		if ver == mapperFormatVersionCodec {
			mapperParsed, err = readVerifiedMapperBytes(fsys, mapperPath, mapperEntry.CRC32C, mapperEntry.Size)
		} else {
			mapperParsed, err = readVerifiedMapper(fsys, mapperPath, mapperEntry.CRC32C, mapperEntry.Size)
		}
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// tombstones.bin — optional node-removal set. Absent for older
	// snapshots and for any graph that never deleted a node: the readback
	// stays empty (backward compatibility). When present its CRC32C is
	// verified exactly like the other components.
	var tombParsed TombstonesReadback
	if tombEntry != nil {
		tombParsed, err = readVerifiedTombstones(fsys, filepath.Join(dir, TombstonesFile), tombEntry.CRC32C)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// edgehandles.bin — optional per-handle edge metadata. Absent for older
	// snapshots and for any graph that never used the handle-keyed stores:
	// the readback stays empty (backward compatibility). When present its
	// CRC32C is verified exactly like the other components.
	var edgeHandlesParsed EdgeHandlesReadback
	if ehEntry := findEntry(m.Files, EdgeHandlesFile); ehEntry != nil {
		edgeHandlesParsed, err = readVerifiedEdgeHandles(fsys, filepath.Join(dir, EdgeHandlesFile), ehEntry.CRC32C, ehEntry.Size)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// constraints.bin — optional schema constraint set. Absent for older
	// snapshots and for any snapshot taken with no constraints declared: the
	// readback stays empty (backward compatibility). When present its CRC32C
	// is verified exactly like the other components.
	var constraintsParsed ConstraintsReadback
	if ccEntry := findEntry(m.Files, ConstraintsFile); ccEntry != nil {
		constraintsParsed, err = readVerifiedConstraints(fsys, filepath.Join(dir, ConstraintsFile), ccEntry.CRC32C, ccEntry.Size)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// indexdefs.bin — optional secondary-index definition set. Absent for older
	// snapshots and for any snapshot taken with no indexes declared: the
	// readback stays empty (backward compatibility). When present its CRC32C is
	// verified exactly like the other components.
	var indexDefsParsed IndexDefsReadback
	if idEntry := findEntry(m.Files, IndexDefsFile); idEntry != nil {
		indexDefsParsed, err = readVerifiedIndexDefs(fsys, filepath.Join(dir, IndexDefsFile), idEntry.CRC32C, idEntry.Size)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	// indexes/<name>.bin — best-effort load. Corruption surfaces as
	// nil Bytes on the IndexReadback so the recovery path can rebuild
	// from the LPG rather than aborting.
	var idxReadback []IndexReadback
	if len(m.Indexes) > 0 {
		idxReadback, err = loadIndexesWith(fsys, dir, m.Indexes)
		if err != nil {
			metrics.IncCounter("store.snapshot.LoadSnapshotFull.errors", 1)
			return LoadedSnapshot{}, err
		}
	}

	return LoadedSnapshot{
		Manifest:    m,
		CSR:         csrParsed,
		Labels:      labelsParsed,
		Properties:  propsParsed,
		Mapper:      mapperParsed,
		Indexes:     idxReadback,
		Tombstones:  tombParsed,
		EdgeHandles: edgeHandlesParsed,
		Constraints: constraintsParsed,
		IndexDefs:   indexDefsParsed,
	}, nil
}

// findEntry returns a pointer to the FileEntry named name, or nil when
// absent. Used for optional additive components (edgehandles.bin) that are
// not part of the fixed [findEntries] tuple.
func findEntry(files []FileEntry, name string) *FileEntry {
	for k := range files {
		if files[k].Name == name {
			return &files[k]
		}
	}
	return nil
}

// findEntries returns pointers to the csr.bin, labels.bin,
// properties.bin, mapper.bin and tombstones.bin entries in files, or nil
// for any that are absent. The slice is walked once and pointers index
// into the original storage so the caller can inspect them without
// copying.
func findEntries(files []FileEntry) (csrEntry, labelsEntry, propsEntry, mapperEntry, tombEntry *FileEntry) {
	for k := range files {
		switch files[k].Name {
		case CSRFile:
			csrEntry = &files[k]
		case LabelsFile:
			labelsEntry = &files[k]
		case PropertiesFile:
			propsEntry = &files[k]
		case MapperFile:
			mapperEntry = &files[k]
		case TombstonesFile:
			tombEntry = &files[k]
		}
	}
	return csrEntry, labelsEntry, propsEntry, mapperEntry, tombEntry
}

// boundedComponentReader wraps r so at most size bytes are readable when a
// positive manifest size is known. A declared record count in a component
// header can then never drive a structural parser's append loop past the real
// on-disk size: once the bounded reader EOFs, the parser fails fail-stop on
// the truncated record. When size <= 0 (no manifest size recorded) the reader
// is returned unbounded, preserving the prior behaviour. For a valid file the
// recorded size equals the on-disk size, so the CRC computed over the bounded
// stream still covers exactly the component bytes.
func boundedComponentReader(r io.Reader, size int64) io.Reader {
	if size <= 0 {
		return r
	}
	return io.LimitReader(r, size)
}

// safeCSRAllocBound returns the byte bound [readCSRLimited] may assume is
// available for CSR records, derived from the ACTUAL on-disk file size rather
// than the attacker-controlled manifest [FileEntry.Size]. A poisoned store
// directory (a tampered backup or a shared filesystem) can set the manifest
// size to 0 — or any inflated value — which would otherwise collapse
// readCSRLimited's precise bound to the 128 GiB backstop [maxCSRCount] and let
// an 18-byte csr.bin declaring nVertices=1<<34 drive a ~128 GiB eager make()
// and an out-of-memory fatal crash on recovery, before any CRC check
// (CWE-789 / CWE-770). The real file size is a fact of the bytes on disk and
// cannot be inflated by the manifest; content integrity remains CRC-guarded by
// the caller. We return min(manifestSize, realSize) (realSize when the manifest
// size is non-positive), so a legitimate snapshot (manifest size == real size)
// is bounded exactly as before while a lying manifest can only make the bound
// tighter, never exceed the real bytes. The size is obtained by fstat-ing the
// already-open descriptor (f.Stat), not by re-stat'ing the path, so it reflects
// exactly the bytes this handle reads and there is no stat/open TOCTOU: the fd
// was opened with O_NOFOLLOW, and its size cannot diverge from what is read.
func safeCSRAllocBound(f ReadFile, manifestSize int64) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("%w: stat %s: %w", ErrCorrupted, CSRFile, err)
	}
	bound := info.Size()
	if manifestSize > 0 && manifestSize < bound {
		bound = manifestSize
	}
	return bound, nil
}

// readVerifiedCSR opens path, runs the file bytes through CRC32C and
// the structural CSR reader simultaneously, and returns the parsed
// snapshot iff the CRC matches expected. Any disagreement surfaces
// as [ErrCorrupted]. size is the manifest-recorded file size; the allocation
// bound passed to [readCSRLimited] is [safeCSRAllocBound]'s min of it and the
// real on-disk size, so a malicious header declaring more records than the file
// could hold — even under a manifest that lies about the size — is rejected
// before any allocation. The file is opened with O_NOFOLLOW (via
// openSnapshotComponent) so a component symlinked outside the snapshot dir is
// rejected.
func readVerifiedCSR(fsys fileSystem, path string, expected uint32, size int64) (CSRReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return CSRReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	bound, err := safeCSRAllocBound(f, size)
	if err != nil {
		return CSRReadback{}, err
	}
	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := readCSRLimited(tee, bound)
	if err != nil {
		return CSRReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return CSRReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return CSRReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, CSRFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedLabels is the dual of [readVerifiedCSR] for labels.bin. size is
// the manifest-recorded file size; the body reader is bounded to it via
// [io.LimitReader] so a malicious header declaring more records than the file
// could hold cannot drive the parser's append loop past the real on-disk size
// (the structural reader then fails fail-stop on the truncated record).
func readVerifiedLabels(fsys fileSystem, path string, expected uint32, size int64) (LabelsReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return LabelsReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(boundedComponentReader(f, size), hasher)
	parsed, err := ReadLabels(tee)
	if err != nil {
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return LabelsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return LabelsReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, LabelsFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedEdgeHandles is the dual of [readVerifiedCSR] for
// edgehandles.bin. size bounds the body reader (see [readVerifiedLabels]).
func readVerifiedEdgeHandles(fsys fileSystem, path string, expected uint32, size int64) (EdgeHandlesReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return EdgeHandlesReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(boundedComponentReader(f, size), hasher)
	parsed, err := ReadEdgeHandles(tee)
	if err != nil {
		return EdgeHandlesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return EdgeHandlesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return EdgeHandlesReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, EdgeHandlesFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedTombstones is the dual of [readVerifiedCSR] for
// tombstones.bin.
func readVerifiedTombstones(fsys fileSystem, path string, expected uint32) (TombstonesReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return TombstonesReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(f, hasher)
	parsed, err := ReadTombstones(tee)
	if err != nil {
		return TombstonesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return TombstonesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return TombstonesReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, TombstonesFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedConstraints is the dual of [readVerifiedCSR] for constraints.bin.
// size is the manifest-declared component size; the body reader is bounded to it
// so a corrupt/hostile count or length field cannot drive reads beyond the
// declared file before the CRC is verified (mirrors [readVerifiedIndexDefs] /
// [readVerifiedProperties] / [readVerifiedLabels]).
func readVerifiedConstraints(fsys fileSystem, path string, expected uint32, size int64) (ConstraintsReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return ConstraintsReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(boundedComponentReader(f, size), hasher)
	parsed, err := ReadConstraints(tee)
	if err != nil {
		return ConstraintsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return ConstraintsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return ConstraintsReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, ConstraintsFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedIndexDefs is the dual of [readVerifiedCSR] for indexdefs.bin.
// size is the manifest-declared component size; the body reader is bounded to
// it so a corrupt/hostile count or length field cannot drive reads beyond the
// declared file before the CRC is verified (#F-STORE-LOW; mirrors
// [readVerifiedProperties]/[readVerifiedLabels]).
func readVerifiedIndexDefs(fsys fileSystem, path string, expected uint32, size int64) (IndexDefsReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return IndexDefsReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(boundedComponentReader(f, size), hasher)
	parsed, err := ReadIndexDefs(tee)
	if err != nil {
		return IndexDefsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return IndexDefsReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return IndexDefsReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, IndexDefsFile, got, expected)
	}
	return parsed, nil
}

// readVerifiedProperties is the dual of [readVerifiedCSR] for
// properties.bin. size bounds the body reader (see [readVerifiedLabels]).
func readVerifiedProperties(fsys fileSystem, path string, expected uint32, size int64) (PropertiesReadback, error) {
	f, err := fsys.OpenComponent(path)
	if err != nil {
		return PropertiesReadback{}, err
	}
	// best-effort: read-only file, close err is non-actionable for callers.
	defer func() { _ = f.Close() }()

	hasher := crc32.New(castagnoli)
	tee := io.TeeReader(boundedComponentReader(f, size), hasher)
	parsed, err := ReadProperties(tee)
	if err != nil {
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return PropertiesReadback{}, fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	if got := hasher.Sum32(); got != expected {
		return PropertiesReadback{}, fmt.Errorf("%w: %s crc32c=%d want=%d",
			ErrCorrupted, PropertiesFile, got, expected)
	}
	return parsed, nil
}
