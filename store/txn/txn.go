// Package txn provides the transactional surface (Begin / Commit /
// Rollback) layered over an [lpg.Graph] and a [wal.Writer].
//
// A transaction buffers mutations in a per-Tx slice. Commit appends
// each mutation as a WAL frame, then a single [OpCommit] marker frame,
// fsyncs the WAL once, and only then applies the mutations to the
// in-memory graph — so a process crash between Commit's WAL sync and the
// in-memory apply is recoverable by replaying the WAL into a fresh graph.
//
// # Atomicity
//
// Every store writes each op as a v3 ([OpRecordV3]) frame carrying a
// per-Store transaction sequence, followed by an [OpCommit] marker frame
// for the same sequence; recovery buffers a transaction's ops and applies
// them only on reading the durable marker. A crash that tears the batch at
// any point therefore recovers all of the transaction or none of it —
// never a partial node or edge. This is the Atomicity guarantee (see
// docs/acid-audit.md, gap F1).
//
// The legacy v1 (untagged, fmt.Sprintf-based) frame format is no longer
// produced; the v1 constructor has been removed and recovery rejects any
// v1 frame found on disk (see [store/recovery.ErrUnsupportedRecordVersion]).
//
// Single-writer is enforced by a per-store mutex acquired in Begin
// and released in Commit or Rollback; reads on the underlying graph
// remain lock-free in the lpg / adjlist contracts.
//
// # Constructor matrix
//
// The package exposes two constructors that differ only in whether edge
// weights are made durable:
//
//   - [NewStoreWithCodec] — typed N codec, no weight codec; emits only
//     [OpAddEdge]. [Tx.AddEdge] with a non-zero weight returns
//     [ErrNoWeightCodec]; zero-weight calls buffer an [OpAddEdge] record.
//   - [NewStoreWithOptions] — typed N codec plus typed W codec; emits
//     [OpAddEdgeWeighted] for every [Tx.AddEdge] call (the weight payload
//     is written even when the caller passes the zero value of W, so the
//     wire shape stays unambiguous).
package txn

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ErrTxFinished is returned by operations on a transaction that has
// already been committed or rolled back.
var ErrTxFinished = errors.New("txn: transaction already finished")

// ErrTransactionTooLarge is returned by [Tx.Commit] / [Tx.CommitWALOnly]
// when the transaction has buffered more than the store's per-transaction
// op cap (see [DefaultMaxTxnOps] and the maxTxnOps argument of
// [NewStoreWithCodecCapped] / [NewStoreWithOptionsCapped]). The check runs
// in the commit/append path BEFORE any WAL frame is written, so a
// rejected transaction is never made durable — nothing partial reaches
// disk and the caller's in-memory rollback (the Cypher write path's undo
// log, #1282) restores the pre-transaction state.
//
// The cap exists for ACID Durability and the "bounded resources" mandate:
// recovery buffers an entire transaction's ops in memory before applying
// them on the [OpCommit] marker, so a producer that could durably commit
// an arbitrarily large transaction could write a WAL that recovery cannot
// replay without unbounded allocation. The producer cap is therefore
// always <= the recovery cap (both default to [DefaultMaxTxnOps]) so any
// transaction that commits durably is guaranteed to replay; see
// [store/recovery.ErrTransactionTooLarge] for the recovery-side bound.
var ErrTransactionTooLarge = errors.New("txn: transaction exceeds the per-transaction op cap")

// DefaultMaxTxnOps is the default upper bound on the number of ops a single
// transaction may buffer before [Tx.Commit] / [Tx.CommitWALOnly] rejects it
// with [ErrTransactionTooLarge]. It is applied by [NewStoreWithCodec] and
// [NewStoreWithOptions] (the uncapped constructors pass it implicitly), and
// is the same value [store/recovery] uses as its default recovery-side cap,
// so the producer and recovery agree: any transaction the producer commits
// durably is guaranteed to fit within the recovery buffer and replay.
//
// The bound caps the worst case — a single transaction whose op frames
// recovery must buffer in memory before the [OpCommit] marker, or a
// crafted/corrupt marker-less run of valid frames — so neither the producer
// nor recovery allocates proportionally to an unbounded op count. It is set
// high enough that ordinary transactions, the openCypher TCK (tiny
// transactions), and every example stay well below it (it matches the
// engine's sibling pipeline-breaker caps such as [DefaultMaxResultRows],
// with headroom above them so a result-row-capped write still replays);
// callers that genuinely need an unbounded transaction must opt out
// explicitly with [MaxTxnOpsUnlimited].
const DefaultMaxTxnOps = 16_000_000

// MaxTxnOpsUnlimited is the explicit opt-out sentinel for the maxTxnOps
// argument of [NewStoreWithCodecCapped] / [NewStoreWithOptionsCapped]: pass
// this value to disable the per-transaction op cap entirely. It is distinct
// from zero, which selects [DefaultMaxTxnOps]. Use it only when the caller
// can bound transaction size by another means, because an unbounded
// transaction then forces recovery to buffer every op frame in memory
// before applying the batch on its [OpCommit] marker.
const MaxTxnOpsUnlimited = -1

// ErrCommittedNotApplied is returned by [Tx.Commit] when the transaction
// was made durable (its op frames and [OpCommit] marker were written and
// fsynced) but a later in-memory apply step failed — today only reachable
// as [adjlist.ErrShardFull] when the store's graph was built with a
// [adjlist.Config.MaxShardCapacity] cap.
//
// The transaction IS durably committed: it carries a complete commit
// marker, so recovery — which rebuilds the graph without a shard-capacity
// cap — replays it in full and atomically. Callers must therefore treat
// this as "committed; the in-memory view is temporarily behind and will be
// consistent after the next recovery", NOT as a rollback: retrying the
// transaction would commit it a second time. The underlying apply error is
// wrapped and recoverable with [errors.Is]/[errors.Unwrap]. This sentinel
// exists so a durable commit is never reported as a plain, ambiguous
// failure (audit gap F5, see docs/acid-audit.md).
var ErrCommittedNotApplied = errors.New("txn: transaction committed durably but in-memory apply failed; recovery will reconcile")

// OpKind enumerates the mutation kinds supported by a transaction.
type OpKind uint8

// Mutation kinds supported by a transaction. The values are stable
// wire identifiers: legacy unweighted commits stay on [OpAddEdge] so
// pre-T8 readers continue to walk them, and new weighted commits use
// [OpAddEdgeWeighted] so the weight payload sits between the codec-
// encoded endpoints and the trailing label.
const (
	// OpAddEdge buffers an AddEdge(src, dst, _) mutation. The applied
	// weight on the in-memory graph is the zero value of W. This kind
	// is emitted by stores constructed without a weight codec (see
	// [NewStore] and [NewStoreWithCodec]) and by [NewStoreWithOptions]
	// stores when the caller passes the zero W value.
	OpAddEdge OpKind = iota + 1
	// OpSetNodeLabel buffers a SetNodeLabel(node, label) mutation.
	OpSetNodeLabel
	// OpSetEdgeLabel buffers a SetEdgeLabel(src, dst, label) mutation.
	OpSetEdgeLabel
	// OpAddEdgeWeighted buffers an AddEdge(src, dst, w) mutation with
	// a typed weight payload. Only emitted by stores constructed via
	// [NewStoreWithOptions] (which carries a [WeightCodec]). Recovery
	// readers that do not know about [OpAddEdgeWeighted] surface the
	// frame as an unknown kind; readers that do know it walk the
	// weight payload via the registered [WeightCodec] before reading
	// the trailing label.
	OpAddEdgeWeighted

	// OpAddNode buffers an AddNode(key) mutation.
	OpAddNode
	// OpRemoveNode buffers a logical node removal (strips labels and
	// properties; the mapper entry is permanent).
	OpRemoveNode
	// OpRemoveNodeLabel buffers a RemoveNodeLabel(node, label) mutation.
	// The label is carried in the Label field of the Op.
	OpRemoveNodeLabel
	// OpSetNodeProperty buffers a SetNodeProperty(node, key, value) mutation.
	// Key is the property key; Value is the typed property value.
	OpSetNodeProperty
	// OpDelNodeProperty buffers a DelNodeProperty(node, key) mutation.
	// Key is the property key.
	OpDelNodeProperty
	// OpRemoveEdge buffers a RemoveEdge(src, dst) mutation.
	OpRemoveEdge
	// OpSetEdgeProperty buffers a SetEdgeProperty(src, dst, key, value) mutation.
	// Key is the property key; Value is the typed property value.
	OpSetEdgeProperty
	// OpDelEdgeProperty buffers a DelEdgeProperty(src, dst, key) mutation.
	// Key is the property key.
	OpDelEdgeProperty

	// OpCommit is a control record, not a graph mutation. It marks the
	// durable end of a transaction batch in the v3 ([OpRecordV3]) WAL
	// envelope: a commit writes one OpCommit frame, carrying the
	// transaction's sequence number, after all of the transaction's op
	// frames and immediately before the single fsync. Recovery treats it
	// as a no-op against the graph; its sole effect is on the replay state
	// machine, which applies a buffered transaction's ops only when it
	// reads the matching OpCommit. A torn write that loses the OpCommit
	// (or any preceding op frame) causes recovery to discard the whole
	// transaction, giving all-or-nothing atomicity (audit gap F1, see
	// docs/acid-audit.md). OpCommit never appears in a v1/v2 frame.
	OpCommit

	// Stage-2 stable-edge-handle op kinds. They are appended AFTER OpCommit
	// so every pre-existing OpKind value (and OpCommit itself) keeps its
	// stable wire identity; a WAL written by an older binary never carries
	// these kinds, and a reader that predates them surfaces them as unknown
	// kinds. They carry an 8-byte little-endian handle appended after the
	// body a same-kind handle-less op would carry (see the encode helpers).

	// OpAddEdgeH buffers an AddEdge(src, dst, w) mutation carrying a durable
	// stable per-edge handle (see graph/lpg/edge_handle.go). It is the
	// handle-bearing successor of [OpAddEdgeWeighted]: the body is the
	// weighted-edge body followed by the 8-byte handle. The handle is
	// allocated from the graph's monotone handle counter when the op is
	// buffered (so the value is stable in the WAL frame) and is replayed via
	// [lpg.Graph.AddEdgeHIfAbsent] so a snapshot + full-WAL recovery does
	// not double the edge. Emitted by [Tx.AddEdge] on a weight-codec store.
	OpAddEdgeH
	// OpSetEdgeLabelByHandle buffers a SetEdgeLabelByHandle(src, dst,
	// handle, label) mutation, persisting one parallel edge's per-CREATE
	// type so it survives recovery keyed to the stable handle rather than
	// collapsing into the per-pair union. The body is the edge-with-label
	// body followed by the 8-byte handle.
	OpSetEdgeLabelByHandle
	// OpSetEdgePropertyByHandle buffers a SetEdgePropertyByHandle(src, dst,
	// handle, key, value) mutation, persisting one parallel edge's
	// per-CREATE property. The body is the edge-property body followed by
	// the 8-byte handle.
	OpSetEdgePropertyByHandle
	// OpRemoveEdgeInstanceByHandle buffers a RemoveEdgeInstanceByHandle(src,
	// dst, handle) mutation, dropping one logical edge's per-handle metadata
	// on DELETE while leaving sibling handles untouched. The body is the
	// edge-no-tail body followed by the 8-byte handle.
	OpRemoveEdgeInstanceByHandle

	// Schema-DDL op kinds. They are appended AFTER the Stage-2 handle ops so
	// every pre-existing OpKind value keeps its stable wire identity; a WAL
	// written by an older binary never carries these kinds, and a reader that
	// predates them surfaces them as unknown kinds. Unlike every mutation kind
	// above, a constraint op carries NO node-identifier endpoints — it is a
	// schema record, so its body is three length-prefixed strings (label,
	// property, name) preceded by a one-byte [ConstraintKind] tag, encoded
	// independently of the node [Codec] (see [appendOpConstraintBody]). The
	// recovery decoder surfaces the record via [store/recovery.Result] rather
	// than applying it to the graph, because constraint definitions are engine
	// schema, not graph topology.

	// OpCreateConstraint buffers a CREATE CONSTRAINT schema change: it records
	// that a UNIQUE or NOT NULL constraint named Name is declared on
	// (Label).Property. It is replayed on recovery to re-register the
	// constraint in the engine's registry. The op mutates no graph state.
	OpCreateConstraint
	// OpDropConstraint buffers a DROP CONSTRAINT schema change: it records that
	// the constraint on (Label).Property (kind ConstraintKind) named Name is
	// removed. It is replayed on recovery to suppress an earlier
	// OpCreateConstraint for the same key. The op mutates no graph state.
	OpDropConstraint
	// OpCreateIndex buffers a CREATE INDEX schema change: it records that a
	// secondary index named Name of the given IndexKind is built on
	// (Label).Property. It is replayed on recovery to re-register and re-backfill
	// the index in the index.Manager, so a user-created index survives a crash
	// and a restart (Durability + Consistency). The op mutates no graph state.
	OpCreateIndex
	// OpDropIndex buffers a DROP INDEX schema change: it records that the named
	// index is removed. It is replayed on recovery to suppress an earlier
	// OpCreateIndex for the same name. The op mutates no graph state.
	OpDropIndex
)

