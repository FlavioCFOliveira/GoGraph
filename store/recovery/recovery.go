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
	"sort"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
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
	// SnapshotTombstones reports how many node tombstones were restored
	// from the snapshot's tombstones.bin component before WAL replay. It is
	// 0 for snapshots without the component (older snapshots, or any graph
	// that never removed a node) and for the non-self-sufficient (v2) path,
	// where tombstones are reconstructed by replaying OpRemoveNode instead.
	SnapshotTombstones int
	// Constraints reports the durable schema constraints recovered for the
	// graph: the set declared in the snapshot's constraints.bin component
	// (the checkpoint-survival path) reconciled with the CREATE/DROP
	// CONSTRAINT ops replayed from the WAL tail (the post-snapshot path), so
	// the result is the constraint set as of the last durable commit. The
	// engine re-registers these on open and re-seeds each UNIQUE value-set by
	// scanning [Result.Graph]; constraint definitions are engine schema, not
	// graph topology, so they are surfaced here rather than applied to Graph.
	// The slice is deterministically ordered (kind, label, property, name).
	Constraints []ConstraintRecord
	// Indexes reports the durable index definitions recovered from the WAL
	// ([txn.OpCreateIndex] / [txn.OpDropIndex] ops). The engine re-registers
	// and re-backfills these on open so a user-created index survives a crash
	// and a restart (Durability). Index definitions are engine schema, not
	// graph topology, so they are surfaced here rather than applied to Graph.
	// The slice is deterministically ordered (by name).
	Indexes []IndexRecord
	WALOps  int
	// TailErr reports why WAL replay stopped before the end of the file,
	// or nil when every frame was consumed at a clean EOF. Two outcomes
	// are possible:
	//
	//   - A benign torn tail ([wal.ErrTornFrame]) — the normal
	//     crash-after-the-last-fsync case, or a CRC-valid but unparseable
	//     trailing frame from an interrupted write. The committed prefix is
	//     fully recovered and [Open]/[OpenCtx] return a nil function error;
	//     [Result.IsClean] reports true.
	//   - Genuine corruption inside an already-durable frame
	//     ([wal.ErrCRCMismatch], [wal.ErrBadMagic],
	//     [wal.ErrUnsupportedVersion], [wal.ErrFrameTooLarge],
	//     [ErrUnsupportedRecordVersion], or [ErrTransactionTooLarge]). The
	//     committed prefix up to the bad frame is still placed in Graph for
	//     diagnostics, but the same error is returned as the function error
	//     and [Result.IsClean] reports false, so a caller cannot accidentally
	//     append to a corrupt WAL.
	TailErr error
	// WALTailOffset is the byte offset of the last durable frame boundary
	// in the WAL. It equals the WAL file size when every frame was
	// consumed cleanly, and the boundary of the last fully-consumed frame
	// when replay stopped early — for a benign torn tail
	// ([wal.ErrTornFrame]) that is the start of the torn frame. It is 0
	// when the directory has no WAL file. Callers that reopen the WAL for
	// append must truncate the file to this offset first so new frames are
	// not written after torn-tail junk that every subsequent reader would
	// stop at ([wal.Open] performs that truncation itself for benign torn
	// tails).
	WALTailOffset int64
}

// IsClean reports whether recovery completed without encountering genuine
// on-disk corruption. It is true when [Result.TailErr] is nil or a benign
// torn tail ([wal.ErrTornFrame]) — the states from which it is safe to
// reopen the WAL for append — and false when TailErr is a
// genuine-corruption sentinel ([wal.ErrCRCMismatch], [wal.ErrBadMagic],
// [wal.ErrUnsupportedVersion], [wal.ErrFrameTooLarge],
// [ErrUnsupportedRecordVersion], or [ErrTransactionTooLarge]).
//
// IsClean is the exact complement of the function-error contract: [Open]
// and [OpenCtx] return a non-nil error if and only if IsClean is false.
// Callers that recover-then-append should branch on IsClean (or on the
// returned error) and refuse to append when it is false; appending to a
// corrupt WAL would permanently embed the corruption and silently drop
// every committed op that followed the bad frame.
func (r Result[N, W]) IsClean() bool {
	return !tailErrIsCorruption(r.TailErr)
}

// tailErrIsCorruption classifies a [Result.TailErr] value as genuine
// on-disk corruption (true) versus a benign / absent stop condition
// (false). It mirrors [wal.Reader.Replay], which surfaces every WAL-reader
// error except [wal.ErrTornFrame] as a hard error, and additionally treats
// recovery's own [ErrUnsupportedRecordVersion] and [ErrTransactionTooLarge]
// as corruption.
//
// A nil error, a torn tail, and the CRC-valid-but-unparseable trailing-frame
// markers raised by the codec apply path (a truncated v2 body, a missing
// trailing label/key length) are all benign: each represents an interrupted
// final write whose committed prefix is intact, identical to the durability
// contract documented on [store/txn.Tx.Commit].
func tailErrIsCorruption(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, wal.ErrTornFrame):
		return false
	case errors.Is(err, wal.ErrCRCMismatch),
		errors.Is(err, wal.ErrBadMagic),
		errors.Is(err, wal.ErrUnsupportedVersion),
		errors.Is(err, wal.ErrFrameTooLarge),
		errors.Is(err, ErrUnsupportedRecordVersion),
		errors.Is(err, ErrTransactionTooLarge):
		return true
	default:
		// CRC-valid-but-unparseable trailing frame (truncated v2 body,
		// missing label/key length, short payload): benign, same as a torn
		// tail — the committed prefix is intact.
		return false
	}
}

