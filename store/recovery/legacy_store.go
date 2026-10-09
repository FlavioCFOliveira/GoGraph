package recovery

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// Migration of a legacy store (rmp #3072).
//
// Every graph is a directed multigraph. A store written before that by a graph
// configured undirected ("directed": false) or simple ("multigraph": false)
// declares it in its snapshot manifest, and every transaction in the WAL above
// that snapshot was written under the old semantics:
//
//   - Undirected: every edge was stored twice, once per direction, sharing one
//     handle, and an edge operation named it in either direction. Properties
//     and relationship types were kept under the direction a write named, so
//     the two directions of one edge could hold different values. The loader
//     keeps both directions of the snapshot, and replay applies every operation
//     literally to that two-sided image, with the old engine's semantics: an
//     insertion or removal acts on both directions (see mirrorLegacyEdgeOp),
//     every other edge operation on the direction it names. Once the WAL is
//     replayed, foldLegacyUndirected folds each edge to one relationship,
//     oriented from the lower node id to the higher, and refuses the store
//     with [ErrLegacyMirrorConflict] when the two directions disagree.
//   - Simple: a repeated AddEdge between an existing ordered pair was a no-op,
//     yet its record was written. Replay skips it, so no relationship the old
//     engine never acknowledged appears, but reserves its edge handle as the
//     old engine did, so no later relationship is given it.
//
// Recovery then checkpoints the store before it returns — before any caller can
// write — so the snapshot it publishes is in the current format and every
// legacy WAL frame lies below its redo position. A crash before that checkpoint
// completes leaves the legacy snapshot and WAL as they were (the checkpoint
// publishes its snapshot by an atomic directory swap and reclaims WAL segments
// only after), so the next open repeats the migration from the same state.

// ErrLegacyMigrationUnsupportedFS is returned by a recovery over a filesystem
// backend other than the operating system's when the store must be migrated
// from a legacy undirected or simple-graph snapshot: the migration checkpoint
// writes through the operating-system filesystem only. The store is left
// unchanged.
var ErrLegacyMigrationUnsupportedFS = errors.New("recovery: legacy store migration requires the operating-system filesystem")

// ErrLegacyMigrationNeedsCodec is returned by a clean recovery of a legacy
// undirected or simple-graph store when no node-identifier codec was supplied:
// without one the migration checkpoint cannot write the mapper that makes the
// new snapshot self-sufficient, so the write-ahead log could not be folded
// under it and its legacy records would be replayed again under the new
// semantics. The store is left unchanged.
var ErrLegacyMigrationNeedsCodec = errors.New("recovery: legacy store migration needs a node-identifier codec")

// legacyReplay is the old semantics the WAL above a legacy snapshot was
// written under. A nil *legacyReplay applies none.
type legacyReplay struct {
	undirected bool
	simple     bool
}

// legacyReplayOf returns the replay semantics for loaded: nil unless its
// manifest declares a legacy undirected or simple graph.
func legacyReplayOf(loaded *snapshot.LoadedSnapshot) *legacyReplay {
	if !loaded.Legacy.Any() {
		return nil
	}
	return &legacyReplay{undirected: loaded.Legacy.Undirected, simple: loaded.Legacy.Simple}
}

// decodeEndpoints decodes the two codec-encoded endpoints at the head of op's
// body and returns them with the rest of the body. skip reports, under legacy,
// an edge insertion the simple-graph engine would have ignored because the
// ordered pair already had an edge.
func decodeEndpoints[N comparable, W any](g *lpg.Graph[N, W], op *Op, codec txn.Codec[N], legacy *legacyReplay) (src, dst N, rest []byte, skip bool, err error) {
	if src, rest, err = codec.Decode(op.Body); err != nil {
		return src, dst, rest, false, err
	}
	if dst, rest, err = codec.Decode(rest); err != nil {
		return src, dst, rest, false, err
	}
	if legacy != nil && legacy.simple && isEdgeInsertion(op.Kind) && g.AdjList().HasEdge(src, dst) {
		metrics.IncCounter("store.recovery.legacySimple.duplicateSkipped", 1)
		return src, dst, rest, true, nil
	}
	return src, dst, rest, false, nil
}

// reserveSkippedHandle draws the edge handle the legacy simple-graph engine
// drew for an insertion it then ignored as a duplicate, so no relationship
// recovered or created later is given it: an [txn.OpAddEdgeH] frame names its
// handle, which seeds the handle counter past it, and an [txn.OpAddEdge] or
// [txn.OpAddEdgeWeighted] frame, which names none, consumes the next one, as
// that engine's insertion did. rest is the body after the endpoints; false
// reports an [txn.OpAddEdgeH] body too short to hold its handle.
func reserveSkippedHandle[N comparable, W any](g *lpg.Graph[N, W], kind txn.OpKind, rest []byte) bool {
	if kind != txn.OpAddEdgeH {
		_ = g.NextEdgeHandle() // drawn only to advance the counter, as the old insertion did
		return true
	}
	// An OpAddEdgeH body ends with its 8-byte handle (see edgeHTail).
	if len(rest) < 8 {
		return false
	}
	handle, _ := trailingHandle(rest[len(rest)-8:])
	g.SeedEdgeHandle(handle + 1)
	return true
}

