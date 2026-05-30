// Package recovery rebuilds the in-memory graph state from a
// snapshot (when present) plus the WAL tail, and exposes the harness
// used to fuzz crash semantics in tests.
//
// Recovery is the dual of [store/txn.Tx.Commit]: a Tx writes ops to
// the WAL, syncs, then applies them in memory. After a crash any
// op that reached the WAL is replayed during Open; ops that did not
// fsync are dropped — exactly the durability contract documented on
// Tx.Commit.
package recovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/index"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
	"gograph/store/snapshot"
	"gograph/store/txn"
	"gograph/store/wal"
)

// Result reports what Open found.
type Result[N comparable, W any] struct {
	Graph       *lpg.Graph[N, W]
	SnapshotHit bool
	// SnapshotSchemaVersion is the on-disk manifest version of the
	// snapshot that was loaded — 1 for legacy CSR-only directories
	// produced by [snapshot.WriteSnapshotCSR], 2 for directories
	// produced by [snapshot.WriteSnapshotFull]. The field is 0 when
	// no snapshot was found (SnapshotHit == false), so callers can
	// branch on `Result.SnapshotSchemaVersion >= 2` to detect a v2
	// snapshot without first re-reading the manifest from disk.
	SnapshotSchemaVersion int
	// SnapshotLabels reports how many label records the snapshot
	// contributed back into the graph after WAL replay. v1
	// snapshots (CSR-only) leave this at 0; v2 snapshots that
	// include labels.bin populate it.
	SnapshotLabels int
	// SnapshotProperties reports how many typed property records the
	// snapshot contributed back into the graph after WAL replay. v1
	// snapshots and v2 snapshots without a properties.bin leave it
	// at 0; v2 snapshots that include properties.bin populate it.
	SnapshotProperties int
	// SnapshotIndexes reports how many secondary indexes were
	// re-hydrated from indexes/<name>.bin payloads. Indexes whose
	// snapshot file was missing or whose CRC32C did not validate are
	// NOT counted here: they were rebuilt-on-replay instead, which is
	// metered separately via `store.snapshot.indexes.corrupted`.
	SnapshotIndexes int
	WALOps          int
	TailErr         error
}

// Options carries the codecs used by [Open] and [OpenCtx]. Both
// fields are required: Codec serialises endpoint identifiers and
// WeightCodec serialises edge weights for [txn.OpAddEdgeWeighted]
// records.
//
// Options is the recovery-side mirror of [txn.Options]: keeping the
// recovery-argument type local to the recovery package spares callers
// the awkward cross-package import of `txn.Options` purely to feed
// the open path. The two structs share the same shape so callers
// holding a [txn.Options] can pass `Options(opts)` (Go allows the
// conversion because the underlying types match field-for-field).
type Options[N comparable, W any] struct {
	// Codec serialises endpoint identifiers. Must not be nil.
	Codec txn.Codec[N]
	// WeightCodec serialises edge weights. Must not be nil.
	WeightCodec txn.WeightCodec[W]
}

// applySnapshotIndexes feeds every readback in rb into the live
// manager m, calling [index.Serializer.Deserialize] on the matching
// registered index. An index whose readback Bytes are nil (file
// missing or corrupted upstream), or whose Deserialize fails, is
// logged via the standard library [log] package and counted under
// `store.snapshot.indexes.corrupted`; the recovery proceeds with the
// index in its zero state so the LPG remains usable.
//
// Returns the number of indexes successfully re-hydrated.
func applySnapshotIndexes(m *index.Manager, rb []snapshot.IndexReadback) int {
	if m == nil || len(rb) == 0 {
		return 0
	}
	loaded := 0
	for _, r := range rb {
		sub, err := m.GetIndex(r.Name)
		if err != nil {
			// The manager does not know this index — skip; the
			// corresponding file's bytes are dropped.
			log.Printf("recovery: index %q on disk but not registered, ignoring", r.Name)
			continue
		}
		if r.Bytes == nil {
			metrics.IncCounter("store.snapshot.indexes.corrupted", 1)
			log.Printf("recovery: index %q corrupted, will rebuild from LPG", r.Name)
			continue
		}
		ser, ok := sub.(index.Serializer)
		if !ok {
			log.Printf("recovery: index %q does not implement Serializer, skipping", r.Name)
			continue
		}
		if derr := ser.Deserialize(bytes.NewReader(r.Bytes)); derr != nil {
			metrics.IncCounter("store.snapshot.indexes.corrupted", 1)
			log.Printf("recovery: index %q corrupted (%v), will rebuild from LPG", r.Name, derr)
			continue
		}
		loaded++
	}
	if loaded > 0 {
		metrics.IncCounter("store.snapshot.indexes.loaded", uint64(loaded))
	}
	return loaded
}