// Options carries the codecs used by [Open] and [OpenCtx] plus the
// per-transaction op cap. Codec (endpoint identifiers) is required and must
// not be nil; WeightCodec (edge weights for [txn.OpAddEdgeWeighted] and
// weighted [txn.OpAddEdgeH] records) mirrors the producer store — nil for a
// codec-only store ([txn.NewStoreWithCodec]), non-nil for a weight-codec
// store ([txn.NewStoreWithOptions]); MaxTxnOps is optional and defaults to a
// finite bound.
//
// Keeping the recovery-argument type local to the recovery package spares
// callers the awkward cross-package import of `txn.Options` purely to feed
// the open path. Codec and WeightCodec mirror [txn.Options] field-for-field,
// so a caller holding a [txn.Options] can build a recovery [Options] from it
// with [OptionsFromTxn] (or the equivalent literal); MaxTxnOps is
// recovery-specific and has no [txn.Options] counterpart (the producer cap is
// set on the [txn.Store] at construction, not via [txn.Options]).
type Options[N comparable, W any] struct {
	// Codec serialises endpoint identifiers. Must not be nil.
	Codec txn.Codec[N]
	// WeightCodec serialises edge weights. It must mirror the producer
	// store's configuration: nil for a codec-only store
	// ([txn.NewStoreWithCodec], whose frames carry no weight bytes and
	// replay with the zero value of W), non-nil for a weight-codec store
	// ([txn.NewStoreWithOptions]).
	WeightCodec txn.WeightCodec[W]
	// MaxTxnOps bounds the number of ops recovery buffers for a single v3
	// transaction before its [txn.OpCommit] marker. A transaction whose
	// buffered op count exceeds the resolved cap fails recovery with
	// [ErrTransactionTooLarge] rather than allocating proportionally to an
	// unbounded marker-less run (a legitimately huge transaction OR a
	// crafted/corrupt WAL tail).
	//
	// The value follows the standard convention: 0 (the zero value every
	// existing caller carries) selects [txn.DefaultMaxTxnOps], the same
	// default the producer uses, so producer and recovery agree; a negative
	// value ([txn.MaxTxnOpsUnlimited]) disables the cap; any other positive
	// value is the cap verbatim. It must be >= the producer cap so every
	// durably-committed transaction replays.
	MaxTxnOps int
}

// OptionsFromTxn builds a recovery [Options] from a [txn.Options], copying the
// two codecs and leaving [Options.MaxTxnOps] at its zero value (which resolves
// to [txn.DefaultMaxTxnOps], the producer's default, so producer and recovery
// agree). It replaces the direct `Options(opts)` struct conversion that worked
// while the two types were field-for-field identical; that conversion no
// longer compiles now that recovery [Options] carries the recovery-specific
// MaxTxnOps field. Callers that need a non-default recovery cap set MaxTxnOps
// on the returned value.
func OptionsFromTxn[N comparable, W any](opts txn.Options[N, W]) Options[N, W] {
	return Options[N, W]{Codec: opts.Codec, WeightCodec: opts.WeightCodec}
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

// ErrTransactionTooLarge is returned by [Open] / [OpenCtx] when a v3
// transaction buffers more than the recovery op cap (see [Options.MaxTxnOps]
// and [txn.DefaultMaxTxnOps]) before its [txn.OpCommit] marker is read.
//
// Recovery buffers an in-flight transaction's ops in memory and applies
// them only on the durable [txn.OpCommit] marker, so an unbounded
// marker-less run of valid-CRC v3 frames — a legitimately huge transaction
// OR a crafted/corrupt WAL tail — would otherwise force recovery to
// allocate proportionally to the run before discarding it (an
// OOM-on-restart / decompression-bomb-class hazard). The cap stops the run
// the instant the buffered op count exceeds it, so recovery never allocates
// past the bound. It is the recovery-side dual of
// [store/txn.ErrTransactionTooLarge]; the producer cap is <= the recovery
// cap, so any transaction a producer commits durably is guaranteed to fit
// and replay — this sentinel only fires on an over-cap transaction that was
// never legitimately produced (corruption or an out-of-band WAL).
//
// Like the other genuine-corruption sentinels it is fail-stop: it is
// surfaced as the function error, classified by [Result.IsClean] as not
// clean, and the committed prefix that pre-dates the run stays in
// [Result.Graph] for diagnostics.
var ErrTransactionTooLarge = errors.New("recovery: v3 transaction exceeds the per-transaction op cap")

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

// ConstraintRecord is one durable schema-constraint definition recovered from
// the WAL ([txn.OpCreateConstraint]) or the snapshot's constraints.bin
// component. It mirrors [txn.Op]'s constraint fields without the
// graph-mutation payload. The engine maps it back to its own constraint
// registry on open.
type ConstraintRecord struct {
	// Kind selects UNIQUE vs NOT NULL.
	Kind txn.ConstraintKind
	// Label is the constrained node label.
	Label string
	// Property is the constrained property key.
	Property string
	// Name is the user-defined constraint name.
	Name string
}

// decodeConstraintBody parses the body of an [txn.OpCreateConstraint] /
// [txn.OpDropConstraint] frame produced by [txn.appendOpConstraintBody]:
//
//	uint8  constraintKind
//	uint16 labelLen || label
//	uint16 propLen  || property
//	uint16 nameLen  || name
//
// It returns the decoded record and ok=false when the body is truncated or
// the trailing length prefixes overflow the buffer (a torn final write); the
// replay loop treats a constraint op whose body cannot be walked the same way
// it treats any other undecodable trailing op — a benign cut, not corruption.
func decodeConstraintBody(kind txn.OpKind, body []byte) (ConstraintRecord, bool) {
	if len(body) < 1 {
		return ConstraintRecord{}, false
	}
	ck := txn.ConstraintKind(body[0])
	rest := body[1:]
	label, rest, ok := readU16String(rest)
	if !ok {
		return ConstraintRecord{}, false
	}
	prop, rest, ok := readU16String(rest)
	if !ok {
		return ConstraintRecord{}, false
	}
	name, _, ok := readU16String(rest)
	if !ok {
		return ConstraintRecord{}, false
	}
	_ = kind // kind is implied by the caller; the body tag is authoritative.
	return ConstraintRecord{Kind: ck, Label: label, Property: prop, Name: name}, true
}

// readU16String reads a uint16 little-endian length prefix followed by that
// many bytes from the head of buf, returning the string, the unread tail, and
// ok=false when the prefix or the body runs past the end of buf.
func readU16String(buf []byte) (s string, rest []byte, ok bool) {
	if len(buf) < 2 {
		return "", buf, false
	}
	n := int(binary.LittleEndian.Uint16(buf))
	buf = buf[2:]
	if len(buf) < n {
		return "", buf, false
	}
	return string(buf[:n]), buf[n:], true
}

// constraintSet accumulates the recovered constraint definitions, reconciling
// the snapshot-loaded set with the WAL-replayed CREATE/DROP CONSTRAINT ops by
// (kind, label, property) key with last-writer-wins semantics: a later DROP
// removes an earlier CREATE, and a later CREATE replaces an earlier one
// (re-declaring with a new name). It is keyed so a UNIQUE and a NOT NULL
// constraint on the same (label, property) are tracked independently, matching
// the engine registry, which keys UNIQUE and NOT NULL in separate maps.
type constraintSet struct {
	byKey map[constraintSetKey]ConstraintRecord
}

// constraintSetKey identifies one constraint slot. Name is deliberately
// excluded: re-declaring a constraint on the same (kind, label, property) with
// a different name replaces the prior record rather than adding a second.
type constraintSetKey struct {
	kind     txn.ConstraintKind
	label    string
	property string
}

func newConstraintSet() *constraintSet {
	return &constraintSet{byKey: make(map[constraintSetKey]ConstraintRecord)}
}

// applyCreate records a CREATE CONSTRAINT.
func (s *constraintSet) applyCreate(rec ConstraintRecord) {
	s.byKey[constraintSetKey{rec.Kind, rec.Label, rec.Property}] = rec
}

// applyDrop removes a constraint by its (kind, label, property) key.
func (s *constraintSet) applyDrop(rec ConstraintRecord) {
	delete(s.byKey, constraintSetKey{rec.Kind, rec.Label, rec.Property})
}

// snapshot returns the accumulated constraints in deterministic order
// (kind, label, property, name).
func (s *constraintSet) snapshot() []ConstraintRecord {
	if len(s.byKey) == 0 {
		return nil
	}
	out := make([]ConstraintRecord, 0, len(s.byKey))
	for _, rec := range s.byKey {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		if a.Property != b.Property {
			return a.Property < b.Property
		}
		return a.Name < b.Name
	})
	return out
}