// ConstraintKind is the wire tag distinguishing UNIQUE from NOT NULL in an
// [OpCreateConstraint] / [OpDropConstraint] body. The values are stable wire
// identifiers and must not be reordered or reused; they mirror the engine-side
// exec.ConstraintKind without importing that package (the store layer stays
// decoupled from the Cypher executor).
type ConstraintKind uint8

const (
	// ConstraintUnique tags a UNIQUE constraint.
	ConstraintUnique ConstraintKind = iota
	// ConstraintNotNull tags a NOT NULL constraint.
	ConstraintNotNull
)

// IndexKind is the wire tag distinguishing hash from btree in an
// [OpCreateIndex] / [OpDropIndex] body. The values are stable wire identifiers
// and must not be reordered or reused; they mirror the IR-side ir.IndexType
// without importing that package (the store layer stays decoupled from the
// Cypher executor).
type IndexKind uint8

const (
	// IndexKindHash is a hash-based exact-match index.
	IndexKindHash IndexKind = iota
	// IndexKindBTree is a B-tree range index.
	IndexKindBTree
)

// Op-record version markers. The marker is a single byte written at
// offset zero of every v2 WAL payload. v1 records have no marker —
// their first byte is the [OpKind] value (always 1..3 today, with
// room to grow into the low region of the byte space). We pick a v2
// marker far outside the [OpKind] range so a v1-vs-v2 reader can
// disambiguate by peeking the first byte: any payload that starts
// with OpRecordV2 is necessarily a v2 frame because no legitimate
// OpKind value reaches 0xFE.
//
// 0xFE is chosen specifically because it leaves 0x00..0x0F free for
// future OpKind growth, is not a printable ASCII character (so
// hex-dumped logs are visually unambiguous), and is one less than the
// universally-recognised "all bits set" sentinel 0xFF — leaving room
// for at least one further version bump (e.g. OpRecordV3 = 0xFD) in
// the same disambiguation scheme.
const (
	// OpRecordV1 is the reserved logical version of the legacy untagged
	// record format. This format is no longer produced — the v1 store
	// constructor and its encoder were removed — and any v1 frame found
	// on disk is rejected on read by [store/recovery.Decode] with
	// [store/recovery.ErrUnsupportedRecordVersion]. The constant is
	// retained (value 0) as a RESERVED sentinel so the rejection path and
	// its tests can name the version they refuse; it is never written to
	// disk and must not be reused for a new record version.
	OpRecordV1 uint8 = 0
	// OpRecordV2 is the magic byte that marks the start of a v2-tagged
	// op record. See the package doc above for the rationale.
	OpRecordV2 uint8 = 0xFE
	// OpRecordV3 is the magic byte that marks the start of a v3-tagged
	// op record. A v3 payload is laid out as:
	//
	//	uint8  version (OpRecordV3 = 0xFD)
	//	uint8  kind    (an [OpKind], or [OpCommit] for the commit marker)
	//	uint64 txnSeq  little-endian per-Store transaction sequence
	//	...    the same body bytes a v2 record of this kind carries...
	//
	// v3 adds the txnSeq word and the [OpCommit] marker so a multi-op
	// transaction is recovered atomically: recovery buffers a v3
	// transaction's ops and applies them only on reading the matching
	// OpCommit. The body after the txnSeq word is byte-identical to the
	// v2 body for the same kind, so the recovery decoder reuses the v2
	// body walk. 0xFD is the value reserved for OpRecordV3 in the
	// disambiguation scheme documented above; a v1/v2/v3 reader peeks the
	// first byte to select the decoder.
	OpRecordV3 uint8 = 0xFD
)

// codecHolder is the type-erased view of [Codec] used by Store so the
// Store struct itself carries the codec without re-parameterising on the
// concrete codec implementation. Methods on the holder are called from
// the Commit fast path; the indirection is a single interface dispatch
// per op.
type codecHolder[N comparable] interface {
	Codec[N]
}

// Options carries the codecs used by [NewStoreWithOptions]. Both
// fields are required: Codec serialises endpoint identifiers and
// WeightCodec serialises edge weights for [OpAddEdgeWeighted] records.
//
// A nil WeightCodec is rejected by [NewStoreWithOptions]; callers that
// do not need durable weights should use [NewStoreWithCodec] instead.
type Options[N comparable, W any] struct {
	// Codec serialises endpoint identifiers. Must not be nil.
	Codec Codec[N]
	// WeightCodec serialises edge weights. Must not be nil.
	WeightCodec WeightCodec[W]
}

// Store bundles an [lpg.Graph] with a [wal.Writer] and the single-
// writer lock that serialises transactions.
//
// Concurrency: any number of goroutines may call Begin/BeginCtx;
// transactions serialise on a single-writer semaphore, so only one Tx is
// active at any moment. Reads on the underlying lpg.Graph remain
// concurrent and lock-free per the lpg/adjlist contracts.
// [Store.RunUnderCommitLock] runs a closure while holding that same
// commit semaphore, so a background checkpointer can exclude the commit
// window while it snapshots and truncates the WAL.
//
// The single-writer lock is a buffered channel of capacity one used as a
// binary semaphore rather than a [sync.Mutex], so the acquire is
// cancellable: [Store.BeginCtx] selects the acquire against ctx.Done() and
// returns the context error without blocking for the holder's full
// duration. A [sync.Mutex] cannot honour a deadline while it is contended;
// the semaphore can, which is what makes the engine write path
// ([cypher.Engine.RunInTx]) respect a caller's deadline under write
// contention. Capacity one preserves exact mutual exclusion: a second
// acquire blocks (or fails with ctx) until the holder releases, so the
// single-writer contract is identical to the previous mutex.
type Store[N comparable, W any] struct {
	// sem is the single-writer semaphore: a buffered channel of capacity
	// one. A send acquires the writer (Begin / BeginCtx / RunUnderCommitLock);
	// a receive releases it (Tx.release / RunUnderCommitLock's defer). It is
	// allocated once at construction ([newStore]); a zero-value Store is not
	// usable. See [Store.acquire] / [Store.release].
	sem    chan struct{}
	g      *lpg.Graph[N, W]
	wal    *wal.Writer
	codec  codecHolder[N]
	wcodec WeightCodec[W]

	// txnSeq is the last assigned transaction sequence number. A
	// Commit/CommitWALOnly increments it once and stamps the
	// value into every v3 op frame and the trailing [OpCommit] marker, so
	// recovery can group a transaction's frames and apply them atomically.
	// It is incremented only while the single-writer semaphore is held (the
	// lock acquired in Begin), so the atomic type is for safe publication
	// rather than contended access.
	txnSeq atomic.Uint64

	// maxTxnOps is the per-transaction op cap enforced in the commit/append
	// path: a transaction buffering more than this many ops is rejected with
	// [ErrTransactionTooLarge] BEFORE any WAL frame is written, so it is
	// never made durable. The value is normalised at construction time
	// ([resolveMaxTxnOps]): 0 (the uncapped constructors' implicit value)
	// becomes [DefaultMaxTxnOps], and [MaxTxnOpsUnlimited] becomes 0 here,
	// meaning "no cap". It is set once in the constructor and read-only
	// thereafter, so it needs no synchronisation. Keeping the producer cap
	// <= the recovery cap guarantees every durably-committed transaction
	// replays within recovery's buffer (audit gap: bounded resources).
	maxTxnOps int

	// --- group-commit apply gate (#1507) ---
	//
	// Group commit releases the single-writer semaphore after a transaction's
	// frames are appended (so the next transaction can append while this one
	// fsyncs), and the fsync itself is coalesced across committers by
	// [wal.Writer.SyncGroup]. But [Tx.Commit]'s post-durability in-memory apply
	// ([applyOp] under [lpg.Graph.ApplyAtomically]) must still run in
	// transaction-sequence order: applying a higher-seq transaction before a
	// lower-seq one could materialise an op against a node a not-yet-applied
	// earlier transaction was to create (lpg property writes are
	// create-on-demand), letting a [lpg.Graph.View] reader observe a state no
	// serial schedule produces — a Consistency/Isolation regression. The apply
	// gate restores that order WITHOUT holding the append semaphore across the
	// fsync: a committer waits until appliedSeq == its seq-1, applies, then
	// advances appliedSeq and wakes the next committer.
	//
	// applyMu guards appliedSeq and is the locker of applyCond.
	applyMu sync.Mutex
	// applyCond signals committers when appliedSeq advances.
	applyCond *sync.Cond
	// appliedSeq is the highest transaction sequence whose post-durability
	// in-memory apply step has completed (or been skipped, for a durable txn
	// whose apply failed or whose path performs no apply). A committer holding
	// sequence seq waits until appliedSeq == seq-1 before applying, then sets
	// appliedSeq = seq. It is advanced for EVERY consumed sequence — including a
	// transaction whose fsync failed or whose apply errored — so a failed
	// transaction never wedges the apply chain behind it.
	appliedSeq uint64

	// --- in-flight commit tracker (#1507 quiesce boundary) ---
	//
	// Group commit releases the single-writer semaphore after the append phase
	// but BEFORE the coalesced fsync ([wal.Writer.SyncGroup]) and the
	// sequence-ordered apply. The semaphore therefore no longer bounds the
	// in-flight-fsync window, but [Store.RunUnderCommitLock] — the seam
	// [store.DB] and a checkpointer use to exclude the commit path while they
	// close or truncate the WAL — relied on the semaphore bounding it. Without a
	// separate tracker, RunUnderCommitLock could acquire the semaphore while a
	// committer is parked inside SyncGroup, and the caller's fn (e.g. wal.Close)
	// would then race that in-flight flush+fsync, making un-acknowledged frames
	// durable (an acked != durable violation).
	//
	// inflight counts committers that have released the semaphore but not yet
	// finished their SyncGroup + apply. A committer increments it WHILE STILL
	// HOLDING the semaphore (in markInflight, before releaseAfterAppend), so
	// once the semaphore is free the increment is already visible to a
	// RunUnderCommitLock that then acquires it; the committer decrements (and
	// broadcasts when reaching zero) only after its entire commit finishes
	// (doneInflight). The increment MUST happen-before the release; reversing
	// them reopens the race.
	inflightMu   sync.Mutex
	inflightCond *sync.Cond
	inflight     int
}