// Op is the decoded form of a transaction-encoded WAL payload,
// mirroring the encoder in [store/txn].
//
// Only the typed, tagged record shapes are decodable; the legacy v1
// (untagged, fmt.Sprintf-based) frame written by the removed v1 store
// constructor is no longer produced and is rejected at [Decode] (see
// [ErrUnsupportedRecordVersion]).
//
//   - For a v2 (typed, tagged) frame, [Op.Version] is [txn.OpRecordV2] and
//     [Op.Body] carries the opaque codec-encoded endpoints (src then
//     dst, back-to-back, self-delimiting per the installed [txn.Codec]).
//     The caller walks them out of [Op.Body] via the codec.
//   - For a v3 (typed, tagged, transaction-grouped) frame, [Op.Version]
//     is [txn.OpRecordV3], [Op.TxnSeq] carries the per-transaction
//     sequence, and [Op.Body] is byte-identical to the v2 body for the
//     same kind.
//
// [Op.Kind] and [Op.Label] are populated for both decodable versions.
type Op struct {
	Kind    txn.OpKind
	Label   string
	Version uint8
	Body    []byte
	// TxnSeq is the transaction sequence carried by a v3
	// ([txn.OpRecordV3]) frame, grouping the frames of one atomically-
	// committed transaction. It is 0 for v2 frames.
	TxnSeq uint64
}

// ErrUnsupportedRecordVersion is returned by [Decode] for a WAL record
// whose leading version byte is neither [txn.OpRecordV2] nor
// [txn.OpRecordV3]. In practice this is a legacy v1 ([txn.OpRecordV1])
// untagged frame: such frames are no longer produced by the module
// (the v1 store constructor was removed) and the fmt.Sprintf-derived
// endpoints they carried have no inverse through a typed codec, so they
// are rejected explicitly rather than silently mis-decoded. The recovery
// replay loop surfaces the wrapped error via [Result.TailErr] and stops
// at the offending frame.
var ErrUnsupportedRecordVersion = errors.New("recovery: unsupported WAL record version")

// Decode parses one payload back into an [Op]. The parser peeks the
// first byte to select the decoder:
//
//   - 0xFD ([txn.OpRecordV3]) introduces a v3 (tagged, transaction-
//     grouped) record; the body after the txnSeq word is copied into
//     [Op.Body] verbatim for the typed open path.
//   - 0xFE ([txn.OpRecordV2]) introduces a v2 (tagged) record; the
//     remainder up to the uint16-length-prefixed trailing label is
//     copied into [Op.Body] verbatim for the typed open path.
//   - Any other first byte is a legacy v1 ([txn.OpRecordV1]) untagged
//     frame, which the module no longer produces; [Decode] rejects it
//     with [ErrUnsupportedRecordVersion] rather than mis-decoding the
//     non-invertible fmt.Sprintf layout.
func Decode(payload []byte) (Op, error) {
	defer metrics.Time("store.recovery.Decode")()
	if len(payload) < 1 {
		metrics.IncCounter("store.recovery.Decode.errors", 1)
		return Op{}, errors.New("recovery: short payload")
	}
	switch payload[0] {
	case txn.OpRecordV3:
		return decodeV3(payload)
	case txn.OpRecordV2:
		return decodeV2(payload)
	default:
		// A v1 (txn.OpRecordV1) untagged frame, or any unknown version
		// tag. v1 frames are no longer written and are not invertible
		// through a typed codec; reject explicitly.
		metrics.IncCounter("store.recovery.Decode.errors", 1)
		return Op{}, fmt.Errorf("%w: leading byte 0x%02x (legacy %s = 0x%02x is rejected)",
			ErrUnsupportedRecordVersion, payload[0], "txn.OpRecordV1", txn.OpRecordV1)
	}
}