// isEdgeInsertion reports whether kind inserts an edge.
func isEdgeInsertion(kind txn.OpKind) bool {
	switch kind {
	case txn.OpAddEdge, txn.OpAddEdgeWeighted, txn.OpAddEdgeH:
		return true
	}
	return false
}

// legacyCheckpoint is what the migration checkpoint needs from a recovery.
type legacyCheckpoint[N comparable, W any] struct {
	g           *lpg.Graph[N, W]
	codec       txn.Codec[N]
	wcodec      txn.WeightCodec[W]
	constraints []ConstraintRecord
	indexes     []IndexRecord
}

// migrateLegacyStore completes the migration of a legacy store: after a clean
// recovery with legacy semantics it durably checkpoints the recovered graph
// into dir, in the current format, before recovery returns. It does nothing
// when legacy is nil or the recovery was not clean (an unclean store opens
// read-only, so it accepts no write the checkpoint would have to precede).
//
// It is the single-threaded form of [checkpoint.Checkpointer]'s three phases —
// capture, publish, reclaim — written against the snapshot and WAL primitives
// directly, because store/checkpoint's own tests import this package. No
// caller holds the graph yet, so the capture needs no commit serialisation.
// The order is the checkpointer's: the snapshot is published by an atomic
// directory swap and verified readable before the WAL control file records the
// new redo position, and segments are reclaimed only after that. A crash at any
// point therefore leaves either the legacy snapshot with its whole WAL, which
// the next open migrates again, or the new snapshot, above whose redo position
// no legacy frame lies.
func migrateLegacyStore[N comparable, W any](fsys recoveryFS, dir string, legacy *legacyReplay, clean bool, c legacyCheckpoint[N, W]) error {
	if legacy == nil || !clean {
		return nil
	}
	if _, ok := fsys.(osBackend); !ok {
		return ErrLegacyMigrationUnsupportedFS
	}
	if c.codec == nil {
		return ErrLegacyMigrationNeedsCodec
	}
	crashpoint.Breakpoint("recovery.legacy-migration.replayed-pre-checkpoint")
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		return fmt.Errorf("recovery: legacy migration: open WAL: %w", err)
	}
	if err := errors.Join(checkpointLegacy(dir, w, c), w.Close()); err != nil {
		return fmt.Errorf("recovery: legacy migration: %w", err)
	}
	metrics.IncCounter("store.recovery.legacyMigration.checkpointed", 1)
	return nil
}

// checkpointLegacy captures c.g at the WAL's durable end, publishes the image
// as a current-format snapshot, and folds the WAL under it.
func checkpointLegacy[N comparable, W any](dir string, w *wal.Writer, c legacyCheckpoint[N, W]) error {
	watermark := w.DurableOffset()
	at := c.g.BeginCaptureRead()
	cs := csr.BuildFromAdjListAsOf(c.g.AdjList(),
		func(id graph.NodeID) bool { return c.g.NodeExistsAsOf(id, at) },
		at.StartTS(), at.TxID())
	var capt *snapshot.Capture[W]
	var err error
	if c.wcodec != nil {
		capt, err = snapshot.CaptureGraphWithWeightCodec[N, W](c.g, cs, c.codec, c.wcodec, at)
	} else {
		capt, err = snapshot.CaptureGraph[N, W](c.g, cs, c.codec, at)
	}
	c.g.EndRead(at)
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	capt.SetWALPosition(w.StoreID(), watermark)
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteCapture(snapDir, capt, constraintSpecs(c.constraints), indexDefSpecs(c.indexes)); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	// The verification the checkpointer runs before it truncates the log: the
	// image parses and its mapper decodes through the store's codec, and it
	// carries every component recovery needs without the log.
	if err := snapshot.VerifySnapshotReadable[N](snapDir, c.codec); err != nil {
		return fmt.Errorf("published snapshot is not readable, WAL retained: %w", err)
	}
	m, err := snapshot.ReadManifestFile(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("read published manifest, WAL retained: %w", err)
	}
	if !snapshot.SelfSufficient(m.Files, len(c.constraints) > 0, len(c.indexes) > 0) {
		return errors.New("published snapshot is not self-sufficient, WAL retained")
	}
	if err := w.MarkCheckpoint(watermark); err != nil {
		return fmt.Errorf("WAL control file, WAL retained: %w", err)
	}
	crashpoint.Breakpoint("recovery.legacy-migration.published-pre-reclaim")
	if _, err := w.ReclaimSegments(); err != nil {
		return fmt.Errorf("reclaim WAL segments: %w", err)
	}
	return nil
}

// constraintSpecs converts recovered constraint records to snapshot specs.
func constraintSpecs(rs []ConstraintRecord) []snapshot.ConstraintSpec {
	out := make([]snapshot.ConstraintSpec, len(rs))
	for i, r := range rs {
		out[i] = snapshot.ConstraintSpec{Label: r.Label, Property: r.Property, Name: r.Name, Kind: uint8(r.Kind)}
	}
	return out
}

// indexDefSpecs converts recovered index records to snapshot specs.
func indexDefSpecs(rs []IndexRecord) []snapshot.IndexDefSpec {
	out := make([]snapshot.IndexDefSpec, len(rs))
	for i, r := range rs {
		out[i] = snapshot.IndexDefSpec{Name: r.Name, Label: r.Label, Property: r.Property, Kind: uint8(r.Kind)}
	}
	return out
}