// resolveMaxTxnOps normalises the maxTxnOps constructor argument to the
// internal convention used by [Store.maxTxnOps]: 0 (the implicit value the
// uncapped constructors pass) selects [DefaultMaxTxnOps]; [MaxTxnOpsUnlimited]
// (-1) selects 0, meaning "no cap"; any other positive value is taken
// verbatim. This mirrors the engine's sibling pipeline-breaker cap
// resolvers (e.g. cypher.resolveMaxResultRows).
func resolveMaxTxnOps(maxTxnOps int) int {
	switch maxTxnOps {
	case 0:
		return DefaultMaxTxnOps
	case MaxTxnOpsUnlimited:
		return 0
	default:
		return maxTxnOps
	}
}

// NewStoreWithCodec returns a Store wrapping g and wal that encodes
// node identifiers via the supplied typed [Codec]. Each transaction is
// emitted as v3-tagged frames: a one-byte version tag ([OpRecordV3]),
// the [OpKind], the per-transaction sequence, then the codec-encoded
// src and dst values inline, then a uint16 little-endian label length
// and the label bytes — one frame per op, followed by an [OpCommit]
// marker so recovery applies the transaction atomically. The body is
// the dual of the v3 branch in [store/recovery.Decode], which detects
// the version tag and walks the body back through the same codec.
//
// codec must not be nil.
//
// The returned store has no [WeightCodec]; [Tx.AddEdge] called with a
// non-zero weight returns [ErrNoWeightCodec]. Callers that need
// durable weighted edges should use [NewStoreWithOptions].
func NewStoreWithCodec[N comparable, W any](g *lpg.Graph[N, W], wlog *wal.Writer, codec Codec[N]) *Store[N, W] {
	defer metrics.Time("store.txn.NewStoreWithCodec")()
	return NewStoreWithCodecCapped(g, wlog, codec, 0)
}

// NewStoreWithCodecCapped is [NewStoreWithCodec] with an explicit
// per-transaction op cap. maxTxnOps follows the standard convention: 0
// selects [DefaultMaxTxnOps], [MaxTxnOpsUnlimited] disables the cap, and any
// other positive value is the cap verbatim. A transaction buffering more
// than the resolved cap is rejected by [Tx.Commit] / [Tx.CommitWALOnly] with
// [ErrTransactionTooLarge] before any WAL frame is written.
//
// codec must not be nil. The returned store has no [WeightCodec]; see
// [NewStoreWithCodec] for the weight-handling contract.
func NewStoreWithCodecCapped[N comparable, W any](g *lpg.Graph[N, W], wlog *wal.Writer, codec Codec[N], maxTxnOps int) *Store[N, W] {
	defer metrics.Time("store.txn.NewStoreWithCodecCapped")()
	s := &Store[N, W]{
		sem:       make(chan struct{}, 1),
		g:         g,
		wal:       wlog,
		codec:     codec,
		maxTxnOps: resolveMaxTxnOps(maxTxnOps),
	}
	s.applyCond = sync.NewCond(&s.applyMu)
	s.inflightCond = sync.NewCond(&s.inflightMu)
	return s
}

// NewStoreWithOptions returns a Store wrapping g and wal that encodes
// node identifiers via opts.Codec and edge weights via opts.WeightCodec.
// Each WAL payload is emitted in the v2 format. Weighted [Tx.AddEdge]
// calls produce [OpAddEdgeWeighted] frames whose body is laid out as:
//
//	uint8  version  ([OpRecordV2])
//	uint8  kind     ([OpAddEdgeWeighted])
//	codec  src
//	codec  dst
//	wcodec w
//	uint16 labelLen (always 0 for AddEdge)
//
// Calls to [Tx.AddEdge] that pass the zero value of W still buffer an
// [OpAddEdge] record (without a weight payload), which preserves
// backwards-compatible replay under readers that predate
// [OpAddEdgeWeighted].
//
// opts.Codec must not be nil; opts.WeightCodec must not be nil.
// Passing the legacy fmt codec via opts.Codec is undefined behaviour.
func NewStoreWithOptions[N comparable, W any](g *lpg.Graph[N, W], wlog *wal.Writer, opts Options[N, W]) *Store[N, W] {
	defer metrics.Time("store.txn.NewStoreWithOptions")()
	return NewStoreWithOptionsCapped(g, wlog, opts, 0)
}

// NewStoreWithOptionsCapped is [NewStoreWithOptions] with an explicit
// per-transaction op cap. maxTxnOps follows the standard convention: 0
// selects [DefaultMaxTxnOps], [MaxTxnOpsUnlimited] disables the cap, and any
// other positive value is the cap verbatim. A transaction buffering more
// than the resolved cap is rejected by [Tx.Commit] / [Tx.CommitWALOnly] with
// [ErrTransactionTooLarge] before any WAL frame is written.
//
// opts.Codec and opts.WeightCodec must not be nil.
func NewStoreWithOptionsCapped[N comparable, W any](g *lpg.Graph[N, W], wlog *wal.Writer, opts Options[N, W], maxTxnOps int) *Store[N, W] {
	defer metrics.Time("store.txn.NewStoreWithOptionsCapped")()
	s := &Store[N, W]{
		sem:       make(chan struct{}, 1),
		g:         g,
		wal:       wlog,
		codec:     opts.Codec,
		wcodec:    opts.WeightCodec,
		maxTxnOps: resolveMaxTxnOps(maxTxnOps),
	}
	s.applyCond = sync.NewCond(&s.applyMu)
	s.inflightCond = sync.NewCond(&s.inflightMu)
	return s
}

// Codec returns the [Codec] installed on the Store. The returned value
// is the same one passed to [NewStoreWithCodec] or [NewStoreWithOptions].
// Callers should treat the return as read-only.
func (s *Store[N, W]) Codec() Codec[N] { return s.codec }

// WeightCodec returns the [WeightCodec] installed on the Store, or nil
// if the store was constructed without one. Callers should treat the
// return as read-only.
func (s *Store[N, W]) WeightCodec() WeightCodec[W] { return s.wcodec }

// MaxTxnOps returns the resolved per-transaction op cap enforced by
// [Tx.Commit] / [Tx.CommitWALOnly], or 0 when the cap is disabled
// ([MaxTxnOpsUnlimited]). The uncapped constructors resolve to
// [DefaultMaxTxnOps]. A transaction buffering more than a non-zero cap is
// rejected with [ErrTransactionTooLarge] before any WAL frame is written.
func (s *Store[N, W]) MaxTxnOps() int { return s.maxTxnOps }

// Graph returns the underlying mutable graph.
//
// Reads through the returned [lpg.Graph] are partial-transaction-free and
// cross-substructure-consistent ONLY when performed inside [lpg.Graph.View].
// A direct accessor call made outside View (HasNodeLabel, NodeProperties,
// AdjList().Neighbours, NodeIndex().Scan, and the like) remains per-operation
// atomic, but may observe a multi-operation transaction half-applied — for
// example the edge of an edge-plus-labels write before its endpoint labels.
// An embedding application that reads this graph concurrently with committing
// transactions must therefore wrap its reads in [lpg.Graph.View] to obtain the
// no-partial-transaction guarantee; writes go through the [Tx] API, which
// applies them under the same [lpg.Graph.ApplyAtomically] barrier.
//
// See the lpg package documentation and docs/isolation-design.md for the full
// opt-in contract and the tracked lock-free per-shard snapshot that will make
// every read transaction-consistent without the barrier.
func (s *Store[N, W]) Graph() *lpg.Graph[N, W] { return s.g }

// acquire takes the single-writer semaphore, honouring ctx. It first
// fails fast if ctx is already done (so an already-cancelled caller never
// acquires even when the semaphore is free — the select below would
// otherwise pick a ready case pseudo-randomly), then blocks on a send into
// the capacity-one channel, racing it against ctx.Done(). On cancellation
// it returns ctx.Err() WITHOUT having acquired, so there is nothing to
// release. A nil return means the writer is held and the caller must
// eventually call [Store.release] exactly once.
func (s *Store[N, W]) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release frees the single-writer semaphore. It must be called exactly
// once for every successful [Store.acquire]; calling it without a prior
// successful acquire would let a second writer in and break mutual
// exclusion. The receive cannot block: the channel holds exactly one token
// while the writer is held.
func (s *Store[N, W]) release() { <-s.sem }

// RunUnderCommitLock runs fn while holding the store's single-writer
// commit lock — the SAME semaphore [Store.Begin] acquires and
// [Tx.Commit]/[Tx.CommitWALOnly]/[Tx.Rollback] release. While fn runs no
// transaction can be between Begin and its commit/rollback: neither a new
// in-memory apply (the [lpg.Graph.ApplyAtomically] window opened inside a
// transaction) nor a new WAL frame append can race fn.
//
// This is the serialisation seam a background checkpointer needs to take a
// consistent snapshot and truncate the WAL atomically against the commit
// path: the store lock is otherwise private, so an external checkpointer
// wired only with its own mutex would never exclude the engine's eager
// write+commit window (see store/checkpoint and docs/acid-audit.md F3.5).
//
// The acquire is uncancellable (it uses a background context), matching the
// previous mutex semantics: a checkpointer that takes this lock blocks until
// the active writer releases. fn must not call [Store.Begin]/[Store.BeginCtx]
// or open a transaction on this store (the lock is not re-entrant — that
// would deadlock). fn MAY read the graph through [lpg.Graph.View]; the
// resulting lock order is store-lock → visMu, which matches the engine's own
// order (Begin acquires the store lock, then ApplyAtomically acquires visMu),
// so no new deadlock is introduced. fn's error is returned unwrapped.
//
// Concurrency: safe to call from any goroutine; it serialises against every
// transaction on the store.
func (s *Store[N, W]) RunUnderCommitLock(fn func() error) error {
	defer metrics.Time("store.txn.RunUnderCommitLock")()
	// acquire(context.Background()) cannot fail, so the held token is
	// guaranteed and the deferred release is always paired with it.
	_ = s.acquire(context.Background())
	defer s.release()
	// Drain in-flight group commits before running fn. Holding the semaphore
	// excludes any NEW commit from appending and bumping inflight; once the
	// semaphore is held, every committer that already released it has its
	// inflight increment visible (the increment happens-before the release).
	// Waiting for inflight==0 therefore guarantees no SyncGroup (flush+fsync)
	// is in flight when fn (e.g. wal.Close / wal.Truncate) runs, restoring the
	// quiesce boundary the semaphore alone no longer provides under group
	// commit. The wait is uncancellable, matching the acquire above.
	s.drainInflight()
	return fn()
}