// decodeV3 parses a typed v3 tagged record. Layout:
//
//	uint8  version (txn.OpRecordV3)
//	uint8  kind
//	uint64 txnSeq  (little-endian)
//	...    body, byte-identical to the v2 body for this kind...
//
// The body (everything after the txnSeq word) matches the v2 layout, so it
// is copied verbatim into [Op.Body] and walked by the same typed apply
// path ([applyOpCodec]). An [txn.OpCommit] marker has an empty body; the
// recovery replay loop reads it to apply the buffered transaction.
func decodeV3(payload []byte) (Op, error) {
	if len(payload) < 10 { // version + kind + uint64 txnSeq
		metrics.IncCounter("store.recovery.Decode.errors", 1)
		return Op{}, errors.New("recovery: short v3 payload")
	}
	return Op{
		Version: txn.OpRecordV3,
		Kind:    txn.OpKind(payload[1]),
		TxnSeq:  binary.LittleEndian.Uint64(payload[2:10]),
		Body:    append([]byte(nil), payload[10:]...),
	}, nil
}

// decodeV2 parses a typed tagged record. The codec-encoded endpoints,
// optional weight payload, and trailing label are opaque to this
// layer: locating the boundaries between them requires walking the
// codec (and the weight codec for [txn.OpAddEdgeWeighted]), so
// [Decode] returns the entire post-header region in [Op.Body] and
// leaves [Op.Label] empty. The typed apply path ([applyOpCodec]) is
// responsible for invoking the codec on [Op.Body] to extract src, dst,
// the optional weight, then reading the uint16 label length prefix and
// label bytes from the remaining tail.
func decodeV2(payload []byte) (Op, error) {
	// version + kind = 2 bytes minimum. The body may be empty for
	// hypothetical zero-byte-codec endpoints, so we do not enforce a
	// lower bound beyond the header here; downstream codec.Decode will
	// fail loudly on a truncated body.
	if len(payload) < 2 {
		metrics.IncCounter("store.recovery.Decode.errors", 1)
		return Op{}, errors.New("recovery: short v2 payload")
	}
	op := Op{
		Version: txn.OpRecordV2,
		Kind:    txn.OpKind(payload[1]),
		Body:    append([]byte(nil), payload[2:]...),
	}
	return op, nil
}

// Open opens the store at dir for graphs keyed by N values and
// weighted by W values, using opts.Codec for endpoint identifiers
// and opts.WeightCodec for [txn.OpAddEdgeWeighted] frames. It is the
// canonical recovery entry point.
//
// Open loads any snapshot under dir/snapshot (v1 or v2; CSR-only or
// CSR + labels + properties + indexes), then replays the WAL at
// dir/wal applying each op into the live graph. Labels, properties,
// and registered indexes carried by a v2 snapshot are reconstructed
// into the returned [Result.Graph] when the LPG has a Manager wired
// before the call returns (see [TestRecovery_IndexesSurviveRestart_WiredEarly]
// for the recommended startup ordering).
//
// Both opts.Codec and opts.WeightCodec must be non-nil. Pre-T8 WALs
// that contain only [txn.OpAddEdge] frames replay identically to the
// codec-only path: the apply phase writes the zero value of W for
// each unweighted record. Mixed WALs preserve weights for
// [txn.OpAddEdgeWeighted] frames and apply zero for the unweighted
// ones — the forward-compatibility contract documented at
// [txn.NewStoreWithOptions].
//
// Open is safe to call on a dir that contains only a snapshot, only
// a WAL, both, or neither: missing components are tolerated and the
// returned [Result.Graph] is a fresh empty graph when neither exists.
func Open[N comparable, W any](dir string, opts Options[N, W]) (Result[N, W], error) {
	defer metrics.Time("store.recovery.Open")()
	res, err := OpenCtx[N, W](context.Background(), dir, opts)
	if err != nil {
		metrics.IncCounter("store.recovery.Open.errors", 1)
	}
	return res, err
}