// accumulateConstraintOp routes a decoded constraint op into cs. It returns
// (true, true) when op is a constraint op that decoded cleanly, (true, false)
// when op is a constraint op whose body is torn/undecodable (the caller treats
// this like any other undecodable trailing op — a benign cut), and
// (false, false) when op is not a constraint op at all (the caller applies it
// to the graph as usual).
func accumulateConstraintOp(cs *constraintSet, op *Op) (isConstraint, ok bool) {
	switch op.Kind {
	case txn.OpCreateConstraint:
		rec, decoded := decodeConstraintBody(op.Kind, op.Body)
		if !decoded {
			return true, false
		}
		cs.applyCreate(rec)
		return true, true
	case txn.OpDropConstraint:
		rec, decoded := decodeConstraintBody(op.Kind, op.Body)
		if !decoded {
			return true, false
		}
		cs.applyDrop(rec)
		return true, true
	default:
		return false, false
	}
}

// IndexRecord is one durable index definition recovered from the WAL
// ([txn.OpCreateIndex]). It mirrors [txn.Op]'s index fields without the
// graph-mutation payload. The engine re-registers and re-backfills these on
// open so user-created indexes survive a crash and restart.
type IndexRecord struct {
	// Kind selects hash vs btree.
	Kind txn.IndexKind
	// Name is the user-defined index name.
	Name string
	// Label is the indexed node label.
	Label string
	// Property is the indexed property key.
	Property string
}

// decodeIndexBody parses the body of an [txn.OpCreateIndex] /
// [txn.OpDropIndex] frame produced by [txn.appendOpIndexBody]:
//
//	uint8  indexKind ([txn.IndexKind])
//	uint16 nameLen  || name
//	uint16 labelLen || label
//	uint16 propLen  || property
//
// It returns the decoded record and ok=false when the body is truncated
// (a torn final write — treated as a benign cut, not corruption).
func decodeIndexBody(body []byte) (IndexRecord, bool) {
	if len(body) < 1 {
		return IndexRecord{}, false
	}
	ik := txn.IndexKind(body[0])
	rest := body[1:]
	name, rest, ok := readU16String(rest)
	if !ok {
		return IndexRecord{}, false
	}
	label, rest, ok := readU16String(rest)
	if !ok {
		return IndexRecord{}, false
	}
	prop, _, ok := readU16String(rest)
	if !ok {
		return IndexRecord{}, false
	}
	return IndexRecord{Kind: ik, Name: name, Label: label, Property: prop}, true
}

// indexSet accumulates recovered index definitions, reconciling CREATE/DROP ops
// by name with last-writer-wins semantics: a DROP removes a CREATE of the same
// name, and a later CREATE replaces an earlier one.
type indexSet struct {
	byName map[string]IndexRecord
}

func newIndexSet() *indexSet {
	return &indexSet{byName: make(map[string]IndexRecord)}
}

func (s *indexSet) applyCreate(rec IndexRecord) {
	s.byName[rec.Name] = rec
}

func (s *indexSet) applyDrop(name string) {
	delete(s.byName, name)
}