// markInflight registers a committer as in-flight (past the append phase,
// pending its SyncGroup + apply). It MUST be called while the single-writer
// semaphore is still held, immediately before [Tx.releaseAfterAppend], so the
// increment happens-before the release and is visible to any
// [Store.RunUnderCommitLock] that subsequently acquires the semaphore.
func (s *Store[N, W]) markInflight() {
	s.inflightMu.Lock()
	s.inflight++
	s.inflightMu.Unlock()
}

// doneInflight clears the in-flight registration of a committer that has
// finished its entire commit (SyncGroup returned and the apply gate advanced).
// It broadcasts when the count reaches zero so a draining
// [Store.RunUnderCommitLock] is woken. It must be called exactly once for every
// [Store.markInflight].
func (s *Store[N, W]) doneInflight() {
	s.inflightMu.Lock()
	s.inflight--
	if s.inflight == 0 {
		s.inflightCond.Broadcast()
	}
	s.inflightMu.Unlock()
}

// drainInflight blocks until no group commit is in flight. The caller must hold
// the single-writer semaphore so no new commit can start; the wait is
// uncancellable.
func (s *Store[N, W]) drainInflight() {
	s.inflightMu.Lock()
	for s.inflight != 0 {
		s.inflightCond.Wait()
	}
	s.inflightMu.Unlock()
}

// Begin opens a new transaction. The returned Tx holds the
// store's single-writer lock until Commit or Rollback runs. The acquire is
// uncancellable; callers that need a deadline must use [Store.BeginCtx].
func (s *Store[N, W]) Begin() *Tx[N, W] {
	defer metrics.Time("store.txn.Begin")()
	tx, _ := s.BeginCtx(context.Background())
	return tx
}

// BeginCtx is the context-aware variant of [Store.Begin]. The single-writer
// lock is a capacity-one semaphore, so the acquire itself is cancellable:
// BeginCtx selects the acquire against ctx.Done() and returns (nil, ctx.Err())
// the instant ctx is cancelled or its deadline elapses — even while another
// writer holds the lock — rather than blocking for the holder's full
// duration. This is what lets a deadline-bearing engine write
// ([cypher.Engine.RunInTx]) honour its deadline under write contention. On a
// nil error the returned Tx holds the lock until Commit or Rollback runs;
// once held, further ctx checks happen at the caller's discretion.
func (s *Store[N, W]) BeginCtx(ctx context.Context) (*Tx[N, W], error) {
	defer metrics.Time("store.txn.BeginCtx")()
	if err := s.acquire(ctx); err != nil {
		metrics.IncCounter("store.txn.BeginCtx.errors", 1)
		return nil, err
	}
	return &Tx[N, W]{store: s}, nil
}

// Op is a single buffered mutation.
//
// The type carries the endpoint identifiers (Src, Dst), the edge weight
// (Weight), a string Label used by label ops, and Key / Value used by
// property ops. Fields are zero-valued for op kinds that do not require them.
type Op[N comparable, W any] struct {
	Kind     OpKind
	Src, Dst N
	Weight   W
	Label    string
	// Key is the property key for SetNodeProperty, DelNodeProperty,
	// SetEdgeProperty, and DelEdgeProperty ops.
	Key string
	// Value is the typed property value for SetNodeProperty and SetEdgeProperty
	// ops. It is the zero PropertyValue for all other op kinds.
	Value lpg.PropertyValue
	// Handle is the stable per-edge handle carried by the Stage-2
	// handle-bearing op kinds ([OpAddEdgeH], [OpSetEdgeLabelByHandle],
	// [OpSetEdgePropertyByHandle], [OpRemoveEdgeInstanceByHandle]). It is 0
	// for every other op kind.
	Handle uint64
	// ConstraintName is the user-defined constraint name carried by the
	// schema-DDL op kinds ([OpCreateConstraint], [OpDropConstraint]). For
	// those kinds Label holds the constrained node label and Key holds the
	// constrained property; ConstraintKind selects UNIQUE vs NOT NULL. It is
	// the empty string for every other op kind.
	ConstraintName string
	// ConstraintKind selects UNIQUE vs NOT NULL for the schema-DDL op kinds.
	// It is the zero value ([ConstraintUnique]) and ignored for every other
	// op kind.
	ConstraintKind ConstraintKind
	// IndexKind selects hash vs btree for the index schema-DDL op kinds
	// ([OpCreateIndex], [OpDropIndex]). It is the zero value ([IndexKindHash])
	// and ignored for every other op kind.
	IndexKind IndexKind
}

// Tx is an in-progress transaction. It holds the store's single-writer
// lock from [Store.Begin] / [Store.BeginCtx] until [Tx.Commit] or
// [Tx.Rollback] runs, and buffers its mutations in an unsynchronised
// slice. A Tx is therefore NOT safe for concurrent use: it is owned by
// the single goroutine that opened it, which must drive every operation
// and the terminal Commit/Rollback. Distinct transactions are serialised
// by the single-writer lock, so they never run concurrently.
type Tx[N comparable, W any] struct {
	store    *Store[N, W]
	ops      []Op[N, W]
	finished bool
}

// AddEdge buffers an AddEdge(src, dst, w) operation on the graph.
//
// The operation is always recorded as a handle-bearing [OpAddEdgeH]
// frame, so the edge keeps a stable per-edge identity across recovery
// and replays idempotently over a snapshot that already restored it
// (no doubled parallel edge on a multigraph). If the store was
// constructed with a [WeightCodec] (via [NewStoreWithOptions]) the
// frame carries w; a store without a weight codec (built with
// [NewStoreWithCodec]) accepts only a zero-value w — the frame omits
// the weight bytes and replay produces a zero-weight edge — and AddEdge
// returns [ErrNoWeightCodec] for any non-zero w. Callers needing
// durable weighted edges must use [NewStoreWithOptions].
func (t *Tx[N, W]) AddEdge(src, dst N, w W) error {
	if t.finished {
		return ErrTxFinished
	}
	// The handle is minted now, from the graph's monotone counter, so the
	// value is fixed in the WAL frame before the commit fsync; replay
	// re-inserts it via AddEdgeHIfAbsent (idempotent against a snapshot
	// that already loaded it).
	if t.store.wcodec == nil {
		if !isZero(w) {
			return ErrNoWeightCodec
		}
		handle := t.store.g.NextEdgeHandle()
		t.ops = append(t.ops, Op[N, W]{Kind: OpAddEdgeH, Src: src, Dst: dst, Handle: handle})
		return nil
	}
	handle := t.store.g.NextEdgeHandle()
	t.ops = append(t.ops, Op[N, W]{Kind: OpAddEdgeH, Src: src, Dst: dst, Weight: w, Handle: handle})
	return nil
}

// SetNodeLabel buffers a SetNodeLabel(node, label) operation.
func (t *Tx[N, W]) SetNodeLabel(node N, label string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpSetNodeLabel, Src: node, Label: label})
	return nil
}

// SetEdgeLabel buffers a SetEdgeLabel(src, dst, label) operation.
// The underlying edge must exist at apply time; otherwise the
// underlying SetEdgeLabel call is a documented no-op.
func (t *Tx[N, W]) SetEdgeLabel(src, dst N, label string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpSetEdgeLabel, Src: src, Dst: dst, Label: label})
	return nil
}

// AddNode buffers an AddNode(key) operation that interns key into the graph.
func (t *Tx[N, W]) AddNode(key N) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpAddNode, Src: key})
	return nil
}

// RemoveNode buffers a logical node removal: strips all labels and properties
// from key. The mapper entry is permanent; this op records the intent so WAL
// replay can reproduce the stripped state.
func (t *Tx[N, W]) RemoveNode(key N) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpRemoveNode, Src: key})
	return nil
}

// RemoveNodeLabel buffers a RemoveNodeLabel(node, label) operation.
func (t *Tx[N, W]) RemoveNodeLabel(node N, label string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpRemoveNodeLabel, Src: node, Label: label})
	return nil
}

// SetNodeProperty buffers a SetNodeProperty(node, propKey, value) operation.
func (t *Tx[N, W]) SetNodeProperty(node N, propKey string, value lpg.PropertyValue) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpSetNodeProperty, Src: node, Key: propKey, Value: value})
	return nil
}

// DelNodeProperty buffers a DelNodeProperty(node, propKey) operation.
func (t *Tx[N, W]) DelNodeProperty(node N, propKey string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpDelNodeProperty, Src: node, Key: propKey})
	return nil
}

// RemoveEdge buffers a RemoveEdge(src, dst) operation.
func (t *Tx[N, W]) RemoveEdge(src, dst N) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpRemoveEdge, Src: src, Dst: dst})
	return nil
}

// SetEdgeProperty buffers a SetEdgeProperty(src, dst, propKey, value) operation.
func (t *Tx[N, W]) SetEdgeProperty(src, dst N, propKey string, value lpg.PropertyValue) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpSetEdgeProperty, Src: src, Dst: dst, Key: propKey, Value: value})
	return nil
}

// DelEdgeProperty buffers a DelEdgeProperty(src, dst, propKey) operation.
func (t *Tx[N, W]) DelEdgeProperty(src, dst N, propKey string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpDelEdgeProperty, Src: src, Dst: dst, Key: propKey})
	return nil
}

// AddEdgeWithHandle buffers an [OpAddEdgeH] operation: an AddEdge(src, dst,
// w) carrying the supplied durable stable per-edge handle. The handle must
// be a value the caller minted from the graph's [lpg.Graph.NextEdgeHandle]
// counter (or replayed from a durable record); it is written verbatim into
// the WAL frame and re-inserted on recovery via
// [lpg.Graph.AddEdgeHIfAbsent]. Used by the Cypher write path
// (walMutatorAdapter) so a parallel CREATE's handle is durable; the direct
// [Tx.AddEdge] path mints its own handle. Requires a weight codec.
func (t *Tx[N, W]) AddEdgeWithHandle(src, dst N, w W, handle uint64) error {
	if t.finished {
		return ErrTxFinished
	}
	if t.store.wcodec == nil {
		return ErrNoWeightCodec
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpAddEdgeH, Src: src, Dst: dst, Weight: w, Handle: handle})
	return nil
}