// OpenCtx is the context-aware variant of [Open]. ctx.Err() is
// checked at the snapshot-load boundary and at every 4096 WAL frames
// replayed; on cancellation the function returns the partially-
// recovered Result paired with the wrapped ctx.Err.
func OpenCtx[N comparable, W any](ctx context.Context, dir string, opts Options[N, W]) (Result[N, W], error) {
	defer metrics.Time("store.recovery.OpenCtx")()
	if opts.Codec == nil {
		metrics.IncCounter("store.recovery.OpenCtx.errors", 1)
		return Result[N, W]{}, errors.New("recovery: nil codec")
	}
	if opts.WeightCodec == nil {
		metrics.IncCounter("store.recovery.OpenCtx.errors", 1)
		return Result[N, W]{}, errors.New("recovery: nil weight codec")
	}
	res, err := openCodec[N, W](ctx, dir, opts.Codec, opts.WeightCodec)
	if err != nil {
		metrics.IncCounter("store.recovery.OpenCtx.errors", 1)
	}
	return res, err
}

// openCodec is the shared core of [Open] and [OpenCtx]. wcodec is nil
// for the codec-only path; when non-nil the function honours
// [txn.OpAddEdgeWeighted] records by decoding the typed weight payload
// before applying.
//
//nolint:gocyclo // recovery: snapshot probe + labels load + WAL open + per-frame decode + per-frame apply + ctx ticks + labels apply
func openCodec[N comparable, W any](
	ctx context.Context,
	dir string,
	codec txn.Codec[N],
	wcodec txn.WeightCodec[W],
) (Result[N, W], error) {
	g := lpg.New[N, W](adjlist.Config{Directed: true})
	res := Result[N, W]{Graph: g}

	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.recovery.openCodec.errors", 1)
		return res, err
	}
	snapDir := filepath.Join(dir, "snapshot")
	var snapLabels snapshot.LabelsReadback
	var snapProps snapshot.PropertiesReadback
	var snapIndexes []snapshot.IndexReadback
	var haveSnapLabels, haveSnapProps bool
	if _, err := os.Stat(filepath.Join(snapDir, "manifest.json")); err == nil {
		loaded, err := snapshot.LoadSnapshotFull(snapDir)
		if err != nil {
			metrics.IncCounter("store.recovery.openCodec.errors", 1)
			return res, fmt.Errorf("recovery: snapshot open: %w", err)
		}
		res.SnapshotHit = true
		res.SnapshotSchemaVersion = loaded.Manifest.Version
		snapLabels = loaded.Labels
		snapProps = loaded.Properties
		snapIndexes = loaded.Indexes
		haveSnapLabels = len(loaded.Labels.NodeLabels) > 0 || len(loaded.Labels.EdgeLabels) > 0
		haveSnapProps = len(loaded.Properties.NodeProperties) > 0 || len(loaded.Properties.EdgeProperties) > 0

		// v3 snapshot: the mapper.bin payload re-seeds the in-memory
		// interning table BEFORE WAL replay so the rest of the load
		// chain (CSR apply, labels apply, properties apply, WAL apply)
		// finds every NodeID already resolved. A version-1 (string)
		// mapper.bin lands in Pairs; a version-2 (codec) mapper.bin lands
		// in RawPairs and is decoded through the supplied codec. v2
		// snapshots without a mapper produce an empty readback here and
		// the original WAL-replay-only reconstruction path applies.
		haveMapper := len(loaded.Mapper.Pairs) > 0 || len(loaded.Mapper.RawPairs) > 0
		if haveMapper {
			if len(loaded.Mapper.RawPairs) > 0 {
				if err := snapshot.ApplyMapperToGraphWithCodec(g, loaded.Mapper, codec); err != nil {
					metrics.IncCounter("store.recovery.openCodec.errors", 1)
					return res, fmt.Errorf("recovery: apply snapshot mapper: %w", err)
				}
			} else if err := snapshot.ApplyMapperToGraph(g, loaded.Mapper); err != nil {
				metrics.IncCounter("store.recovery.openCodec.errors", 1)
				return res, fmt.Errorf("recovery: apply snapshot mapper: %w", err)
			}
			// With the mapper restored, the CSR adjacency can be
			// applied directly — no WAL prefix needed. AddEdge is
			// idempotent against a freshly-restored mapper because each
			// (src, dst) appears at most once in the CSR snapshot.
			if err := snapshot.ApplyCSRToGraph(g, &loaded.CSR); err != nil {
				metrics.IncCounter("store.recovery.openCodec.errors", 1)
				return res, fmt.Errorf("recovery: apply snapshot CSR: %w", err)
			}
		}
	}

	walPath := filepath.Join(dir, "wal")
	walMissing := false
	if _, err := os.Stat(walPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			metrics.IncCounter("store.recovery.openCodec.errors", 1)
			return res, err
		}
		walMissing = true
	}
	if !walMissing {
		r, err := wal.OpenReader(walPath)
		if err != nil {
			metrics.IncCounter("store.recovery.openCodec.errors", 1)
			return res, err
		}
		// best-effort: read-only WAL reader, close err is non-actionable for callers.
		defer func() { _ = r.Close() }()
		// pending buffers the ops of an in-flight v3 transaction until its
		// OpCommit marker is read. The store serialises commits (single
		// writer), so a transaction's frames are contiguous and never
		// interleave with another's; an un-marked tail at end of input is
		// an incomplete transaction and is discarded for atomicity (F1).
		var pending []Op
		for f := range r.Frames() {
			if res.WALOps&0xFFF == 0 {
				if err := ctx.Err(); err != nil {
					metrics.IncCounter("store.recovery.openCodec.errors", 1)
					return res, err
				}
			}
			op, derr := Decode(f.Payload)
			if derr != nil {
				res.TailErr = derr
				break
			}
			if op.Version == txn.OpRecordV3 {
				if op.Kind != txn.OpCommit {
					pending = append(pending, op)
					continue
				}
				// Durable transaction boundary: apply the buffered ops as a
				// unit. A crash that tore the batch never reaches a marker
				// with a partial set — the marker is the last frame written.
				ok := true
				for i := range pending {
					if !applyOpCodec(g, &pending[i], codec, wcodec) {
						ok = false
						break
					}
					res.WALOps++
				}
				pending = pending[:0]
				if !ok {
					res.TailErr = errors.New("recovery: corrupt op inside a committed v3 transaction")
					break
				}
				continue
			}
			// v2 frame: self-committing (one frame is one transaction). v1
			// frames never reach here — Decode rejects them upstream with
			// ErrUnsupportedRecordVersion.
			if !applyOpCodec(g, &op, codec, wcodec) {
				// A malformed v2 body (truncated endpoints, missing or
				// overflowing trailing label/key length) failed to decode
				// through the codec; stop replay so callers see the cut-off
				// boundary deterministically.
				res.TailErr = errors.New("recovery: v2 frame is not decodable through the supplied codec")
				break
			}
			res.WALOps++
		}
		if tErr := r.TailError(); tErr != nil {
			res.TailErr = tErr
		}
	}

	// Apply snapshot-side labels after the WAL is fully applied so the
	// mapper has every node interned that the WAL referenced. Snapshot
	// label records whose NodeIDs the mapper cannot resolve are dropped
	// (with metric) by ApplyLabelsToGraph, not surfaced as an error:
	// this keeps recovery resilient against snapshot-without-WAL flows.
	if haveSnapLabels {
		if err := snapshot.ApplyLabelsToGraph(g, snapLabels); err != nil {
			metrics.IncCounter("store.recovery.openCodec.errors", 1)
			return res, fmt.Errorf("recovery: apply snapshot labels: %w", err)
		}
		res.SnapshotLabels = len(snapLabels.NodeLabels) + len(snapLabels.EdgeLabels)
	}
	// Apply snapshot-side properties after labels for symmetry with
	// the write order. Resilient skip-on-unresolved semantics mirror
	// the labels apply path.
	if haveSnapProps {
		if err := snapshot.ApplyPropertiesToGraph(g, snapProps); err != nil {
			metrics.IncCounter("store.recovery.openCodec.errors", 1)
			return res, fmt.Errorf("recovery: apply snapshot properties: %w", err)
		}
		res.SnapshotProperties = len(snapProps.NodeProperties) + len(snapProps.EdgeProperties)
	}
	// Secondary indexes (label / hash / btree) are re-hydrated last so
	// the live graph is fully populated when we ask the Manager for the
	// matching subscribers. Indexes are only re-hydrated when the LPG
	// has a Manager wired in; absent that, the snapshot bytes are
	// dropped (the index is rebuilt lazily on the next mutation pass).
	if len(snapIndexes) > 0 {
		res.SnapshotIndexes = applySnapshotIndexes(g.IndexManager(), snapIndexes)
	}
	return res, nil
}