// snapshot returns the accumulated index definitions in deterministic order
// (by name).
func (s *indexSet) snapshot() []IndexRecord {
	if len(s.byName) == 0 {
		return nil
	}
	out := make([]IndexRecord, 0, len(s.byName))
	for _, rec := range s.byName {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// accumulateIndexOp routes a decoded index op into is. It returns
// (true, true) when op is an index op that decoded cleanly, (true, false) when
// the body is torn/undecodable (benign cut), and (false, false) when op is not
// an index op.
func accumulateIndexOp(is *indexSet, op *Op) (isIndex, ok bool) {
	switch op.Kind {
	case txn.OpCreateIndex:
		rec, decoded := decodeIndexBody(op.Body)
		if !decoded {
			return true, false
		}
		is.applyCreate(rec)
		return true, true
	case txn.OpDropIndex:
		rec, decoded := decodeIndexBody(op.Body)
		if !decoded {
			return true, false
		}
		is.applyDrop(rec.Name)
		return true, true
	default:
		return false, false
	}
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
// opts.Codec must be non-nil. opts.WeightCodec mirrors the producer
// store: nil for a codec-only store ([txn.NewStoreWithCodec]), non-nil
// for a weight-codec store ([txn.NewStoreWithOptions]). Pre-T8 WALs
// that contain only [txn.OpAddEdge] frames replay identically under
// either setting: the apply phase writes the zero value of W for
// each unweighted record. Mixed WALs preserve weights for
// [txn.OpAddEdgeWeighted] frames and apply zero for the unweighted
// ones — the forward-compatibility contract documented at
// [txn.NewStoreWithOptions].
//
// Open is safe to call on a dir that contains only a snapshot, only
// a WAL, both, or neither: missing components are tolerated and the
// returned [Result.Graph] is a fresh empty graph when neither exists.
//
// A torn or truncated WAL tail — the normal state after a crash between
// two fsyncs — is benign: Open recovers the committed prefix, returns a
// nil error, and records the cut via [Result.TailErr] / [Result.IsClean].
// Genuine corruption inside an already-durable frame ([wal.ErrCRCMismatch],
// [wal.ErrBadMagic], [wal.ErrUnsupportedVersion], [wal.ErrFrameTooLarge], or
// a legacy/garbage record version surfaced as [ErrUnsupportedRecordVersion])
// is fail-stop: Open returns that error (the committed prefix is still
// placed in [Result.Graph] for diagnostics) and [Result.IsClean] reports
// false. Callers that recover-then-append must branch on the returned error
// or [Result.IsClean] and refuse to append onto a corrupt WAL, which would
// otherwise permanently embed the corruption and drop every committed op
// past the bad frame.
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
	res, err := openCodec[N, W](ctx, dir, opts.Codec, opts.WeightCodec, resolveRecoveryMaxTxnOps(opts.MaxTxnOps))
	if err != nil {
		metrics.IncCounter("store.recovery.OpenCtx.errors", 1)
	}
	return res, err
}

// resolveRecoveryMaxTxnOps normalises the [Options.MaxTxnOps] field to the
// internal convention used by [openCodec]: 0 (the zero value every existing
// caller carries) selects [txn.DefaultMaxTxnOps] — matching the producer
// default so the two caps agree; [txn.MaxTxnOpsUnlimited] (-1) selects 0,
// meaning "no cap"; any other positive value is taken verbatim. It mirrors
// the producer-side txn.resolveMaxTxnOps so a recovery built from a
// [txn.Options]-shaped caller inherits the identical finite default.
func resolveRecoveryMaxTxnOps(maxTxnOps int) int {
	switch maxTxnOps {
	case 0:
		return txn.DefaultMaxTxnOps
	case txn.MaxTxnOpsUnlimited:
		return 0
	default:
		return maxTxnOps
	}
}

// defaultRecoveryConfig is the adjacency-list configuration used to
// reconstruct a graph when no persisted shape is available: no snapshot,
// a snapshot-load failure (diagnostic graph only), or a snapshot whose
// manifest predates the persisted config field.
//
// Multigraph: true is the historical recovery behaviour. It matches
// openCypher's property-graph model (CREATE of a relationship is
// additive — two CREATEs between the same ordered pair must yield two
// relationships) and the configuration the Cypher TCK harness uses. The
// WAL, snapshot and CSR layers already round-trip parallel edges with
// distinct per-instance types/properties, so a graph recovered from a
// config-less snapshot must be multigraph or those parallel edges
// collapse to one on the next reopen — silent data loss for every
// consumer that recovers a pre-config snapshot from disk. A graph that
// never created a parallel edge behaves identically under either mode,
// so this default is also safe for simple graphs whose snapshots predate
// the persisted config.
func defaultRecoveryConfig() adjlist.Config {
	return adjlist.Config{Directed: true, Multigraph: true}
}

// recoveryGraphConfig resolves the adjacency-list configuration to
// reconstruct a graph from a snapshot manifest's persisted graph config
// (pass [snapshot.Manifest.GraphConfig]). When gc is non-nil (every NEW
// full snapshot carries it), its directed/multigraph flags are honoured
// exactly, so a graph created SIMPLE (Multigraph: false) is recovered
// SIMPLE and a graph created MULTIGRAPH is recovered MULTIGRAPH — the same
// AddEdge semantics before and after a snapshot round-trip. When gc is nil
// (an older snapshot, or one written by the CSR-only legacy writer) the
// historical [defaultRecoveryConfig] is used so pre-existing snapshots
// replay byte-for-byte as they did before this field existed.
//
// MaxShardCapacity is never restored from the manifest: it is a runtime
// growth bound, not a property of the stored graph, and re-imposing it
// could make recovery itself fail with [adjlist.ErrShardFull] while
// replaying data that legitimately exceeds the cap. The recovered graph
// is always unbounded.
func recoveryGraphConfig(gc *snapshot.GraphConfig) adjlist.Config {
	if gc == nil {
		return defaultRecoveryConfig()
	}
	return adjlist.Config{
		Directed:   gc.Directed,
		Multigraph: gc.Multigraph,
	}
}

// openCodec is the shared core of [Open] and [OpenCtx]. wcodec is nil
// for the codec-only path; when non-nil the function honours
// [txn.OpAddEdgeWeighted] records by decoding the typed weight payload
// before applying.
//
// maxTxnOps is the resolved recovery op cap (0 means "no cap"): when a v3
// transaction buffers more than maxTxnOps ops before its OpCommit marker,
// replay stops with [ErrTransactionTooLarge] rather than buffering an
// unbounded marker-less run.
//
//nolint:gocyclo // recovery: snapshot probe + labels load + WAL open + per-frame decode + per-frame apply + ctx ticks + labels apply
func openCodec[N comparable, W any](
	ctx context.Context,
	dir string,
	codec txn.Codec[N],
	wcodec txn.WeightCodec[W],
	maxTxnOps int,
) (Result[N, W], error) {
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.recovery.openCodec.errors", 1)
		// No snapshot has been loaded yet, so the diagnostic graph uses the
		// default reconstruction config (see recoveryGraphConfig).
		return Result[N, W]{Graph: lpg.New[N, W](defaultRecoveryConfig())}, err
	}
	snapDir := filepath.Join(dir, "snapshot")

	// Interrupted-publish repair. The snapshot writers publish via a
	// crash-atomic three-step swap — archive(live -> live+".bak"), then
	// rename(staging -> live), then drop the backup (see store/snapshot).
	// A crash inside that window leaves the live directory absent and the
	// previous snapshot stranded at snapshot.bak; the WAL holds only the
	// deltas committed after that snapshot, so WITHOUT promotion every
	// checkpointed transaction would be silently lost. Promote the backup
	// before probing for the manifest below. The stale staging directory
	// is always safe to drop: it is rewritten from scratch on every
	// checkpoint and only ever duplicates state that is fully
	// reconstructable from the (promoted) snapshot plus the WAL.
	snapBak := snapDir + ".bak"
	_ = os.RemoveAll(snapDir + ".tmp") // best-effort: stale staging cleanup
	if _, statErr := os.Stat(filepath.Join(snapDir, "manifest.json")); errors.Is(statErr, os.ErrNotExist) {
		if _, bakErr := os.Stat(filepath.Join(snapBak, "manifest.json")); bakErr == nil {
			if renErr := os.Rename(snapBak, snapDir); renErr != nil {
				// Fail-stop, never fail-silent: the backup is the only
				// surviving copy of the checkpointed state; proceeding
				// without it would silently recover an empty graph.
				metrics.IncCounter("store.recovery.openCodec.errors", 1)
				return Result[N, W]{Graph: lpg.New[N, W](defaultRecoveryConfig())},
					fmt.Errorf("recovery: promote snapshot backup: %w", renErr)
			}
			// Promoted; the normal snapshot load below proceeds as if the
			// publish had never been interrupted.
		}
	}

	var snapLabels snapshot.LabelsReadback
	var snapProps snapshot.PropertiesReadback
	var snapEdgeHandles snapshot.EdgeHandlesReadback
	var snapIndexes []snapshot.IndexReadback
	var haveSnapLabels, haveSnapProps, haveSnapEdgeHandles bool
	// snapshotSideAppliedEarly records that the snapshot's labels and
	// properties were applied BEFORE WAL replay (the self-sufficient path);
	// the deferred after-WAL apply below is then skipped for them.
	var snapshotSideAppliedEarly bool

	// cAcc accumulates schema constraints. It is seeded from the snapshot's
	// constraints.bin component below (the checkpoint-survival source) and
	// then reconciled with the CREATE/DROP CONSTRAINT ops the WAL replay loop
	// feeds it, so the result is the constraint set as of the last durable
	// commit even when a checkpoint truncated the WAL prefix that first
	// declared a constraint.
	cAcc := newConstraintSet()

	// iAcc accumulates durable index definitions from CREATE/DROP INDEX ops
	// in the WAL. Unlike constraints, indexes have no snapshot component today;
	// the entire durable set comes from WAL ops replayed here. The engine
	// re-registers and re-backfills them on open.
	iAcc := newIndexSet()

	// Load the snapshot manifest BEFORE constructing the live graph so the
	// graph is reconstructed with the directed/multigraph shape the
	// originating graph was created with. A manifest that predates the
	// persisted config (or the CSR-only legacy writer, which has no graph to
	// read) leaves Manifest.GraphConfig nil; recoveryGraphConfig then returns
	// the historical default {Directed: true, Multigraph: true} so existing
	// snapshots — including the additive-CREATE openCypher engine snapshots
	// that depend on multigraph — replay exactly as before.
	var loaded snapshot.LoadedSnapshot
	haveManifest := false
	if _, err := os.Stat(filepath.Join(snapDir, "manifest.json")); err == nil {
		loaded, err = snapshot.LoadSnapshotFull(snapDir)
		if err != nil {
			metrics.IncCounter("store.recovery.openCodec.errors", 1)
			// On a snapshot-load failure no config is available; build the
			// diagnostic graph with the default reconstruction config.
			return Result[N, W]{Graph: lpg.New[N, W](defaultRecoveryConfig())},
				fmt.Errorf("recovery: snapshot open: %w", err)
		}
		haveManifest = true
	}

	// Reconstruct the graph with the persisted shape (or the default when no
	// manifest / no persisted config). The config is fixed for the lifetime
	// of the recovered graph.
	g := lpg.New[N, W](recoveryGraphConfig(loaded.Manifest.GraphConfig))
	res := Result[N, W]{Graph: g}

	if haveManifest {
		res.SnapshotHit = true
		res.SnapshotSchemaVersion = loaded.Manifest.Version
		snapLabels = loaded.Labels
		snapProps = loaded.Properties
		snapEdgeHandles = loaded.EdgeHandles
		snapIndexes = loaded.Indexes
		// Seed the constraint accumulator with the snapshot's durable
		// constraint set so a checkpoint that truncated the WAL prefix does
		// not lose constraints; WAL ops replayed below layer on top.
		for _, c := range loaded.Constraints.Specs {
			cAcc.applyCreate(ConstraintRecord{
				Kind:     txn.ConstraintKind(c.Kind),
				Label:    c.Label,
				Property: c.Property,
				Name:     c.Name,
			})
		}
		haveSnapLabels = len(loaded.Labels.NodeLabels) > 0 || len(loaded.Labels.EdgeLabels) > 0
		haveSnapProps = len(loaded.Properties.NodeProperties) > 0 || len(loaded.Properties.EdgeProperties) > 0
		haveSnapEdgeHandles = len(loaded.EdgeHandles.Records) > 0

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
			// Restore the snapshot tombstone set now that every snapshot
			// node is interned (mapper) and materialised (CSR), and BEFORE
			// the WAL is replayed: a WAL re-create (OpAddNode) for a
			// tombstoned id then revives it chronologically, and a WAL
			// delete re-tombstones idempotently. Only the self-sufficient
			// path applies snapshot tombstones — on the WAL-authoritative
			// v2 path (WAL never truncated) the deletions are reconstructed
			// by replaying OpRemoveNode, so applying a possibly-stale
			// snapshot set there could wrongly re-tombstone a re-created
			// node.
			snapshot.ApplyTombstonesToGraph(g, loaded.Tombstones)
			res.SnapshotTombstones = len(loaded.Tombstones.IDs)

			// Self-sufficient path: the mapper is fully restored, so every
			// snapshot node is already interned. Apply the snapshot's labels
			// and properties NOW — BEFORE WAL replay — so the WAL tail's
			// mutations win chronologically. The snapshot is the committed
			// state at checkpoint time; the WAL holds the deltas that came
			// after. If the WAL tail deleted a node and re-created it with
			// different labels/properties, applying the snapshot state first
			// (then the WAL on top) yields the re-created state, whereas the
			// old after-WAL order re-added the stale snapshot labels and
			// clobbered the re-created properties (#1266). The mapper-less
			// (v2) path below keeps applying these after WAL replay, where
			// the WAL is what interns the nodes the snapshot records.
			if haveSnapLabels {
				if err := snapshot.ApplyLabelsToGraph(g, loaded.Labels); err != nil {
					metrics.IncCounter("store.recovery.openCodec.errors", 1)
					return res, fmt.Errorf("recovery: apply snapshot labels: %w", err)
				}
				res.SnapshotLabels = len(loaded.Labels.NodeLabels) + len(loaded.Labels.EdgeLabels)
			}
			if haveSnapProps {
				if err := snapshot.ApplyPropertiesToGraph(g, loaded.Properties); err != nil {
					metrics.IncCounter("store.recovery.openCodec.errors", 1)
					return res, fmt.Errorf("recovery: apply snapshot properties: %w", err)
				}
				res.SnapshotProperties = len(loaded.Properties.NodeProperties) + len(loaded.Properties.EdgeProperties)
			}
			// Per-handle edge metadata: re-attach each parallel edge's
			// per-CREATE type and properties keyed to the stable handle the
			// CSR component already re-stamped onto the adjacency slot. Applied
			// BEFORE WAL replay (self-sufficient path) so the WAL tail's
			// per-handle mutations win chronologically, and seeded with the
			// handle high-water counter so post-recovery edge creation never
			// re-mints a live handle (invariant I5).
			if haveSnapEdgeHandles {
				snapshot.ApplyEdgeHandlesToGraph(g, loaded.EdgeHandles)
			}
			snapshotSideAppliedEarly = true
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
		// The buffer may also hold the orphaned ops of a PRIOR transaction
		// whose OpCommit was never written (its commit failed between the
		// data frames and the marker, and the WAL kept growing); those are
		// discarded on the next marker by the TxnSeq suffix filter below,
		// never merged into the committed transaction.
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
					// Bound the in-flight transaction buffer: stop the instant
					// the buffered op count would exceed the cap, BEFORE
					// appending (and before copying another op body), so a
					// marker-less run — a legitimately huge transaction OR a
					// crafted/corrupt valid-CRC tail — can never drive recovery
					// to allocate proportionally to the run. Fail-stop, like the
					// other genuine-corruption sentinels (tailErrIsCorruption).
					if maxTxnOps > 0 && len(pending) >= maxTxnOps {
						metrics.IncCounter("store.recovery.openCodec.txnTooLarge", 1)
						res.TailErr = fmt.Errorf("%w: buffered %d ops with no commit marker (cap %d)",
							ErrTransactionTooLarge, len(pending), maxTxnOps)
						break
					}
					pending = append(pending, op)
					continue
				}
				// Durable transaction boundary: apply the buffered ops as a
				// unit. A crash that tore the batch never reaches a marker
				// with a partial set — the marker is the last frame written.
				//
				// Atomicity guard: a marker commits ONLY the ops carrying
				// its own TxnSeq. The buffer may still hold ops from a prior
				// transaction whose marker was never written (commit failed
				// after the data frames, before the OpCommit); flushing them
				// here would resurrect an aborted transaction fused into
				// this one. Commits are serialised, so the committed ops are
				// a contiguous suffix of the buffer: discard the orphaned
				// prefix whose TxnSeq does not match the marker's.
				commitSeq := op.TxnSeq
				start := 0
				for start < len(pending) && pending[start].TxnSeq != commitSeq {
					start++
				}
				if start > 0 {
					metrics.IncCounter("store.recovery.openCodec.orphanedOps", uint64(start))
				}
				committed := pending[start:]
				ok := true
				for i := range committed {
					if !applyOrAccumulate(g, &committed[i], codec, wcodec, cAcc, iAcc) {
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
			if !applyOrAccumulate(g, &op, codec, wcodec, cAcc, iAcc) {
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
		res.WALTailOffset = r.TailOffset()
	}

	// Finalise the recovered constraint set: the snapshot's durable
	// constraints reconciled with the WAL-replayed CREATE/DROP CONSTRAINT ops.
	// Runs whether or not a WAL was present so a snapshot-only directory still
	// surfaces its constraints.
	res.Constraints = cAcc.snapshot()

	// Finalise the recovered index definitions from WAL-replayed
	// CREATE/DROP INDEX ops.
	res.Indexes = iAcc.snapshot()

	// Mapper-less (v2) path only: apply snapshot-side labels after the WAL
	// is fully applied so the mapper has every node interned that the WAL
	// referenced. Snapshot label records whose NodeIDs the mapper cannot
	// resolve are dropped (with metric) by ApplyLabelsToGraph, not surfaced
	// as an error: this keeps recovery resilient against snapshot-without-
	// WAL flows. The self-sufficient path applied these BEFORE WAL replay
	// (snapshotSideAppliedEarly), so it is skipped here.
	if haveSnapLabels && !snapshotSideAppliedEarly {
		if err := snapshot.ApplyLabelsToGraph(g, snapLabels); err != nil {
			metrics.IncCounter("store.recovery.openCodec.errors", 1)
			return res, fmt.Errorf("recovery: apply snapshot labels: %w", err)
		}
		res.SnapshotLabels = len(snapLabels.NodeLabels) + len(snapLabels.EdgeLabels)
	}
	// Apply snapshot-side properties after labels for symmetry with
	// the write order. Resilient skip-on-unresolved semantics mirror
	// the labels apply path.
	if haveSnapProps && !snapshotSideAppliedEarly {
		if err := snapshot.ApplyPropertiesToGraph(g, snapProps); err != nil {
			metrics.IncCounter("store.recovery.openCodec.errors", 1)
			return res, fmt.Errorf("recovery: apply snapshot properties: %w", err)
		}
		res.SnapshotProperties = len(snapProps.NodeProperties) + len(snapProps.EdgeProperties)
	}
	// Per-handle edge metadata on the mapper-less (v2) path: applied after the
	// WAL replay so the mapper has interned every node the snapshot records.
	// The handle-keyed stores are NodeID-keyed and do not require the edge to
	// be present, so this is safe even when the v2 CSR carried no handle
	// column. The self-sufficient path applied these before WAL replay
	// (snapshotSideAppliedEarly), so it is skipped here.
	if haveSnapEdgeHandles && !snapshotSideAppliedEarly {
		snapshot.ApplyEdgeHandlesToGraph(g, snapEdgeHandles)
	}
	// Secondary indexes (label / hash / btree) are re-hydrated last so
	// the live graph is fully populated when we ask the Manager for the
	// matching subscribers. Indexes are only re-hydrated when the LPG
	// has a Manager wired in; absent that, the snapshot bytes are
	// dropped (the index is rebuilt lazily on the next mutation pass).
	if len(snapIndexes) > 0 {
		res.SnapshotIndexes = applySnapshotIndexes(g.IndexManager(), snapIndexes)
	}
	// Fail-stop on genuine corruption: a CRC mismatch, bad magic,
	// unsupported frame/record version, or oversized length inside an
	// already-durable frame means the WAL is damaged, not merely
	// crash-truncated. Surface it as the function error (the committed
	// prefix stays in res.Graph for diagnostics) so no caller can silently
	// append onto the corruption and drop every op past the bad frame. A
	// benign torn tail ([wal.ErrTornFrame]) and a CRC-valid-but-unparseable
	// trailing frame are NOT corruption and return success — the normal
	// crash-after-fsync recovery case (see [tailErrIsCorruption]).
	if tailErrIsCorruption(res.TailErr) {
		metrics.IncCounter("store.recovery.openCodec.corruptTail", 1)
		return res, res.TailErr
	}
	return res, nil
}

// applyOrAccumulate routes a decoded op either to the constraint accumulator
// cs (when it is a schema-DDL op, which mutates no graph state) or to the
// graph via [applyOpCodec] (every mutation op). It returns true when the op
// was handled and false only when the op's body is torn/undecodable, so the
// caller surfaces the same cut-off boundary it would for any other undecodable
// trailing op. Constraint ops are surfaced via [Result.Constraints] rather than
// applied to g because a constraint definition is engine schema, not graph
// topology.
func applyOrAccumulate[N comparable, W any](
	g *lpg.Graph[N, W],
	op *Op,
	codec txn.Codec[N],
	wcodec txn.WeightCodec[W],
	cs *constraintSet,
	is *indexSet,
) bool {
	if isConstraint, ok := accumulateConstraintOp(cs, op); isConstraint {
		return ok
	}
	if isIdx, ok := accumulateIndexOp(is, op); isIdx {
		return ok
	}
	return applyOpCodec(g, op, codec, wcodec)
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
			// Reconstruct the tombstone so the node is logically deleted
			// after replay, not merely a label-stripped live node. Without
			// this the deletion is non-durable: a re-opened store would
			// resurrect the node as an undeletable ghost. A later OpAddNode
			// for the same key revives it (g.AddNode clears the tombstone),
			// so replay order is honoured.
			g.RemoveNode(src)
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
			// LPG edge removal: a fully-disconnected pair also sheds its
			// per-pair edge labels/properties, so a later OpAddEdge for the
			// same endpoints does not resurrect the removed edge's labels.
			g.RemoveEdge(src, dst)
		}

	case txn.OpAddEdgeH:
		return applyAddEdgeH(g, src, dst, rest, wcodec)

	case txn.OpSetEdgeLabelByHandle:
		return applySetEdgeLabelByHandle(g, src, dst, rest)

	case txn.OpSetEdgePropertyByHandle:
		return applySetEdgePropertyByHandle(g, src, dst, rest)

	case txn.OpRemoveEdgeInstanceByHandle:
		return applyRemoveEdgeInstanceByHandle(g, src, dst, rest)

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

// trailingHandle reads the 8-byte little-endian stable edge handle that the
// Stage-2 handle-bearing op kinds append after their same-kind body. It
// returns (handle, true) when exactly 8 bytes remain at the head of rest,
// and (0, false) when the frame is truncated (no durable handle). The
// caller treats false as a corrupt frame and stops replay.
func trailingHandle(rest []byte) (uint64, bool) {
	if len(rest) < 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(rest[:8]), true
}

// applyAddEdgeH applies an [txn.OpAddEdgeH] frame. src and dst were already
// codec-decoded by the caller; rest is the body after them: an optional
// weight (present only when the producer store had a [txn.WeightCodec]),
// then a uint16-length-prefixed label (always empty for an edge add),
// then the 8-byte stable handle. The edge is inserted via
// [lpg.Graph.AddEdgeHIfAbsent] so a snapshot that already loaded this
// handle (and any earlier replayed frame) makes the replay idempotent —
// no doubled parallel edge. The handle high-water counter is re-seeded so
// a post-recovery edge creation never re-mints a live handle (invariant I5).
//
// A nil wcodec mirrors a codec-only producer ([txn.NewStoreWithCodec]):
// the frame carries no weight bytes and the edge replays with the zero
// value of W. A non-nil wcodec first tries the weighted layout; when that
// does not parse exactly — the frame was written by a codec-only store but
// is being recovered by a caller that passes a weight codec (every caller
// had to before nil was accepted) — it falls back to the weight-less
// layout. The two layouts cannot be confused: the exact-consume check in
// edgeHTail leaves a weight-less frame with fewer bytes after a weight
// decode than the mandatory label-prefix + handle tail requires.
func applyAddEdgeH[N comparable, W any](g *lpg.Graph[N, W], src, dst N, rest []byte, wcodec txn.WeightCodec[W]) bool {
	if wcodec != nil {
		if weight, tail, err := wcodec.Decode(rest); err == nil {
			if handle, ok := edgeHTail(tail); ok {
				return insertEdgeH(g, src, dst, weight, handle)
			}
		}
		// Weighted layout rejected; fall through to the codec-only layout.
	}
	var zero W
	handle, ok := edgeHTail(rest)
	if !ok {
		return false
	}
	return insertEdgeH(g, src, dst, zero, handle)
}

// edgeHTail parses the mandatory tail of an [txn.OpAddEdgeH] body: a
// uint16-length-prefixed label followed by the 8-byte stable handle. The
// tail must consume rest exactly — an OpAddEdgeH body ends at the handle —
// which is what lets [applyAddEdgeH] discriminate the weighted layout from
// the weight-less codec-only layout deterministically.
func edgeHTail(rest []byte) (uint64, bool) {
	if len(rest) < 2 {
		return 0, false
	}
	n := binary.LittleEndian.Uint16(rest)
	rest = rest[2:]
	if uint64(len(rest)) != uint64(n)+8 {
		return 0, false
	}
	return trailingHandle(rest[n:])
}

// insertEdgeH performs the idempotent handle-stamped edge insert shared by
// both [applyAddEdgeH] layouts and re-seeds the handle high-water counter.
func insertEdgeH[N comparable, W any](g *lpg.Graph[N, W], src, dst N, w W, handle uint64) bool {
	if _, err := g.AddEdgeHIfAbsent(src, dst, w, handle); err != nil {
		metrics.IncCounter("store.recovery.applyOp.addEdgeErrors", 1)
		return false
	}
	g.SeedEdgeHandle(handle + 1)
	return true
}

// applySetEdgeLabelByHandle applies an [txn.OpSetEdgeLabelByHandle] frame.
// rest is the body after the two decoded endpoints: a uint16-length-prefixed
// label followed by the 8-byte stable handle. It rebuilds one parallel
// edge's per-CREATE type keyed to its handle.
func applySetEdgeLabelByHandle[N comparable, W any](g *lpg.Graph[N, W], src, dst N, rest []byte) bool {
	if len(rest) < 2 {
		return false
	}
	n := binary.LittleEndian.Uint16(rest)
	rest = rest[2:]
	if uint64(len(rest)) < uint64(n) {
		return false
	}
	label := string(rest[:n])
	rest = rest[n:]
	handle, ok := trailingHandle(rest)
	if !ok {
		return false
	}
	g.SetEdgeLabelByHandle(src, dst, handle, label)
	g.SeedEdgeHandle(handle + 1)
	return true
}

// applySetEdgePropertyByHandle applies an [txn.OpSetEdgePropertyByHandle]
// frame. rest is the body after the two decoded endpoints: a
// uint16-length-prefixed key, the encoded property value, then the 8-byte
// stable handle. It rebuilds one parallel edge's per-CREATE property keyed
// to its handle.
func applySetEdgePropertyByHandle[N comparable, W any](g *lpg.Graph[N, W], src, dst N, rest []byte) bool {
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
	val, rest, verr := decodeRecoveryPropertyValue(rest)
	if verr != nil {
		return false
	}
	handle, ok := trailingHandle(rest)
	if !ok {
		return false
	}
	g.SetEdgePropertyByHandle(src, dst, handle, key, val)
	g.SeedEdgeHandle(handle + 1)
	return true
}

// applyRemoveEdgeInstanceByHandle applies an
// [txn.OpRemoveEdgeInstanceByHandle] frame. rest is the body after the two
// decoded endpoints: a uint16 label length (always 0) followed by the
// 8-byte stable handle. It drops one logical edge's per-handle metadata.
func applyRemoveEdgeInstanceByHandle[N comparable, W any](g *lpg.Graph[N, W], src, dst N, rest []byte) bool {
	if len(rest) < 2 {
		return false
	}
	n := binary.LittleEndian.Uint16(rest)
	rest = rest[2:]
	if uint64(len(rest)) < uint64(n) {
		return false
	}
	rest = rest[n:]
	handle, ok := trailingHandle(rest)
	if !ok {
		return false
	}
	g.RemoveEdgeInstanceByHandle(src, dst, handle)
	g.SeedEdgeHandle(handle + 1)
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

// recoveryListElemMinBytes is the smallest number of bytes one PropList
// element can occupy on the wire: a 1-byte kind plus a 4-byte
// payload-length prefix (the payload itself may be zero bytes). It bounds
// a list capacity hint against the remaining input.
const recoveryListElemMinBytes = 5

// recoveryListCapHint returns a safe capacity hint for a PropList decode
// buffer. count is the untrusted element count from the wire; remaining
// is the number of bytes left to parse. Because each element consumes at
// least [recoveryListElemMinBytes] bytes, the hint is clamped to
// min(count, remaining/recoveryListElemMinBytes), so a hostile count
// cannot trigger a multi-gigabyte eager reservation.
func recoveryListCapHint(count uint32, remaining int) int {
	maxElems := remaining / recoveryListElemMinBytes
	if int64(count) < int64(maxElems) {
		return int(count)
	}
	return maxElems
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
	// count is an untrusted uint32 (up to ~4.3e9). Each element needs at
	// least recoveryListElemMinBytes on the wire, so at most
	// len(buf)/recoveryListElemMinBytes elements can actually follow; clamp
	// the capacity hint to that ceiling so a hostile count cannot drive a
	// multi-GB eager reservation. The loop below still validates and bounds
	// every element.
	elems := make([]lpg.PropertyValue, 0, recoveryListCapHint(count, len(buf)))
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