// SetEdgeLabelByHandle buffers an [OpSetEdgeLabelByHandle] operation,
// persisting `label` against one parallel edge's stable `handle` on the
// (src, dst) pair so the per-CREATE type survives recovery.
func (t *Tx[N, W]) SetEdgeLabelByHandle(src, dst N, handle uint64, label string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpSetEdgeLabelByHandle, Src: src, Dst: dst, Handle: handle, Label: label})
	return nil
}

// SetEdgePropertyByHandle buffers an [OpSetEdgePropertyByHandle] operation,
// persisting key=value against one parallel edge's stable `handle` on the
// (src, dst) pair.
func (t *Tx[N, W]) SetEdgePropertyByHandle(src, dst N, handle uint64, propKey string, value lpg.PropertyValue) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpSetEdgePropertyByHandle, Src: src, Dst: dst, Handle: handle, Key: propKey, Value: value})
	return nil
}

// RemoveEdgeInstanceByHandle buffers an [OpRemoveEdgeInstanceByHandle]
// operation, dropping the per-handle label and property metadata for one
// logical edge on DELETE while leaving sibling handles untouched.
func (t *Tx[N, W]) RemoveEdgeInstanceByHandle(src, dst N, handle uint64) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpRemoveEdgeInstanceByHandle, Src: src, Dst: dst, Handle: handle})
	return nil
}

// CreateConstraint buffers an [OpCreateConstraint] schema change recording that
// a constraint of the given kind, named name, is declared on (label).property.
// The op carries no node endpoints and mutates no graph state on
// [Tx.Commit]; its sole effect is the durable WAL record that
// [store/recovery.Open] replays to re-register the constraint in the engine's
// registry, so a constraint created before a crash survives recovery
// (Durability + Consistency).
func (t *Tx[N, W]) CreateConstraint(kind ConstraintKind, label, property, name string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpCreateConstraint, ConstraintKind: kind, Label: label, Key: property, ConstraintName: name})
	return nil
}

// DropConstraint buffers an [OpDropConstraint] schema change recording that the
// constraint of the given kind, named name, on (label).property is removed. On
// recovery the record suppresses an earlier [OpCreateConstraint] for the same
// key. The op carries no node endpoints and mutates no graph state.
func (t *Tx[N, W]) DropConstraint(kind ConstraintKind, label, property, name string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpDropConstraint, ConstraintKind: kind, Label: label, Key: property, ConstraintName: name})
	return nil
}

// CreateIndex buffers an [OpCreateIndex] schema change recording that a
// secondary index named name of the given kind is declared on
// (label).property. The op carries no node endpoints and mutates no graph
// state on [Tx.Commit]; its sole effect is the durable WAL record that
// [store/recovery.Open] replays to re-register and re-backfill the index,
// so a user-created index survives a crash and a restart.
func (t *Tx[N, W]) CreateIndex(kind IndexKind, label, property, name string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpCreateIndex, IndexKind: kind, Label: label, Key: property, ConstraintName: name})
	return nil
}

// DropIndex buffers an [OpDropIndex] schema change recording that the named
// index is removed. On recovery the record suppresses an earlier
// [OpCreateIndex] for the same name. The op carries no node endpoints and
// mutates no graph state.
func (t *Tx[N, W]) DropIndex(name string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpDropIndex, ConstraintName: name})
	return nil
}

// Commit durably appends every buffered op to the WAL and only then
// applies it to the in-memory graph.
//
// For a typed store the whole batch is committed atomically: every op is
// written as a v3 frame carrying one transaction sequence, followed by an
// [OpCommit] marker frame, then a single fsync. Recovery applies the
// transaction only on reading the durable marker, so a crash that tears
// the batch at any point recovers all of the transaction or none of it
// (audit gap F1, see docs/acid-audit.md).
func (t *Tx[N, W]) Commit() error {
	defer metrics.Time("store.txn.Commit")()
	if t.finished {
		metrics.IncCounter("store.txn.Commit.errors", 1)
		return ErrTxFinished
	}

	// Group-commit phase 1 — APPEND under the single-writer semaphore: cap
	// check, mint the transaction sequence, encode and append every op frame
	// plus the OpCommit marker. The semaphore is released the instant the
	// append completes (releaseAfterAppend) so the next transaction can Begin
	// and append while this one fsyncs — the precondition for fsync coalescing
	// (#1507). The on-disk frame order is unchanged (still serialised by the
	// semaphore in sequence order).
	seq, hasSeq, appendErr := t.appendOnly()
	// Pair the in-flight registration appendOnly made: cleared only after the
	// entire commit (SyncGroup + apply gate) below has finished, so a draining
	// RunUnderCommitLock never closes the WAL mid-fsync.
	defer t.store.doneInflight()

	if !hasSeq {
		// No sequence was minted (empty commit, or the cap-check rejection
		// which writes nothing). It never enters the apply gate. An empty
		// commit still flushes any prior buffered tail; the cap rejection
		// returns its error without I/O.
		if appendErr != nil {
			metrics.IncCounter("store.txn.Commit.errors", 1)
			return appendErr
		}
		if syncErr := t.store.wal.SyncGroup(); syncErr != nil {
			metrics.IncCounter("store.txn.Commit.errors", 1)
			return syncErr
		}
		return nil
	}

	// A sequence was minted (hasSeq). It MUST advance the apply gate exactly
	// once, in every outcome below (append error, fsync failure, apply error,
	// or success), or a gap would wedge every higher-sequence committer.

	// Group-commit phase 2 — DURABILITY with the semaphore free: a single
	// coalesced fsync covers this transaction's marker and every other
	// concurrently-buffered committer's frames. Returns only after the fsync
	// covering this marker has completed (durable-before-visible), or fails the
	// whole group on a sync error (poison fails all). If the append itself
	// failed we still run SyncGroup so a poisoned writer surfaces the sticky
	// error to this committer too; either way this transaction will not apply.
	syncErr := t.store.wal.SyncGroup()

	// Group-commit phase 3 — APPLY in sequence order. Wait until every
	// lower-sequence transaction has applied (or been skipped), so the
	// in-memory view is mutated in WAL order and no Graph.View reader observes
	// an out-of-order or pre-durable state.
	t.waitApplyTurn(seq)
	defer t.advanceApply(seq)

	if appendErr != nil {
		// The append did not complete (encode/append failure). No durable,
		// fully-marked transaction exists; do not apply. Surface the append
		// error (the primary cause); the writer is typically poisoned, so
		// syncErr would echo it.
		metrics.IncCounter("store.txn.Commit.errors", 1)
		return appendErr
	}
	if syncErr != nil {
		// The shared fsync failed: this transaction is NOT durable (its frames
		// were discarded by the writer's poison/truncate). Do not apply.
		metrics.IncCounter("store.txn.Commit.errors", 1)
		return syncErr
	}

	// Apply to the in-memory graph after durability is secured, under the
	// graph's visibility barrier (ApplyAtomically) so the whole transaction's
	// writes flip visible to Graph.View readers as one atomic step — no
	// reader can observe a partially-applied transaction (audit gap F3,
	// docs/isolation-design.md). The transaction is already durable (op
	// frames + OpCommit marker fsynced), so an apply error here does not undo
	// the commit: recovery — which builds the graph without a shard-capacity
	// cap — replays the whole transaction atomically. Surface it as
	// ErrCommittedNotApplied so the caller knows the commit is durable and
	// must not be retried (F5).
	if err := t.store.g.ApplyAtomically(func() error {
		for _, op := range t.ops {
			if aerr := applyOp(t.store.g, op); aerr != nil {
				return aerr
			}
		}
		return nil
	}); err != nil {
		metrics.IncCounter("store.txn.Commit.applyErrors", 1)
		return fmt.Errorf("%w: %w", ErrCommittedNotApplied, err)
	}
	return nil
}

// CommitWALOnly durably appends every buffered op to the WAL but does NOT
// apply the ops to the in-memory graph. Use this when the caller has
// already applied mutations eagerly (e.g. [walMutatorAdapter]) and only
// needs WAL durability without a second in-memory pass. It uses the same
// atomic v3 framing as [Tx.Commit] for typed stores.
//
// It performs no in-memory apply of its own, but it STILL advances the
// sequence-ordered apply gate for the sequence it mints. The gate tracks the
// dense per-store transaction sequence shared by [Tx.Commit] and CommitWALOnly;
// if CommitWALOnly minted a sequence without advancing the gate, a later
// [Tx.Commit] on the same store would wait on appliedSeq forever. Taking the
// turn and immediately advancing (applying nothing) keeps the chain dense
// whether or not the two commit paths are mixed on one store. The caller (the
// Cypher engine's commitUnderBarrier, #1281) has already applied the mutations
// eagerly inside the visibility barrier, and CommitWALOnly returning only after
// the covering fsync preserves durable-before-visible.
func (t *Tx[N, W]) CommitWALOnly() error {
	defer metrics.Time("store.txn.CommitWALOnly")()
	if t.finished {
		metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
		return ErrTxFinished
	}

	seq, hasSeq, appendErr := t.appendOnly()
	defer t.store.doneInflight()
	if !hasSeq {
		if appendErr != nil {
			metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
			return appendErr
		}
		if err := t.store.wal.SyncGroup(); err != nil {
			metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
			return err
		}
		return nil
	}
	syncErr := t.store.wal.SyncGroup()
	// A sequence was minted: take its apply-gate turn and advance it, applying
	// nothing, so the dense chain stays intact for any Commit on this store.
	t.waitApplyTurn(seq)
	t.advanceApply(seq)
	if appendErr != nil {
		metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
		return appendErr
	}
	if syncErr != nil {
		metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
		return syncErr
	}
	return nil
}

// waitApplyTurn blocks until the in-memory apply of every transaction with a
// lower sequence than seq has completed (appliedSeq == seq-1), so this
// transaction applies in WAL/sequence order. Sequences are dense and assigned
// under the single-writer semaphore, so the predecessor is always exactly
// seq-1. See the apply-gate fields on [Store].
func (t *Tx[N, W]) waitApplyTurn(seq uint64) {
	s := t.store
	s.applyMu.Lock()
	for s.appliedSeq != seq-1 {
		s.applyCond.Wait()
	}
	s.applyMu.Unlock()
}

// advanceApply marks seq's apply step complete and wakes the committer waiting
// on seq+1. It must be called exactly once for every sequence whose turn was
// taken via [Tx.waitApplyTurn], in every outcome (success, apply error, or
// fsync failure) — otherwise a failed transaction would wedge the apply chain
// behind it.
func (t *Tx[N, W]) advanceApply(seq uint64) {
	s := t.store
	s.applyMu.Lock()
	s.appliedSeq = seq
	s.applyCond.Broadcast()
	s.applyMu.Unlock()
}