// applyOpCodec applies a decoded op into g via codec. It returns
// true if the op was applied. It returns false for any op whose
// [Op.Version] is not a typed tag ([txn.OpRecordV2] / [txn.OpRecordV3])
// and for a typed frame whose codec-encoded body cannot be walked
// (truncated endpoints, missing or overflowing trailing label/key
// length); the surrounding replay loop surfaces such a frame as a tail
// error so callers see the cut-off boundary deterministically. Legacy
// v1 frames never reach here — [Decode] rejects them with
// [ErrUnsupportedRecordVersion] before apply.
//
// When wcodec is non-nil and the op is [txn.OpAddEdgeWeighted], the
// typed weight payload between codec.dst and the trailing label is
// decoded and applied to the graph. When wcodec is nil and the op is
// [txn.OpAddEdgeWeighted], the apply falls back to a zero weight and
// the `store.recovery.applyOp.fallbackZeroWeight` counter is
// incremented.
//
// The Op is taken by pointer to keep the inner recovery loop
// allocation-free; the function does not mutate op.
//
//nolint:gocyclo // recovery: per-frame walk through codec, optional weight codec, trailing label/key, and property value
func applyOpCodec[N comparable, W any](
	g *lpg.Graph[N, W],
	op *Op,
	codec txn.Codec[N],
	wcodec txn.WeightCodec[W],
) bool {
	// v2 and v3 frames share the same codec-encoded body; v3 differs only
	// in the envelope header (txnSeq) which Decode already stripped into
	// Op.Body. Any other version (e.g. a zero-value Op) is not invertible
	// through a typed codec and is rejected defensively; legacy v1 frames
	// are already rejected upstream by Decode.
	if op.Version != txn.OpRecordV2 && op.Version != txn.OpRecordV3 {
		return false
	}
	src, rest, err := codec.Decode(op.Body)
	if err != nil {
		return false
	}
	dst, rest, err := codec.Decode(rest)
	if err != nil {
		return false
	}

	switch op.Kind {
	case txn.OpAddEdgeWeighted:
		var weight W
		if wcodec != nil {
			weight, rest, err = wcodec.Decode(rest)
			if err != nil {
				return false
			}
		} else {
			metrics.IncCounter("store.recovery.applyOp.fallbackZeroWeight", 1)
			return false
		}
		// consume trailing uint16 label (always 0 for AddEdge)
		if len(rest) < 2 {
			return false
		}
		if err := g.AddEdge(src, dst, weight); err != nil {
			metrics.IncCounter("store.recovery.applyOp.addEdgeErrors", 1)
			return false
		}

	case txn.OpAddEdge:
		// Validate the trailing uint16 label-length prefix. AddEdge does not
		// use the label, but a malformed length (claiming more bytes than remain)
		// indicates a corrupted frame and must not be applied.
		if len(rest) < 2 {
			return false
		}
		n := binary.LittleEndian.Uint16(rest)
		rest = rest[2:]
		if uint64(len(rest)) < uint64(n) {
			return false
		}
		var zero W
		if err := g.AddEdge(src, dst, zero); err != nil {
			metrics.IncCounter("store.recovery.applyOp.addEdgeErrors", 1)
			return false
		}

	case txn.OpSetNodeLabel, txn.OpRemoveNodeLabel, txn.OpSetEdgeLabel,
		txn.OpAddNode, txn.OpRemoveNode, txn.OpRemoveEdge:
		// All of these have uint16 label length + label bytes at this point.
		if len(rest) < 2 {
			return false
		}
		n := binary.LittleEndian.Uint16(rest)
		rest = rest[2:]
		if uint64(len(rest)) < uint64(n) {
			return false
		}
		label := string(rest[:n])
		switch op.Kind {
		case txn.OpAddNode:
			if err := g.AddNode(src); err != nil {
				metrics.IncCounter("store.recovery.applyOp.addNodeErrors", 1)
				return false
			}
		case txn.OpRemoveNode:
			for _, lbl := range g.NodeLabels(src) {
				g.RemoveNodeLabel(src, lbl)
			}
			for k := range g.NodeProperties(src) {
				g.DelNodeProperty(src, k)
			}
		case txn.OpRemoveNodeLabel:
			g.RemoveNodeLabel(src, label)
		case txn.OpSetNodeLabel:
			if err := g.SetNodeLabel(src, label); err != nil {
				metrics.IncCounter("store.recovery.applyOp.setNodeLabelErrors", 1)
				return false
			}
		case txn.OpSetEdgeLabel:
			g.SetEdgeLabel(src, dst, label)
		case txn.OpRemoveEdge:
			g.AdjList().RemoveEdge(src, dst)
		}

	case txn.OpSetNodeProperty, txn.OpDelNodeProperty,
		txn.OpSetEdgeProperty, txn.OpDelEdgeProperty:
		// uint16 key length + key bytes [+ property value for Set ops]
		if len(rest) < 2 {
			return false
		}
		kLen := binary.LittleEndian.Uint16(rest)
		rest = rest[2:]
		if uint64(len(rest)) < uint64(kLen) {
			return false
		}
		key := string(rest[:kLen])
		rest = rest[kLen:]
		switch op.Kind {
		case txn.OpSetNodeProperty:
			val, _, verr := decodeRecoveryPropertyValue(rest)
			if verr != nil {
				return false
			}
			if err := g.SetNodeProperty(src, key, val); err != nil {
				metrics.IncCounter("store.recovery.applyOp.setNodePropertyErrors", 1)
				return false
			}
		case txn.OpDelNodeProperty:
			g.DelNodeProperty(src, key)
		case txn.OpSetEdgeProperty:
			val, _, verr := decodeRecoveryPropertyValue(rest)
			if verr != nil {
				return false
			}
			_ = g.SetEdgeProperty(src, dst, key, val) //nolint:errcheck // no schema validator during WAL replay
		case txn.OpDelEdgeProperty:
			g.DelEdgeProperty(src, dst, key)
		}
	}
	return true
}

// decodeRecoveryPropertyValue parses a [lpg.PropertyValue] from the head of
// buf using the same encoding written by txn.encodePropertyValue.
func decodeRecoveryPropertyValue(buf []byte) (lpg.PropertyValue, []byte, error) {
	if len(buf) < 1 {
		return lpg.PropertyValue{}, buf, errors.New("recovery: short property value (missing kind)")
	}
	kind := lpg.PropertyKind(buf[0])
	buf = buf[1:]
	switch kind {
	case lpg.PropString:
		return decodeRecoveryStringProp(buf)
	case lpg.PropInt64:
		return decodeRecoveryInt64Prop(buf)
	case lpg.PropFloat64:
		return decodeRecoveryFloat64Prop(buf)
	case lpg.PropBool:
		return decodeRecoveryBoolProp(buf)
	case lpg.PropTime:
		return decodeRecoveryTimeProp(buf)
	case lpg.PropBytes:
		return decodeRecoveryBytesProp(buf)
	case lpg.PropList:
		return decodeRecoveryListProp(buf)
	default:
		return lpg.PropertyValue{}, buf, errors.New("recovery: unknown property kind")
	}
}

// decodeRecoveryListProp parses a PropList value from buf (the kind byte has
// already been consumed by [decodeRecoveryPropertyValue]).
// Format matches [txn.encodeTxnListProp]:
//
//	uint32 LE element-count
//	element-count × ( uint8 elem-kind | uint32 elem-payload-len | [elem-payload-len]byte elem-payload )
func decodeRecoveryListProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	if len(buf) < 4 {
		return lpg.PropertyValue{}, buf, errors.New("recovery: PropList: short element count")
	}
	count := binary.LittleEndian.Uint32(buf)
	buf = buf[4:]
	elems := make([]lpg.PropertyValue, 0, count)
	for i := uint32(0); i < count; i++ {
		if len(buf) < 5 { // kind(1) + payloadLen(4)
			return lpg.PropertyValue{}, buf,
				fmt.Errorf("recovery: PropList: truncated element header at index %d", i)
		}
		elemKind := lpg.PropertyKind(buf[0])
		payloadLen := binary.LittleEndian.Uint32(buf[1:5])
		buf = buf[5:]
		if uint64(len(buf)) < uint64(payloadLen) {
			return lpg.PropertyValue{}, buf,
				fmt.Errorf("recovery: PropList: truncated element body at index %d", i)
		}
		payload := buf[:payloadLen]
		buf = buf[payloadLen:]
		elem, err := decodeRecoveryListElement(elemKind, payload)
		if err != nil {
			return lpg.PropertyValue{}, buf,
				fmt.Errorf("recovery: PropList: element %d: %w", i, err)
		}
		elems = append(elems, elem)
	}
	return lpg.ListValue(elems), buf, nil
}