// appendOnly performs group-commit phase 1: it encodes and appends every
// buffered op to the WAL (without fsyncing) and then RELEASES the single-writer
// semaphore so the next transaction can append while this one fsyncs. It is the
// append half of the old appendAndSync; the fsync is now a separate, coalesced
// step ([wal.Writer.SyncGroup]) the caller runs with the semaphore free.
//
// Every op is encoded as a v3 frame carrying a fresh per-transaction sequence
// ([Store.txnSeq]) assigned under the semaphore, and an [OpCommit] marker frame
// for the same sequence is appended after the last op. The on-disk frame order
// is therefore unchanged from the per-commit path — each transaction's ops are
// contiguous in sequence order, followed by its marker — so recovery's
// all-or-nothing replay is unaffected.
//
// The return values:
//   - seq is the assigned transaction sequence (valid only when hasSeq is true);
//   - hasSeq is true once a sequence has been MINTED (txnSeq.Add) — true for any
//     non-empty transaction, even one whose subsequent encode/append failed. A
//     minted sequence MUST take its turn in the apply gate and advance it, or a
//     gap in the dense sequence chain would wedge every higher-sequence
//     committer; the caller therefore enters the gate whenever hasSeq is true
//     and decides whether to apply based on err and the SyncGroup result.
//     hasSeq is false only for an empty commit and for the cap-check rejection,
//     both of which mint no sequence.
//   - err is non-nil when the cap check, encoding, or append failed.
//
// The semaphore is released exactly once, on every path, via
// releaseAfterAppend; the Tx is marked finished at the same time.
//
// markInflight is called here, while the semaphore is still held, so the commit
// is registered as in-flight BEFORE the semaphore is released — the happens-
// before that lets [Store.RunUnderCommitLock] observe it (#1507 quiesce
// boundary). The caller MUST pair it with exactly one [Store.doneInflight] once
// the whole commit (SyncGroup + apply gate) finishes.
func (t *Tx[N, W]) appendOnly() (seq uint64, hasSeq bool, err error) {
	t.store.markInflight()
	if len(t.ops) == 0 {
		// Empty commit: mint no sequence and write no marker. The caller still
		// runs SyncGroup to flush any prior buffered tail (the historical
		// no-op-with-Sync behaviour), then applies nothing.
		t.releaseAfterAppend()
		return 0, false, nil
	}
	// Bounded resources / Durability: reject an over-cap transaction BEFORE
	// minting a sequence or writing any frame, so a transaction recovery could
	// not replay without unbounded buffering is never made durable and never
	// consumes a sequence slot. The producer cap is <= the recovery cap, so
	// every transaction that passes here is guaranteed to fit recovery's buffer
	// (see [ErrTransactionTooLarge], [DefaultMaxTxnOps]).
	if t.store.maxTxnOps > 0 && len(t.ops) > t.store.maxTxnOps {
		metrics.IncCounter("store.txn.appendOnly.txnTooLarge", 1)
		t.releaseAfterAppend()
		return 0, false, fmt.Errorf("%w: %d ops > cap %d", ErrTransactionTooLarge, len(t.ops), t.store.maxTxnOps)
	}
	// Mint the sequence. From here hasSeq is true on every return: the sequence
	// is consumed, so the caller must advance the apply gate past it even if the
	// append below fails (a gap would deadlock the dense sequence chain). A
	// partial append is harmless on disk — recovery discards any frames not
	// followed by a durable matching OpCommit marker — and the err makes the
	// caller skip the in-memory apply.
	seq = t.store.txnSeq.Add(1)
	for _, op := range t.ops {
		payload, enErr := encodeOpTypedV3(op, seq, t.store.codec, t.store.wcodec)
		if enErr != nil {
			t.releaseAfterAppend()
			return seq, true, enErr
		}
		if aerr := t.store.wal.Append(payload); aerr != nil {
			t.releaseAfterAppend()
			return seq, true, aerr
		}
	}
	if aerr := t.store.wal.Append(encodeCommitV3(seq)); aerr != nil {
		t.releaseAfterAppend()
		return seq, true, aerr
	}
	// Frames + marker are buffered. Release the semaphore so the next
	// transaction can append while this one fsyncs (group-commit coalescing).
	t.releaseAfterAppend()
	return seq, true, nil
}

// Rollback discards buffered ops without touching the WAL or graph.
func (t *Tx[N, W]) Rollback() error {
	defer metrics.Time("store.txn.Rollback")()
	if t.finished {
		metrics.IncCounter("store.txn.Rollback.errors", 1)
		return ErrTxFinished
	}
	t.releaseAfterAppend()
	return nil
}

// releaseAfterAppend marks the transaction finished and frees the store's
// single-writer semaphore, exactly once. It is called from [Tx.appendOnly] (on
// every path), from [Tx.Rollback], and the entry-point finished guard in
// Commit/CommitWALOnly/Rollback ensures it is reached once per transaction, so
// the capacity-one semaphore is never over-released (which would let a second
// writer in). The idempotency check makes a double call (defensive) a no-op.
//
// NOTE: under group commit the semaphore is released here — after the frames
// are appended but BEFORE the coalesced fsync — so the fsync window no longer
// holds the writer lock. The fsync's durability and the sequence-ordered
// in-memory apply are coordinated separately (SyncGroup and the apply gate);
// they do not depend on the semaphore being held.
func (t *Tx[N, W]) releaseAfterAppend() {
	if t.finished {
		return
	}
	t.finished = true
	t.store.release()
}

// encodeOpTyped serialises one op to a v2 (tagged) WAL payload using
// the supplied codecs.
//
// Layout for [OpAddEdge], [OpSetNodeLabel], [OpSetEdgeLabel]:
//
//	uint8  version  (always [OpRecordV2])
//	uint8  kind
//	codec  src
//	codec  dst
//	uint16 labelLen
//	[labelLen]byte label
//
// Layout for [OpAddEdgeWeighted]:
//
//	uint8  version  ([OpRecordV2])
//	uint8  kind     ([OpAddEdgeWeighted])
//	codec  src
//	codec  dst
//	wcodec w
//	uint16 labelLen (always 0 for AddEdge)
//
// Layout for single-endpoint node ops ([OpAddNode], [OpRemoveNode],
// [OpRemoveNodeLabel]):
//
//	uint8  version  ([OpRecordV2])
//	uint8  kind
//	codec  src        (the node key)
//	codec  dst-zero   (zero value; included so the recovery decoder
//	                   can walk both endpoint slots uniformly)
//	uint16 labelLen
//	[labelLen]byte label   (empty for OpAddNode/OpRemoveNode; the label
//	                        for OpRemoveNodeLabel)
//
// Layout for property ops ([OpSetNodeProperty], [OpDelNodeProperty],
// [OpSetEdgeProperty], [OpDelEdgeProperty]):
//
//	uint8  version  ([OpRecordV2])
//	uint8  kind
//	codec  src
//	codec  dst        (zero for node ops; dst key for edge ops)
//	uint16 keyLen
//	[keyLen]byte key
//	[propValue]       (only for Set ops: uint8 kind tag + value bytes)
//
// Layout for [OpRemoveEdge]:
//
//	uint8  version  ([OpRecordV2])
//	uint8  kind
//	codec  src
//	codec  dst
//	uint16 = 0      (empty label)
func encodeOpTyped[N comparable, W any](op Op[N, W], codec Codec[N], wcodec WeightCodec[W]) ([]byte, error) {
	const headroom = 2 + 2 // version + kind + uint16 labelLen
	buf := make([]byte, 0, headroom+len(op.Label)+32)
	buf = append(buf, OpRecordV2, byte(op.Kind))
	return appendOpBodyTyped(buf, op, codec, wcodec)
}

// encodeOpTypedV3 serialises one op to a v3 ([OpRecordV3]) WAL payload:
// the v2 header (version + kind) plus an 8-byte little-endian transaction
// sequence, followed by the byte-identical v2 body for that kind. The
// sequence groups a transaction's frames so recovery can apply them
// atomically (see [OpRecordV3] and [OpCommit]).
func encodeOpTypedV3[N comparable, W any](op Op[N, W], seq uint64, codec Codec[N], wcodec WeightCodec[W]) ([]byte, error) {
	const headroom = 2 + 8 + 2 // version + kind + txnSeq + uint16 labelLen
	buf := make([]byte, 0, headroom+len(op.Label)+32)
	buf = append(buf, OpRecordV3, byte(op.Kind))
	buf = binary.LittleEndian.AppendUint64(buf, seq)
	return appendOpBodyTyped(buf, op, codec, wcodec)
}

// encodeCommitV3 serialises the [OpCommit] marker for a v3 transaction:
// version + kind + the transaction sequence, with no body. Recovery
// applies the buffered ops carrying the same sequence when it reads this
// frame; a torn write that loses it discards the whole transaction.
func encodeCommitV3(seq uint64) []byte {
	buf := make([]byte, 0, 2+8)
	buf = append(buf, OpRecordV3, byte(OpCommit))
	buf = binary.LittleEndian.AppendUint64(buf, seq)
	return buf
}

// appendOpBodyTyped appends the codec-encoded body for op to buf (which
// already holds the version + kind, and for v3 the txnSeq). The body
// layout is shared verbatim by the v2 and v3 encoders.
func appendOpBodyTyped[N comparable, W any](buf []byte, op Op[N, W], codec Codec[N], wcodec WeightCodec[W]) ([]byte, error) {
	switch op.Kind {
	case OpAddNode, OpRemoveNode, OpRemoveNodeLabel:
		return encodeOpNodeOnly(buf, op, codec)
	case OpSetNodeProperty, OpDelNodeProperty:
		return encodeOpNodeProperty(buf, op, codec)
	case OpRemoveEdge:
		return encodeOpEdgeNoTail(buf, op, codec)
	case OpSetEdgeProperty, OpDelEdgeProperty:
		return encodeOpEdgeProperty(buf, op, codec)
	case OpAddEdgeWeighted:
		return encodeOpEdgeWeighted(buf, op, codec, wcodec)
	case OpAddEdgeH:
		// Weighted-edge body followed by the 8-byte stable handle.
		var err error
		if buf, err = encodeOpEdgeWeighted(buf, op, codec, wcodec); err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint64(buf, op.Handle), nil
	case OpSetEdgeLabelByHandle:
		// Edge-with-label body followed by the 8-byte stable handle.
		var err error
		if buf, err = encodeOpEdgeWithLabel(buf, op, codec); err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint64(buf, op.Handle), nil
	case OpSetEdgePropertyByHandle:
		// Edge-property body followed by the 8-byte stable handle.
		var err error
		if buf, err = encodeOpEdgeProperty(buf, op, codec); err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint64(buf, op.Handle), nil
	case OpRemoveEdgeInstanceByHandle:
		// Edge-no-tail body followed by the 8-byte stable handle.
		var err error
		if buf, err = encodeOpEdgeNoTail(buf, op, codec); err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint64(buf, op.Handle), nil
	case OpCreateConstraint, OpDropConstraint:
		// Schema record: a one-byte constraint-kind tag followed by three
		// length-prefixed strings (label, property, name). No node codec is
		// involved — a constraint carries no endpoints.
		return appendOpConstraintBody(buf, op), nil
	case OpCreateIndex, OpDropIndex:
		// Index schema record: a one-byte index-kind tag followed by three
		// length-prefixed strings (name, label, property). No node codec is
		// involved — an index definition carries no endpoints.
		return appendOpIndexBody(buf, op), nil
	default: // OpAddEdge, OpSetNodeLabel, OpSetEdgeLabel
		return encodeOpEdgeWithLabel(buf, op, codec)
	}
}