// decodeRecoveryListElement decodes a single list element from its raw payload.
// The kind byte has already been consumed and the payload extracted by
// [decodeRecoveryListProp].
func decodeRecoveryListElement(kind lpg.PropertyKind, payload []byte) (lpg.PropertyValue, error) {
	switch kind {
	case lpg.PropString:
		return lpg.StringValue(string(payload)), nil
	case lpg.PropInt64:
		i, n := binary.Varint(payload)
		if n <= 0 {
			return lpg.PropertyValue{}, errors.New("recovery: PropList element: varint decode failed")
		}
		return lpg.Int64Value(i), nil
	case lpg.PropFloat64:
		if len(payload) < 8 {
			return lpg.PropertyValue{}, errors.New("recovery: PropList element: short float64")
		}
		return lpg.Float64Value(math.Float64frombits(binary.LittleEndian.Uint64(payload))), nil
	case lpg.PropBool:
		if len(payload) < 1 {
			return lpg.PropertyValue{}, errors.New("recovery: PropList element: short bool")
		}
		return lpg.BoolValue(payload[0] != 0), nil
	case lpg.PropTime:
		ns, n := binary.Varint(payload)
		if n <= 0 {
			return lpg.PropertyValue{}, errors.New("recovery: PropList element: time varint decode failed")
		}
		return lpg.TimeValue(time.Unix(0, ns).UTC()), nil
	case lpg.PropBytes:
		cp := make([]byte, len(payload))
		copy(cp, payload)
		return lpg.BytesValue(cp), nil
	default:
		return lpg.PropertyValue{}, fmt.Errorf("recovery: PropList element: unknown kind %d", kind)
	}
}

// decodeRecoveryLengthPrefixed reads a uint32 length followed by
// length bytes; returns the body and the remainder. shared by
// String and Bytes decoders. errTag is mixed into the diagnostic
// (e.g. "string" or "bytes") so the caller's typed error keeps its
// breadcrumb.
func decodeRecoveryLengthPrefixed(buf []byte, errTag string) (body, rest []byte, err error) {
	if len(buf) < 4 {
		return nil, buf, fmt.Errorf("recovery: short %s property (missing length)", errTag)
	}
	n := binary.LittleEndian.Uint32(buf)
	buf = buf[4:]
	if uint64(len(buf)) < uint64(n) {
		return nil, buf, fmt.Errorf("recovery: short %s property body", errTag)
	}
	return buf[:n], buf[n:], nil
}

func decodeRecoveryStringProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	body, rest, err := decodeRecoveryLengthPrefixed(buf, "string")
	if err != nil {
		return lpg.PropertyValue{}, rest, err
	}
	return lpg.StringValue(string(body)), rest, nil
}

func decodeRecoveryBytesProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	body, rest, err := decodeRecoveryLengthPrefixed(buf, "bytes")
	if err != nil {
		return lpg.PropertyValue{}, rest, err
	}
	bs := make([]byte, len(body))
	copy(bs, body)
	return lpg.BytesValue(bs), rest, nil
}

func decodeRecoveryInt64Prop(buf []byte) (lpg.PropertyValue, []byte, error) {
	x, n := binary.Varint(buf)
	if n <= 0 {
		return lpg.PropertyValue{}, buf, errors.New("recovery: short int64 property")
	}
	return lpg.Int64Value(x), buf[n:], nil
}

func decodeRecoveryFloat64Prop(buf []byte) (lpg.PropertyValue, []byte, error) {
	if len(buf) < 8 {
		return lpg.PropertyValue{}, buf, errors.New("recovery: short float64 property")
	}
	bits := binary.LittleEndian.Uint64(buf[:8])
	return lpg.Float64Value(math.Float64frombits(bits)), buf[8:], nil
}

func decodeRecoveryBoolProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	if len(buf) < 1 {
		return lpg.PropertyValue{}, buf, errors.New("recovery: short bool property")
	}
	return lpg.BoolValue(buf[0] != 0), buf[1:], nil
}

func decodeRecoveryTimeProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	nanos, n := binary.Varint(buf)
	if n <= 0 {
		return lpg.PropertyValue{}, buf, errors.New("recovery: short time property")
	}
	return lpg.TimeValue(time.Unix(0, nanos).UTC()), buf[n:], nil
}