// appendOpConstraintBody appends the body of an [OpCreateConstraint] /
// [OpDropConstraint] frame to buf:
//
//	uint8  constraintKind ([ConstraintKind])
//	uint16 labelLen        || [labelLen]byte label
//	uint16 propLen         || [propLen]byte property
//	uint16 nameLen         || [nameLen]byte name
//
// The uint16 length prefixes match the label-encoding convention of the
// sibling body encoders; a constraint label, property, or name longer than a
// uint16 is rejected upstream (the whole query is capped at 1 MiB by the DML
// guard, and a real identifier is a handful of bytes). The body is independent
// of the node [Codec] because a constraint has no endpoints.
func appendOpConstraintBody[N comparable, W any](buf []byte, op Op[N, W]) []byte {
	buf = append(buf, byte(op.ConstraintKind))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
	buf = append(buf, op.Label...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Key)))
	buf = append(buf, op.Key...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.ConstraintName)))
	buf = append(buf, op.ConstraintName...)
	return buf
}

// appendOpIndexBody appends the body of an [OpCreateIndex] / [OpDropIndex]
// frame to buf:
//
//	uint8  indexKind  ([IndexKind])
//	uint16 nameLen    || [nameLen]byte  name
//	uint16 labelLen   || [labelLen]byte label
//	uint16 propLen    || [propLen]byte  property
//
// The body is independent of the node [Codec] because an index definition
// carries no endpoints.
func appendOpIndexBody[N comparable, W any](buf []byte, op Op[N, W]) []byte {
	buf = append(buf, byte(op.IndexKind))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.ConstraintName)))
	buf = append(buf, op.ConstraintName...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
	buf = append(buf, op.Label...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Key)))
	buf = append(buf, op.Key...)
	return buf
}

// encodeOpNodeOnly writes the [Src, zero, label] tail for OpKinds that
// operate on a single node (OpAddNode, OpRemoveNode, OpRemoveNodeLabel).
func encodeOpNodeOnly[N comparable, W any](buf []byte, op Op[N, W], codec Codec[N]) ([]byte, error) {
	var zero N
	var err error
	if buf, err = codec.Encode(buf, op.Src); err != nil {
		return nil, err
	}
	if buf, err = codec.Encode(buf, zero); err != nil {
		return nil, err
	}
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
	buf = append(buf, op.Label...)
	return buf, nil
}

// encodeOpNodeProperty writes the [Src, zero, keyLen, key, (value)] tail
// for OpSetNodeProperty / OpDelNodeProperty.
func encodeOpNodeProperty[N comparable, W any](buf []byte, op Op[N, W], codec Codec[N]) ([]byte, error) {
	var zero N
	var err error
	if buf, err = codec.Encode(buf, op.Src); err != nil {
		return nil, err
	}
	if buf, err = codec.Encode(buf, zero); err != nil {
		return nil, err
	}
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Key)))
	buf = append(buf, op.Key...)
	if op.Kind == OpSetNodeProperty {
		buf = encodePropertyValue(buf, op.Value)
	}
	return buf, nil
}

// encodeOpEdgeNoTail writes [Src, Dst, 0] for OpRemoveEdge (the empty
// label tail keeps the OpRecord layout uniform).
func encodeOpEdgeNoTail[N comparable, W any](buf []byte, op Op[N, W], codec Codec[N]) ([]byte, error) {
	var err error
	if buf, err = codec.Encode(buf, op.Src); err != nil {
		return nil, err
	}
	if buf, err = codec.Encode(buf, op.Dst); err != nil {
		return nil, err
	}
	buf = binary.LittleEndian.AppendUint16(buf, 0)
	return buf, nil
}

// encodeOpEdgeProperty writes [Src, Dst, keyLen, key, (value)] for
// OpSetEdgeProperty / OpDelEdgeProperty.
func encodeOpEdgeProperty[N comparable, W any](buf []byte, op Op[N, W], codec Codec[N]) ([]byte, error) {
	var err error
	if buf, err = codec.Encode(buf, op.Src); err != nil {
		return nil, err
	}
	if buf, err = codec.Encode(buf, op.Dst); err != nil {
		return nil, err
	}
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Key)))
	buf = append(buf, op.Key...)
	if op.Kind == OpSetEdgeProperty || op.Kind == OpSetEdgePropertyByHandle {
		buf = encodePropertyValue(buf, op.Value)
	}
	return buf, nil
}

// encodeOpEdgeWeighted writes [Src, Dst, weight, labelLen, label] for
// OpAddEdgeWeighted. The weight bytes are omitted when wcodec is nil.
func encodeOpEdgeWeighted[N comparable, W any](buf []byte, op Op[N, W], codec Codec[N], wcodec WeightCodec[W]) ([]byte, error) {
	var err error
	if buf, err = codec.Encode(buf, op.Src); err != nil {
		return nil, err
	}
	if buf, err = codec.Encode(buf, op.Dst); err != nil {
		return nil, err
	}
	if wcodec != nil {
		if buf, err = wcodec.Encode(buf, op.Weight); err != nil {
			return nil, err
		}
	}
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
	buf = append(buf, op.Label...)
	return buf, nil
}

// encodeOpEdgeWithLabel writes [Src, Dst, labelLen, label] for the
// default group (OpAddEdge, OpSetNodeLabel, OpSetEdgeLabel).
func encodeOpEdgeWithLabel[N comparable, W any](buf []byte, op Op[N, W], codec Codec[N]) ([]byte, error) {
	var err error
	if buf, err = codec.Encode(buf, op.Src); err != nil {
		return nil, err
	}
	if buf, err = codec.Encode(buf, op.Dst); err != nil {
		return nil, err
	}
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
	buf = append(buf, op.Label...)
	return buf, nil
}

// encodePropertyValue appends the wire encoding of a [lpg.PropertyValue] to buf.
//
// Format:
//
//	uint8  kind tag  ([lpg.PropertyKind])
//	...value bytes...
//
// Kind tags map 1:1 to [lpg.PropString], [lpg.PropInt64], [lpg.PropFloat64],
// [lpg.PropBool], [lpg.PropTime], [lpg.PropBytes], [lpg.PropList]. For
// [lpg.PropString] and [lpg.PropBytes] the value is prefixed with a uint32 LE
// length. For [lpg.PropInt64] the value is a signed varint. For [lpg.PropFloat64]
// the value is a uint64 LE IEEE-754 bit pattern. For [lpg.PropBool] the value is
// a single byte (0 or 1). For [lpg.PropTime] the value is the UTC Unix nanoseconds
// as a signed varint. For [lpg.PropList] the value is a uint32 LE element-count
// followed by element-count sub-records encoded as:
//
//	uint8  elem-kind
//	uint32 elem-payload-len
//	[elem-payload-len]byte elem-payload
//
// Nested PropList elements are not permitted.
func encodePropertyValue(buf []byte, v lpg.PropertyValue) []byte {
	buf = append(buf, byte(v.Kind()))
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s)))
		buf = append(buf, s...)
	case lpg.PropInt64:
		i, _ := v.Int64()
		buf = binary.AppendVarint(buf, i)
	case lpg.PropFloat64:
		f, _ := v.Float64()
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(f))
	case lpg.PropBool:
		b, _ := v.Bool()
		if b {
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
	case lpg.PropTime:
		t, _ := v.Time()
		buf = binary.AppendVarint(buf, t.UnixNano())
	case lpg.PropBytes:
		bs, _ := v.Bytes()
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(bs)))
		buf = append(buf, bs...)
	case lpg.PropList:
		buf = encodeTxnListProp(buf, v)
	}
	return buf
}

// encodeTxnListProp appends the list wire encoding to buf (without the leading
// kind byte, which the caller already wrote). Format:
//
//	uint32 LE element-count
//	element-count × ( uint8 elem-kind | uint32 elem-payload-len | [elem-payload-len]byte elem-payload )
func encodeTxnListProp(buf []byte, v lpg.PropertyValue) []byte {
	elems, _ := v.List()
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(elems)))
	for _, elem := range elems {
		// Encode the element into a temporary buffer to measure its length.
		// The element kind byte is not included — we write it separately.
		var payload []byte
		switch elem.Kind() {
		case lpg.PropString:
			s, _ := elem.String()
			payload = append(payload, s...)
		case lpg.PropInt64:
			i, _ := elem.Int64()
			payload = binary.AppendVarint(payload, i)
		case lpg.PropFloat64:
			f, _ := elem.Float64()
			payload = binary.LittleEndian.AppendUint64(payload, math.Float64bits(f))
		case lpg.PropBool:
			b, _ := elem.Bool()
			if b {
				payload = append(payload, 1)
			} else {
				payload = append(payload, 0)
			}
		case lpg.PropTime:
			t, _ := elem.Time()
			payload = binary.AppendVarint(payload, t.UnixNano())
		case lpg.PropBytes:
			bs, _ := elem.Bytes()
			payload = append(payload, bs...)
		}
		buf = append(buf, byte(elem.Kind()))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(payload)))
		buf = append(buf, payload...)
	}
	return buf
}

// decodePropertyValue parses a [lpg.PropertyValue] from the head of buf.
// Returns the decoded value, the remaining bytes, and any error.
func decodePropertyValue(buf []byte) (lpg.PropertyValue, []byte, error) {
	if len(buf) < 1 {
		return lpg.PropertyValue{}, buf, errors.New("txn: short property value (missing kind)")
	}
	kind := lpg.PropertyKind(buf[0])
	buf = buf[1:]
	switch kind {
	case lpg.PropString:
		return decodeTxnStringProp(buf)
	case lpg.PropInt64:
		return decodeTxnInt64Prop(buf)
	case lpg.PropFloat64:
		return decodeTxnFloat64Prop(buf)
	case lpg.PropBool:
		return decodeTxnBoolProp(buf)
	case lpg.PropTime:
		return decodeTxnTimeProp(buf)
	case lpg.PropBytes:
		return decodeTxnBytesProp(buf)
	case lpg.PropList:
		return decodeTxnListProp(buf)
	default:
		return lpg.PropertyValue{}, buf, errors.New("txn: unknown property kind")
	}
}

// txnListElemMinBytes is the smallest number of bytes one PropList element
// can occupy on the wire: a 1-byte kind plus a 4-byte payload-length prefix
// (the payload itself may be zero bytes). It is the divisor used to bound a
// list capacity hint against the remaining input.
const txnListElemMinBytes = 5

// txnListCapHint returns a safe capacity hint for a PropList decode buffer.
// count is the untrusted element count from the wire; remaining is the number
// of bytes left to parse. Because each element consumes at least
// txnListElemMinBytes bytes, no more than remaining/txnListElemMinBytes
// elements can follow, so the hint is min(count, remaining/txnListElemMinBytes).
// This prevents a hostile count (up to ~4.3e9) from triggering a multi-gigabyte
// eager reservation while still pre-sizing accurately for legitimate lists.
// Mirrors recovery.recoveryListCapHint and snapshot.listCapHint.
func txnListCapHint(count uint32, remaining int) int {
	maxElems := remaining / txnListElemMinBytes
	if int64(count) < int64(maxElems) {
		return int(count)
	}
	return maxElems
}

// decodeTxnListProp parses a PropList value from buf.
// Format (following the kind byte already consumed by the caller):
//
//	uint32 LE element-count
//	element-count × ( uint8 elem-kind | uint32 elem-payload-len | [elem-payload-len]byte elem-payload )
func decodeTxnListProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	if len(buf) < 4 {
		return lpg.PropertyValue{}, buf, errors.New("txn: PropList: short element count")
	}
	count := binary.LittleEndian.Uint32(buf)
	buf = buf[4:]
	// count is an untrusted uint32 (up to ~4.3e9). Each element needs at
	// least txnListElemMinBytes on the wire, so at most len(buf)/txnListElemMinBytes
	// elements can actually follow; clamp the capacity hint to that ceiling so a
	// hostile count cannot drive a multi-GB eager reservation
	// (lpg.PropertyValue is 24 B, so an unclamped count would reserve ~103 GiB).
	// The loop below still validates and bounds every element, so a
	// smaller-than-count capacity only costs a few re-grows for a genuinely large
	// legitimate list. Mirrors recovery.recoveryListCapHint and
	// snapshot.listCapHint so all three PropList decoders share one bound.
	elems := make([]lpg.PropertyValue, 0, txnListCapHint(count, len(buf)))
	for i := uint32(0); i < count; i++ {
		if len(buf) < 5 { // kind(1) + payloadLen(4)
			return lpg.PropertyValue{}, buf,
				fmt.Errorf("txn: PropList: truncated element header at index %d", i)
		}
		elemKind := lpg.PropertyKind(buf[0])
		payloadLen := binary.LittleEndian.Uint32(buf[1:5])
		buf = buf[5:]
		if uint64(len(buf)) < uint64(payloadLen) {
			return lpg.PropertyValue{}, buf,
				fmt.Errorf("txn: PropList: truncated element body at index %d", i)
		}
		payload := buf[:payloadLen]
		buf = buf[payloadLen:]
		elem, err := decodeTxnListElement(elemKind, payload)
		if err != nil {
			return lpg.PropertyValue{}, buf,
				fmt.Errorf("txn: PropList: element %d: %w", i, err)
		}
		elems = append(elems, elem)
	}
	return lpg.ListValue(elems), buf, nil
}

// decodeTxnListElement decodes a single list element from a raw payload.
// The element payload does not include its kind byte (already consumed by
// [decodeTxnListProp]).
func decodeTxnListElement(kind lpg.PropertyKind, payload []byte) (lpg.PropertyValue, error) {
	switch kind {
	case lpg.PropString:
		return lpg.StringValue(string(payload)), nil
	case lpg.PropInt64:
		i, n := binary.Varint(payload)
		if n <= 0 {
			return lpg.PropertyValue{}, errors.New("txn: PropList element: varint decode failed")
		}
		return lpg.Int64Value(i), nil
	case lpg.PropFloat64:
		if len(payload) < 8 {
			return lpg.PropertyValue{}, errors.New("txn: PropList element: short float64")
		}
		return lpg.Float64Value(math.Float64frombits(binary.LittleEndian.Uint64(payload))), nil
	case lpg.PropBool:
		if len(payload) < 1 {
			return lpg.PropertyValue{}, errors.New("txn: PropList element: short bool")
		}
		return lpg.BoolValue(payload[0] != 0), nil
	case lpg.PropTime:
		ns, n := binary.Varint(payload)
		if n <= 0 {
			return lpg.PropertyValue{}, errors.New("txn: PropList element: time varint decode failed")
		}
		return lpg.TimeValue(time.Unix(0, ns).UTC()), nil
	case lpg.PropBytes:
		cp := make([]byte, len(payload))
		copy(cp, payload)
		return lpg.BytesValue(cp), nil
	default:
		return lpg.PropertyValue{}, fmt.Errorf("txn: PropList element: unknown kind %d", kind)
	}
}

// decodeTxnLengthPrefixed reads a uint32 length followed by length
// bytes; returns the body and the remainder. Shared by the String
// and Bytes decoders. errTag is mixed into the diagnostic
// ("string" or "bytes") so the typed error carries its breadcrumb.
func decodeTxnLengthPrefixed(buf []byte, errTag string) (body, rest []byte, err error) {
	if len(buf) < 4 {
		return nil, buf, fmt.Errorf("txn: short %s property (missing length)", errTag)
	}
	n := binary.LittleEndian.Uint32(buf)
	buf = buf[4:]
	if uint64(len(buf)) < uint64(n) {
		return nil, buf, fmt.Errorf("txn: short %s property body", errTag)
	}
	return buf[:n], buf[n:], nil
}

func decodeTxnStringProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	body, rest, err := decodeTxnLengthPrefixed(buf, "string")
	if err != nil {
		return lpg.PropertyValue{}, rest, err
	}
	return lpg.StringValue(string(body)), rest, nil
}

func decodeTxnBytesProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	body, rest, err := decodeTxnLengthPrefixed(buf, "bytes")
	if err != nil {
		return lpg.PropertyValue{}, rest, err
	}
	bs := make([]byte, len(body))
	copy(bs, body)
	return lpg.BytesValue(bs), rest, nil
}

func decodeTxnInt64Prop(buf []byte) (lpg.PropertyValue, []byte, error) {
	x, n := binary.Varint(buf)
	if n <= 0 {
		return lpg.PropertyValue{}, buf, errors.New("txn: short int64 property")
	}
	return lpg.Int64Value(x), buf[n:], nil
}

func decodeTxnFloat64Prop(buf []byte) (lpg.PropertyValue, []byte, error) {
	if len(buf) < 8 {
		return lpg.PropertyValue{}, buf, errors.New("txn: short float64 property")
	}
	bits := binary.LittleEndian.Uint64(buf[:8])
	return lpg.Float64Value(math.Float64frombits(bits)), buf[8:], nil
}

func decodeTxnBoolProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	if len(buf) < 1 {
		return lpg.PropertyValue{}, buf, errors.New("txn: short bool property")
	}
	return lpg.BoolValue(buf[0] != 0), buf[1:], nil
}

func decodeTxnTimeProp(buf []byte) (lpg.PropertyValue, []byte, error) {
	nanos, n := binary.Varint(buf)
	if n <= 0 {
		return lpg.PropertyValue{}, buf, errors.New("txn: short time property")
	}
	return lpg.TimeValue(time.Unix(0, nanos).UTC()), buf[n:], nil
}

// applyOp dispatches one buffered Op against the in-memory LPG.
// Returns any error surfaced by the graph (currently only
// [adjlist.ErrShardFull] is reachable, and only when the underlying
// [adjlist.Config.MaxShardCapacity] is set). The WAL has already been
// fsynced for op by the time applyOp runs, so an error here means the
// durable log and the in-memory view are temporarily inconsistent —
// recovery will replay the same op and surface the same error.
func applyOp[N comparable, W any](g *lpg.Graph[N, W], op Op[N, W]) error {
	switch op.Kind {
	case OpAddEdge:
		var zero W
		return g.AddEdge(op.Src, op.Dst, zero)
	case OpAddEdgeWeighted:
		return g.AddEdge(op.Src, op.Dst, op.Weight)
	case OpAddEdgeH:
		// Handle-bearing add: idempotent against a slot already carrying
		// this handle (the snapshot loaded it, or an earlier frame applied
		// it), so snapshot + full-WAL recovery does not double the edge.
		if _, err := g.AddEdgeHIfAbsent(op.Src, op.Dst, op.Weight, op.Handle); err != nil {
			return err
		}
	case OpSetEdgeLabelByHandle:
		g.SetEdgeLabelByHandle(op.Src, op.Dst, op.Handle, op.Label)
	case OpSetEdgePropertyByHandle:
		return g.SetEdgePropertyByHandle(op.Src, op.Dst, op.Handle, op.Key, op.Value)
	case OpRemoveEdgeInstanceByHandle:
		g.RemoveEdgeInstanceByHandle(op.Src, op.Dst, op.Handle)
	case OpSetNodeLabel:
		return g.SetNodeLabel(op.Src, op.Label)
	case OpSetEdgeLabel:
		g.SetEdgeLabel(op.Src, op.Dst, op.Label)
	case OpAddNode:
		return g.AddNode(op.Src)
	case OpRemoveNode:
		// Logical removal: the mapper entry is permanent, so removal is a
		// tombstone. Strip all labels and properties so the node is
		// unreachable via label/property queries, then tombstone it so it
		// is excluded from live scans and counts. Tombstoning here keeps
		// the in-memory state applied by a committed Tx identical to the
		// state reconstructed by WAL replay (recovery.applyOpCodec does the
		// same), so live and recovered graphs agree. A later OpAddNode for
		// the same key revives it (g.AddNode clears the tombstone).
		for _, lbl := range g.NodeLabels(op.Src) {
			g.RemoveNodeLabel(op.Src, lbl)
		}
		for k := range g.NodeProperties(op.Src) {
			g.DelNodeProperty(op.Src, k)
		}
		g.RemoveNode(op.Src)
	case OpRemoveNodeLabel:
		g.RemoveNodeLabel(op.Src, op.Label)
	case OpSetNodeProperty:
		return g.SetNodeProperty(op.Src, op.Key, op.Value)
	case OpDelNodeProperty:
		g.DelNodeProperty(op.Src, op.Key)
	case OpRemoveEdge:
		// Use the LPG edge removal so a fully-disconnected pair also sheds
		// its per-pair edge labels/properties (matching recovery replay),
		// preventing a later re-add from resurrecting them.
		g.RemoveEdge(op.Src, op.Dst)
	case OpSetEdgeProperty:
		return g.SetEdgeProperty(op.Src, op.Dst, op.Key, op.Value)
	case OpDelEdgeProperty:
		g.DelEdgeProperty(op.Src, op.Dst, op.Key)
	}
	return nil
}

// isZero reports whether w equals the zero value of W. W is not
// constrained to be comparable (the type parameter is `any`), so the
// canonical equality test goes through reflect. The check is on the
// transaction-buffer path (one call per Tx.AddEdge) and not in the
// inner Commit loop, so the reflect cost is bounded and easily
// dominated by the WAL fsync that follows.
func isZero[W any](w W) bool {
	var zero W
	return reflect.DeepEqual(w, zero)
}
