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
// # Concurrency
//
// Independent write transactions run CONCURRENTLY. The store is safe for
// concurrent use by any number of goroutines, and reads on the underlying
// graph remain lock-free in the lpg / adjlist contracts.
//
// This is a change of contract. Until rmp #2306 a capacity-one semaphore
// acquired in Begin and released in Commit or Rollback made the store
// single-writer. That semaphore is retired: [Store.Begin] and [Store.BeginCtx]
// now only REGISTER the transaction as an admitted writer, and the
// registration excludes no other writer — see [Store.enterWriter], which takes
// its mutex solely to increment an in-flight count and blocks only while a
// quiesce ([Store.RunUnderCommitLock]) is draining.
//
// A write-write collision is therefore DETECTED rather than prevented: MVCC
// applies first-updater-wins on the version chain and the loser receives an
// error wrapping [graph/mvcc.ErrSerializationConflict], which is retriable.
// Callers that require two writers to be serialised must arrange that
// themselves; the store does not.
//
// Retiring the semaphore was measured throughput-neutral — it was released
// after the WAL append and never covered the coalesced fsync that dominates a
// durable commit. See docs/benchmarks/store-semaphore-retirement-2026-08-04.md.
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

	// OpDelEdgePropertyByHandle buffers a DelEdgePropertyByHandle(src, dst,
	// handle, key) mutation, removing exactly one property key from one parallel
	// edge's per-instance property bag while leaving sibling handles untouched.
	// It is the single-key removal analogue of [OpSetEdgePropertyByHandle] and
	// the handle-keyed analogue of [OpDelEdgeProperty]: the body is the
	// edge-property body (NO value — the value-append guard in
	// [encodeOpEdgeProperty] omits it for any kind other than the two Set kinds)
	// followed by the 8-byte little-endian handle, byte-symmetric to how
	// [OpDelEdgeProperty] relates to [OpSetEdgeProperty]. It is appended after the
	// schema-DDL kinds so every pre-existing OpKind value keeps its stable wire
	// identity; a WAL written by an older binary never carries it, and a reader
	// that predates it surfaces it as an unknown kind. Emitted by the Cypher
	// write path (walMutatorAdapter) for REMOVE r.x / SET r.x = null on a bound
	// parallel relationship, so the per-instance removal survives recovery.
	OpDelEdgePropertyByHandle

	// OpRemoveEdgeByHandle buffers a RemoveEdgeByHandle(src, dst, handle)
	// mutation: it retires the single parallel edge instance identified by the
	// stable handle on the (src, dst) pair — its adjacency slot AND its
	// per-handle metadata — leaving sibling instances intact. It is the
	// instance-precise counterpart of [OpRemoveEdge] (which removes the FIRST
	// src→dst slot regardless of identity) and the adjacency-bearing companion
	// of [OpRemoveEdgeInstanceByHandle] (which drops only the per-handle
	// metadata): DELETE of a specifically-bound parallel relationship emits this
	// so the exact instance is gone after recovery, not the lowest-indexed one
	// (rmp #2018). The body is the edge-no-tail body ([Src, Dst, uint16=0])
	// followed by the 8-byte little-endian handle, byte-identical to
	// [OpRemoveEdgeInstanceByHandle] apart from the kind byte. It is appended
	// after every pre-existing OpKind so a WAL written by an older binary never
	// carries it, and a reader that predates it surfaces it as an unknown kind.
	OpRemoveEdgeByHandle
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
	// ResumeTxnSeq is the transaction sequence this store continues FROM: the
	// first transaction it commits is assigned ResumeTxnSeq+1.
	//
	// # Why it is needed (rmp #2302, audit finding E5)
	//
	// The sequence groups a transaction's frames so recovery can apply them
	// atomically, and recovery decoded it while nothing ever wrote it back. A
	// store reopened on a non-empty WAL therefore restarted at 0 and minted 1
	// again, so ONE WAL could hold two different transactions under one sequence
	// number. Recovery's TxnSeq-suffix filter tolerated that only because frame
	// contiguity plus equality happened to disambiguate it — an accident, not a
	// guarantee, and one that stops holding the moment a reopen follows a torn
	// tail.
	//
	// # Why it is DERIVED and not persisted
	//
	// Set it from [store/recovery.Result.MaxTxnSeq], which is the highest
	// sequence any replayed v3 frame carried. The WAL already records the
	// sequence in every frame, so a separate durable counter would be a second
	// source of truth that can disagree with the log — the same reasoning
	// rmp #2309 applies to the MVCC clock. Zero, the value every fresh store
	// carries, starts at 1 as before.
	ResumeTxnSeq uint64
}

// Store bundles an [lpg.Graph] with a [wal.Writer] and the single-
// writer lock that serialises transactions.
//
// Concurrency: any number of goroutines may call Begin/BeginCtx;
// transactions no longer serialise on a semaphore, so more than one Tx is
// active at any moment. Reads on the underlying lpg.Graph remain
// concurrent and lock-free per the lpg/adjlist contracts.
// [Store.RunUnderCommitLock] is the one thing that DOES exclude writers: it
// closes the admission gate and drains the admitted writers to zero, so a
// background checkpointer can quiesce the store while it snapshots and
// truncates the WAL.
//
// Admission is cancellable, which is the property a deadline-bearing caller
// needs: [Store.BeginCtx] waits on the current quiesce against ctx.Done() and
// returns the context error without blocking for the quiesce's full
// duration. A [sync.Mutex] cannot honour a deadline while it is contended;
// waiting on the quiesce channel can, which is what makes the engine write
// path ([cypher.Engine.RunInTx]) respect a caller's deadline.
//
// There is no longer any mutual exclusion between writers to preserve. A
// write-write collision is DETECTED by MVCC first-updater-wins on the version
// chain and reported as a retriable error wrapping
// [graph/mvcc.ErrSerializationConflict], rather than prevented by admission.
type Store[N comparable, W any] struct {
	codec  codecHolder[N]
	wcodec WeightCodec[W]

	// applyWaiters maps a transaction sequence to the parking slot of the one
	// committer waiting to apply it. Guarded by applyMu.
	//
	// The apply gate is a strictly sequential chain — the committer holding
	// sequence seq waits for appliedSeq == seq-1 — so a completing transaction
	// has exactly ONE possible successor, seq+1. Waking that successor directly
	// makes [Tx.advanceApply] O(1).
	//
	// It replaces a sync.Cond whose Broadcast woke EVERY parked committer on
	// every single commit; all but one immediately re-checked appliedSeq and
	// parked again, so the gate cost O(N) wakeups per commit and O(N^2) over a
	// concurrent batch. A CPU profile of 60 000 commits across 1024 concurrent
	// writers attributed 77% of all samples to sync.(*Cond).Wait beneath
	// waitApplyTurn, and throughput collapsed 3.9x from its peak at 256 writers
	// (28 400 -> 7 275 commits/s) because the woken-and-re-parked herd starved
	// the append path that fills the next group-commit batch.
	//
	// The entry count is bounded by the number of in-flight committers, which the
	// admission accounting already bounds; each entry
	// is removed by the successor's wake or consumed by its own fast path.
	applyWaiters map[uint64]chan struct{}

	g   *lpg.Graph[N, W]
	wal *wal.Writer

	inflightCond *sync.Cond

	// quiesceDone is the WRITER-ADMISSION GATE, and it is the whole of what
	// replaced the single-writer semaphore (rmp #2306).
	//
	// nil is the steady state: writers are admitted freely and concurrently, so
	// nothing here serialises independent transactions. It is non-nil only while a
	// quiesce ([Store.RunUnderCommitLock]) is in progress, and is closed when that
	// quiesce ends, which wakes every writer parked in [Store.enterWriter].
	//
	// The distinction from the semaphore it replaced matters. That semaphore did
	// two unrelated jobs with one primitive: it serialised writers (concurrency
	// control, which MVCC now owns outright) and it doubled as the quiesce
	// boundary. Keeping only the second job makes this a genuine quiesce
	// primitive — a barrier that is closed only when somebody actually needs the
	// store still — and it costs an admitted writer nothing beyond the in-flight
	// registration it already performed.
	//
	// Guarded by inflightMu, which also guards inflight, so closing the gate and
	// reading the in-flight count are one atomic step: that is what makes the
	// drain sound.
	quiesceDone chan struct{}

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

	// appliedSeq is the highest transaction sequence whose post-durability
	// in-memory apply step has completed (or been skipped, for a durable txn
	// whose apply failed or whose path performs no apply). A committer holding
	// sequence seq waits until appliedSeq == seq-1 before applying, then sets
	// appliedSeq = seq. It is advanced for EVERY consumed sequence — including a
	// transaction whose fsync failed or whose apply errored — so a failed
	// transaction never wedges the apply chain behind it.
	appliedSeq uint64

	// txnSeq is the last assigned transaction sequence number. A
	// Commit/CommitWALOnly increments it once and stamps the
	// value into every v3 op frame and the trailing [OpCommit] marker, so
	// recovery can group a transaction's frames and apply them atomically.
	// Concurrent committers increment it at the same time since rmp #2306, so the
	// atomic type carries genuine contended access and is no longer merely for
	// safe publication. Add is what makes the sequence space dense and unique
	// without any lock, which is the property the apply gate needs
	// ([Tx.waitApplyTurn]).
	txnSeq atomic.Uint64

	// inflight counts ADMITTED WRITERS: incremented in [Store.enterWriter] when a
	// transaction is admitted at Begin, decremented in [Store.exitWriter] when its
	// Commit / CommitWALOnly / Rollback has entirely finished (past SyncGroup and
	// the apply gate). It therefore covers a transaction's WHOLE lifetime.
	//
	// That is one window, where there used to be two abutting ones: the semaphore
	// covered Begin through the append, and this counter covered the append through
	// the end of the commit. A quiesce needed both, and needed them to abut
	// exactly. Merging them removes the seam rather than reasoning about it.
	inflight int

	// --- group-commit apply gate (#1507) ---
	//
	// Committers overlap freely and the fsync is coalesced across them by
	// [wal.Writer.SyncGroup]. But [Tx.Commit]'s post-durability in-memory apply
	// ([applyOp] under [lpg.Graph.ApplyVersioned], rmp #2320) must still run in
	// transaction-sequence order: applying a higher-seq transaction before a
	// lower-seq one could materialise an op against a node a not-yet-applied
	// earlier transaction was to create (lpg property writes are
	// create-on-demand), letting a snapshot reader observe a state no
	// serial schedule produces — a Consistency/Isolation regression. The apply
	// gate restores that order WITHOUT serialising the commit path around the
	// fsync: a committer waits until appliedSeq == its seq-1, applies, then
	// advances appliedSeq and wakes the next committer. rmp #2306 tried to remove
	// it and MEASURED that it cannot be: see
	// [TestApplyGate_ADurableCommitIsNeverRefusedByConflictDetection].
	//
	// applyMu guards appliedSeq and applyWaiters.
	applyMu sync.Mutex

	// --- quiesce boundary (#1507, reshaped by rmp #2306) ---
	//
	// A committer's fsync ([wal.Writer.SyncGroup]) and its sequence-ordered apply
	// run after the WAL append, and [Store.RunUnderCommitLock] — the seam
	// [store.DB] and a checkpointer use to exclude the commit path while they
	// close or truncate the WAL — must exclude all of it, not just the append.
	//
	// It does that with the admission gate (quiesceDone) and the admitted-writer
	// count (inflight), both guarded by inflightMu: close the gate so no new
	// writer is admitted, then drain the count to zero so every admitted one has
	// finished. Until rmp #2306 the first half was the capacity-one semaphore,
	// which excluded new writers only as a side effect of serialising every
	// write.
	inflightMu sync.Mutex
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
	defer metrics.Time("store.txn.NewStoreWithCodec").Stop()
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
	defer metrics.Time("store.txn.NewStoreWithCodecCapped").Stop()
	s := &Store[N, W]{
		g:         g,
		wal:       wlog,
		codec:     codec,
		maxTxnOps: resolveMaxTxnOps(maxTxnOps),
	}
	s.applyWaiters = make(map[uint64]chan struct{}, 64)
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
	defer metrics.Time("store.txn.NewStoreWithOptions").Stop()
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
	defer metrics.Time("store.txn.NewStoreWithOptionsCapped").Stop()
	s := &Store[N, W]{
		g:         g,
		wal:       wlog,
		codec:     opts.Codec,
		wcodec:    opts.WeightCodec,
		maxTxnOps: resolveMaxTxnOps(maxTxnOps),
	}
	// Resume rather than restart, so a sequence already spent in this WAL is
	// never minted a second time. See [Options.ResumeTxnSeq].
	//
	// The apply gate has to be seeded TOO, and the reason is a deadlock this test
	// found rather than a tidiness argument: waitApplyTurn parks until
	// appliedSeq == seq-1, and the predecessor of a resumed store's FIRST
	// transaction was applied by the previous store instance, which no longer
	// exists to advance anything. Left at zero, the first commit after a resume
	// waits for a sequence nobody will ever complete. Seeding both makes the
	// resumed store's first transaction its own chain head, exactly as sequence 1
	// is for a fresh store.
	s.txnSeq.Store(opts.ResumeTxnSeq)
	s.appliedSeq = opts.ResumeTxnSeq
	s.applyWaiters = make(map[uint64]chan struct{}, 64)
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
// cross-substructure-consistent ONLY when performed through an MVCC SNAPSHOT
// ([lpg.Graph.BeginRead] plus [lpg.Graph.ReadAt], released with
// [lpg.Graph.EndRead]).
// A direct accessor call made outside a snapshot (HasNodeLabel, NodeProperties,
// AdjList().Neighbours, NodeIndex().Scan, and the like) remains per-operation
// atomic, but may observe a multi-operation transaction half-applied — for
// example the edge of an edge-plus-labels write before its endpoint labels.
// An embedding application that reads this graph concurrently with committing
// transactions must therefore take a snapshot for its reads; writes go through
// the [Tx] API, which applies them under [lpg.Graph.ApplyVersioned] and publishes
// each transaction with one atomic store into its shared commit record.
//
// The cross-substructure half of that guarantee is measured, not assumed: rmp
// #2378 found that a snapshot re-evaluated visibility per substructure against a
// mutable commit timestamp and so could straddle a commit, and fixed it in commit
// 509929e2 by pinning the verdict per commit record — zero tears in 300 runs
// against a pre-fix 2 to 5 per 100.
//
// (This paragraph used to say the guarantee held only inside lpg.Graph.View, and
// then that lpg.Graph.View did not provide it either since rmp #2320. rmp #2344
// removed that method outright; the snapshot is the only reader now.)
//
// See the lpg package documentation and docs/isolation-design.md for the full
// contract.
//
// (This used to end "and the tracked lock-free per-shard snapshot that will make every
// read transaction-consistent without the barrier". That work is not tracked and will
// not happen: rmp #2051's single atomically-published root was closed as SUPERSEDED in
// sprint 334 — per-object version chains deliver the same guarantee and are what both
// reference engines do. Every read IS transaction-consistent without the barrier
// already, by carrying an instant. rmp #2314.)
func (s *Store[N, W]) Graph() *lpg.Graph[N, W] { return s.g }

// enterWriter admits a writer and registers it as in-flight, honouring ctx.
//
// In the steady state — no quiesce in progress — it does NOT block: any number of
// writers are admitted concurrently, which is the point of retiring the
// single-writer semaphore (rmp #2306). It blocks only while a
// [Store.RunUnderCommitLock] has the admission gate closed, and it honours the
// caller's deadline throughout, which is the property the old cancellable
// semaphore acquire existed to provide (rmp #2174).
//
// The wait costs no goroutine: a parked writer selects on the current quiesce's
// done channel against ctx.Done(). The loop re-checks under the mutex after each
// wake because a second quiesce may have started in between; closing the gate is
// not the same as holding it.
//
// A nil return means the writer is registered and the caller MUST call
// [Store.exitWriter] exactly once.
func (s *Store[N, W]) enterWriter(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.inflightMu.Lock()
		if s.quiesceDone == nil {
			s.inflight++
			s.inflightMu.Unlock()
			return nil
		}
		done := s.quiesceDone
		s.inflightMu.Unlock()
		metrics.IncCounter("store.txn.enterWriter.blocked", 1)
		select {
		case <-done:
		case <-ctx.Done():
			metrics.IncCounter("store.txn.enterWriter.errors", 1)
			return ctx.Err()
		}
	}
}

// exitWriter deregisters an admitted writer, waking a draining quiesce when the
// count reaches zero. It must be called exactly once for every successful
// [Store.enterWriter]; an unpaired call would let a quiesce believe the store is
// still while a transaction is running.
func (s *Store[N, W]) exitWriter() {
	s.inflightMu.Lock()
	s.inflight--
	if s.inflight == 0 {
		s.inflightCond.Broadcast()
	}
	s.inflightMu.Unlock()
}

// RunUnderCommitLock runs fn with the store QUIESCED: it closes the admission
// gate so no further writer is admitted, then drains the already-admitted
// writers to zero, and only then runs fn. Both steps happen under one mutex, so
// a writer cannot slip in between them.
//
// It does NOT borrow a single-writer semaphore — there is none since rmp #2306.
// Two quiesces exclude each other explicitly (a second waits for the first to
// finish) rather than as a side effect of serialising every writer, which is
// what the retired semaphore did incidentally.
//
// While fn runs no transaction can be between Begin and its commit/rollback:
// neither a new in-memory apply (the [lpg.Graph.ApplyVersioned] window opened
// inside a transaction) nor a new WAL frame append can race fn. Since rmp #2320
// that exclusion rests on the admission gate plus the in-flight drain and no
// longer on visMu, because an ordinary write holds visMu shared.
//
// The drain itself is uncancellable, as the semaphore acquire it replaces was.
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
// would deadlock). fn MAY read the graph — the checkpointer does exactly that,
// pinning a snapshot with [lpg.Graph.BeginRead] inside this window. A read takes
// no barrier since rmp #2344 removed lpg.Graph.View, so it cannot invert a lock
// order; when fn instead takes the write barrier the order is store-lock → visMu,
// which matches the engine's own order (Begin acquires the store lock, then
// ApplyVersioned acquires visMu shared), so no new deadlock is introduced. fn's
// error is returned unwrapped.
//
// Concurrency: safe to call from any goroutine; it serialises against every
// transaction on the store.
func (s *Store[N, W]) RunUnderCommitLock(fn func() error) error {
	defer metrics.Time("store.txn.RunUnderCommitLock").Stop()
	s.inflightMu.Lock()
	// Two quiesces must not overlap: fn is a store-wide operation (wal.Close,
	// wal.Truncate, a snapshot capture) and running two concurrently is exactly
	// what the old capacity-one semaphore prevented as a side effect of
	// serialising everything. Wait for any in-progress quiesce to finish and then
	// re-check, because another waiter may win the gate first.
	for s.quiesceDone != nil {
		waitFor := s.quiesceDone
		s.inflightMu.Unlock()
		<-waitFor
		s.inflightMu.Lock()
	}
	// CLOSE THE GATE, then drain — both under the one mutex that guards them, so
	// no writer can be admitted between the two. Closing the gate stops NEW
	// writers; draining to zero waits out the ones already admitted, including
	// every committer inside a SyncGroup flush+fsync. Only then can fn touch the
	// WAL. The drain is uncancellable, as the semaphore acquire it replaces was.
	done := make(chan struct{})
	s.quiesceDone = done
	s.drainInflight()
	s.inflightMu.Unlock()

	err := fn()

	// Reopen the gate BEFORE waking the parked writers. In the other order a woken
	// writer re-checks, still sees a non-nil gate, and parks again on a channel
	// that is already closed — a busy spin instead of an admission.
	s.inflightMu.Lock()
	s.quiesceDone = nil
	s.inflightMu.Unlock()
	close(done)
	return err
}

// drainInflight blocks until no admitted writer remains. The caller must hold
// inflightMu AND have already closed the admission gate (quiesceDone != nil), or
// a newly admitted writer would make the count rise again after it reached zero
// and the drain would prove nothing. The wait is uncancellable.
func (s *Store[N, W]) drainInflight() {
	for s.inflight != 0 {
		s.inflightCond.Wait()
	}
}

// Begin opens a new transaction. The returned Tx holds the
// writer registration until Commit or Rollback runs. The admission is
// uncancellable; callers that need a deadline must use [Store.BeginCtx].
func (s *Store[N, W]) Begin() *Tx[N, W] {
	defer metrics.Time("store.txn.Begin").Stop()
	tx, _ := s.BeginCtx(context.Background())
	return tx
}

// BeginCtx is the context-aware variant of [Store.Begin].
//
// Admission does not wait for another writer: since rmp #2306 there is no
// single-writer lock to queue behind, so in the common case BeginCtx registers
// the writer and returns without blocking at all, however many writers are
// already in flight.
//
// It can still block for one reason — a quiesce
// ([Store.RunUnderCommitLock], used by store.DB.Close and the checkpointer) is
// draining the admitted writers to zero and admits nobody until it finishes.
// That wait is cancellable: BeginCtx selects against ctx.Done() and returns
// (nil, ctx.Err()) the instant ctx is cancelled or its deadline elapses, rather
// than blocking for the quiesce's full duration. This is what lets a
// deadline-bearing engine write ([cypher.Engine.RunInTx]) honour its deadline.
//
// On a nil error the returned Tx is a registered writer until Commit or
// Rollback runs; once admitted, further ctx checks happen at the caller's
// discretion. Registration is for quiesce accounting only — it excludes no
// other writer, and a collision with one is reported at commit as a retriable
// error wrapping [graph/mvcc.ErrSerializationConflict].
func (s *Store[N, W]) BeginCtx(ctx context.Context) (*Tx[N, W], error) {
	defer metrics.Time("store.txn.BeginCtx").Stop()
	if err := s.enterWriter(ctx); err != nil {
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
	// Value is the typed property value for SetNodeProperty and SetEdgeProperty
	// ops. It is the zero PropertyValue for all other op kinds.
	Value    lpg.PropertyValue
	Src, Dst N
	Weight   W
	Label    string
	// Key is the property key for SetNodeProperty, DelNodeProperty,
	// SetEdgeProperty, and DelEdgeProperty ops.
	Key string
	// ConstraintName is the user-defined constraint name carried by the
	// schema-DDL op kinds ([OpCreateConstraint], [OpDropConstraint]). For
	// those kinds Label holds the constrained node label and Key holds the
	// constrained property; ConstraintKind selects UNIQUE vs NOT NULL. It is
	// the empty string for every other op kind.
	ConstraintName string
	// Handle is the stable per-edge handle carried by the Stage-2
	// handle-bearing op kinds ([OpAddEdgeH], [OpSetEdgeLabelByHandle],
	// [OpSetEdgePropertyByHandle], [OpRemoveEdgeInstanceByHandle]). It is 0
	// for every other op kind.
	Handle uint64
	Kind   OpKind
	// ConstraintKind selects UNIQUE vs NOT NULL for the schema-DDL op kinds.
	// It is the zero value ([ConstraintUnique]) and ignored for every other
	// op kind.
	ConstraintKind ConstraintKind
	// IndexKind selects hash vs btree for the index schema-DDL op kinds
	// ([OpCreateIndex], [OpDropIndex]). It is the zero value ([IndexKindHash])
	// and ignored for every other op kind.
	IndexKind IndexKind
}

// Tx is an in-progress transaction. It is registered as an admitted writer from
// [Store.Begin] / [Store.BeginCtx] until [Tx.Commit] or [Tx.Rollback] runs, and
// buffers its mutations in an unsynchronised slice. A Tx is therefore NOT safe
// for concurrent use: it is owned by the single goroutine that opened it, which
// must drive every operation and the terminal Commit/Rollback.
//
// DISTINCT transactions, by contrast, DO run concurrently. Until rmp #2306 they
// were serialised by a capacity-one semaphore; concurrency control is now MVCC
// alone, so two transactions overlap freely and a write-write conflict between
// them is detected and reported rather than prevented by exclusion.
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
	//
	// # Handle order does NOT track WAL order, and nothing needs it to
	//
	// Audit finding E19 (docs/audit-mvcc-sole-cc-2026-08-02.md), resolved by
	// rmp #2304 as a documented non-dependency rather than by moving the mint.
	//
	// Handles are minted at BUFFER time, here, while the transaction's WAL
	// sequence is minted later, in [Tx.appendOnly]. With one writer at a time the
	// two agreed; with concurrent writers they do not —
	// a transaction can buffer a higher handle and take a lower sequence. The
	// question the finding raises is whether anything reads that correspondence.
	// Nothing does, and the two reasons are structural rather than incidental:
	//
	//   - the handle travels IN the frame ([Op.Handle]), so recovery and snapshot
	//     replay use the RECORDED value and never re-mint one. Frame order cannot
	//     change which handle an edge gets.
	//   - restoring the counter is a HIGH-WATER operation
	//     ([lpg.Graph.SeedEdgeHandle] CASes upward and returns early if the
	//     counter is already past the target), and every caller passes
	//     handle+1 — store/recovery/recovery.go, store/snapshot/apply.go and
	//     store/snapshot/edgehandles.go. A maximum is order-independent.
	//
	// What the handle contract actually promises is uniqueness and monotonicity of
	// ISSUE (edge_handle.go), and both survive: the counter is an
	// atomic.Uint64.Add, so two concurrent minters cannot collide. It never
	// promised a correlation with commit order, and no code compares two handles
	// to order them.
	//
	// So the mint stays here. Moving it under the semaphore would buy the
	// correspondence at the cost of holding the semaphore across the buffering
	// loop — which is the opposite of what rmp #2306 needs — for a property with
	// no reader.
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

// DelEdgePropertyByHandle buffers an [OpDelEdgePropertyByHandle] operation,
// removing exactly propKey from one parallel edge's per-instance property bag
// keyed to its stable `handle` on the (src, dst) pair, leaving sibling handles
// untouched. The single-key removal analogue of
// [Tx.SetEdgePropertyByHandle]; emitted for REMOVE r.x / SET r.x = null on a
// bound parallel relationship.
func (t *Tx[N, W]) DelEdgePropertyByHandle(src, dst N, handle uint64, propKey string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpDelEdgePropertyByHandle, Src: src, Dst: dst, Handle: handle, Key: propKey})
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

// RemoveEdgeByHandle buffers an [OpRemoveEdgeByHandle] operation, retiring the
// single parallel edge instance identified by the stable handle on the (src,
// dst) pair — its adjacency slot and its per-handle metadata — while leaving
// sibling instances untouched. It is the instance-precise counterpart of
// [Tx.RemoveEdge]; emitted for DELETE of a specifically-bound parallel
// relationship so the exact instance is gone after recovery (rmp #2018).
func (t *Tx[N, W]) RemoveEdgeByHandle(src, dst N, handle uint64) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N, W]{Kind: OpRemoveEdgeByHandle, Src: src, Dst: dst, Handle: handle})
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
	defer metrics.Time("store.txn.Commit").Stop()
	if t.finished {
		metrics.IncCounter("store.txn.Commit.errors", 1)
		return ErrTxFinished
	}

	// Group-commit phase 1 — APPEND: cap check, mint the transaction sequence,
	// encode and append every op frame plus the OpCommit marker. Contiguity of a
	// transaction's frames is enforced by [wal.Writer.AppendRun], which holds the
	// writer's own mutex for the framing only; nothing here serialises the
	// transaction as a whole (rmp #2306). Frame order need not match sequence
	// order and recovery does not require it — it groups a transaction by the
	// TxnSeq each frame carries.
	//
	// commitTS is 0 — "no MVCC timestamp" (rmp #2309). This is the STORE's own
	// commit path: it applies through the store, has no MVCC clock in scope, and
	// mints no commit instant. Recovery treats a zero-or-absent timestamp as
	// contributing nothing to the derived clock floor, which is exactly right here
	// — a store-only writer has no instant to restore. The MVCC path is
	// [Tx.CommitWALOnly], which is handed the timestamp its caller allocated before
	// the fsync.
	seq, hasSeq, mark, appendErr := t.appendOnly(0)
	// Pair the in-flight registration appendOnly made: cleared only after the
	// entire commit (SyncGroup + apply gate) below has finished, so a draining
	// RunUnderCommitLock never closes the WAL mid-fsync.
	defer t.store.exitWriter()

	if !hasSeq {
		// No sequence was minted (empty commit, or the cap-check rejection
		// which writes nothing). It never enters the apply gate. An empty
		// commit still flushes any prior buffered tail; the cap rejection
		// returns its error without I/O.
		if appendErr != nil {
			metrics.IncCounter("store.txn.Commit.errors", 1)
			return appendErr
		}
		// SyncBuffered, not SyncGroup: this commit appended nothing, so it has no
		// watermark of its own to acknowledge — only a courtesy flush of whatever
		// tail another committer left buffered.
		if syncErr := t.store.wal.SyncBuffered(); syncErr != nil {
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
	syncErr := t.store.wal.SyncGroup(mark)

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

	// Apply to the in-memory graph after durability is secured, as ONE write
	// transaction so the whole transaction's writes flip visible as a single
	// atomic step — no reader can observe a partially-applied transaction (audit
	// gap F3, docs/isolation-design.md).
	//
	// SHARED, not exclusive (rmp #2320): concurrent applies overlap and are
	// serialised only by the per-object latches guarding each version-chain head.
	// What makes the atomic-visibility promise now is not exclusion but the
	// transaction every op CARRIES — applyOp writes through the [lpg.WriteView]
	// this closure is handed, so every version the transaction creates points at
	// one commit record and [lpg.Graph.ApplyVersioned] publishes it with one
	// atomic store.
	//
	// rmp #2304 tried this flip before the ops carried their transaction and had to
	// revert it: with two brackets open, writes resolving their record through the
	// graph's ambient slot split one transaction across two records, and a snapshot
	// reader observed half a transaction (105 942 torn observations from
	// examples/27_concurrent_txn). That is what rmp #2320's threading removed.
	//
	// The transaction is already durable (op frames + OpCommit marker fsynced), so
	// an apply error here does not undo the commit: recovery — which builds the
	// graph without a shard-capacity cap — replays the whole transaction
	// atomically. Surface it as ErrCommittedNotApplied so the caller knows the
	// commit is durable and must not be retried (F5).
	if err := t.store.g.ApplyVersioned(func(wtx lpg.WriteTx) error {
		wv := t.store.g.Writer(wtx)
		for _, op := range t.ops {
			if aerr := applyOp(wv, op); aerr != nil {
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
func (t *Tx[N, W]) CommitWALOnly(commitTS uint64) error {
	defer metrics.Time("store.txn.CommitWALOnly").Stop()
	if t.finished {
		metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
		return ErrTxFinished
	}

	seq, hasSeq, mark, appendErr := t.appendOnly(commitTS)
	defer t.store.exitWriter()
	if !hasSeq {
		if appendErr != nil {
			metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
			return appendErr
		}
		// Nothing of our own to acknowledge; see the same branch in [Tx.Commit].
		if err := t.store.wal.SyncBuffered(); err != nil {
			metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
			return err
		}
		return nil
	}
	syncErr := t.store.wal.SyncGroup(mark)
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
// transaction applies in WAL/sequence order. Sequences are dense because they
// come from a single atomic increment ([Store.txnSeq]) — never because a lock
// serialised the minting — so the predecessor is always exactly seq-1 whether or
// not committers overlap. See the apply-gate fields on [Store].
func (t *Tx[N, W]) waitApplyTurn(seq uint64) {
	s := t.store
	s.applyMu.Lock()
	if s.appliedSeq == seq-1 {
		// Our predecessor has already applied: take the turn without parking.
		// This is the whole path at low concurrency (a single writer never
		// parks), so the gate adds no channel and no allocation there.
		s.applyMu.Unlock()
		return
	}
	// Register our own parking slot under seq, then park. Registering under
	// applyMu after the check above is what rules out a lost wakeup: a
	// predecessor that advances to seq-1 before we register cannot have missed
	// us, because it would have to hold applyMu to do so, and once it has we
	// observe appliedSeq == seq-1 on the fast path instead of parking.
	ch := acquireApplySlot()
	s.applyWaiters[seq] = ch
	s.applyMu.Unlock()

	<-ch
	releaseApplySlot(ch)
}

// advanceApply marks seq's apply step complete and wakes the committer waiting
// on seq+1. It must be called exactly once for every sequence whose turn was
// taken via [Tx.waitApplyTurn], in every outcome (success, apply error, or
// fsync failure) — otherwise a failed transaction would wedge the apply chain
// behind it.
//
// It wakes EXACTLY ONE committer — the holder of seq+1, the only sequence this
// advance can unblock — so its cost is constant in the number of parked
// committers rather than linear in it. See [Store.applyWaiters].
func (t *Tx[N, W]) advanceApply(seq uint64) {
	s := t.store
	s.applyMu.Lock()
	s.appliedSeq = seq
	successor, parked := s.applyWaiters[seq+1]
	if parked {
		delete(s.applyWaiters, seq+1)
	}
	s.applyMu.Unlock()
	if parked {
		// Buffered with capacity one and sent to exactly once (the slot was
		// removed from the map above, so no other advance can reach it), so this
		// never blocks.
		successor <- struct{}{}
	}
}

// applySlotPool recycles apply-gate parking slots so a parked committer
// allocates no channel per transaction. A slot is returned to the pool only
// after its single value has been received, so a pooled channel is always
// empty.
var applySlotPool = sync.Pool{
	New: func() any { return make(chan struct{}, 1) },
}

func acquireApplySlot() chan struct{} { return applySlotPool.Get().(chan struct{}) }

func releaseApplySlot(ch chan struct{}) { applySlotPool.Put(ch) }

// appendOnly performs group-commit phase 1: it encodes and appends every
// buffered op to the WAL (without fsyncing), leaving the fsync to a separate,
// coalesced step ([wal.Writer.SyncGroup]) so one fsync amortises across every
// transaction that appended while it was pending. It is the append half of the
// old appendAndSync.
//
// It releases no writer lock, because since rmp #2306 there is none to release:
// concurrent appenders are admitted freely and the WAL writer serialises the
// appends themselves. Before that, this function's release of the capacity-one
// semaphore is what let the next transaction append while this one fsynced —
// which is why group commit was already reachable here, and why retiring the
// semaphore measured throughput-neutral.
//
// Every op is encoded as a v3 frame carrying a fresh per-transaction sequence
// ([Store.txnSeq]), and an [OpCommit] marker frame
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
func (t *Tx[N, W]) appendOnly(commitTS uint64) (seq uint64, hasSeq bool, watermark int64, err error) {
	if len(t.ops) == 0 {
		// Empty commit: mint no sequence and write no marker. The caller still
		// runs SyncGroup to flush any prior buffered tail (the historical
		// no-op-with-Sync behaviour), then applies nothing.
		t.markFinished()
		return 0, false, 0, nil
	}
	// Bounded resources / Durability: reject an over-cap transaction BEFORE
	// minting a sequence or writing any frame, so a transaction recovery could
	// not replay without unbounded buffering is never made durable and never
	// consumes a sequence slot. The producer cap is <= the recovery cap, so
	// every transaction that passes here is guaranteed to fit recovery's buffer
	// (see [ErrTransactionTooLarge], [DefaultMaxTxnOps]).
	if t.store.maxTxnOps > 0 && len(t.ops) > t.store.maxTxnOps {
		metrics.IncCounter("store.txn.appendOnly.txnTooLarge", 1)
		t.markFinished()
		return 0, false, 0, fmt.Errorf("%w: %d ops > cap %d", ErrTransactionTooLarge, len(t.ops), t.store.maxTxnOps)
	}
	// Mint the sequence. From here hasSeq is true on every return: the sequence
	// is consumed, so the caller must advance the apply gate past it even if the
	// append below fails (a gap would deadlock the dense sequence chain). A
	// partial append is harmless on disk — recovery discards any frames not
	// followed by a durable matching OpCommit marker — and the err makes the
	// caller skip the in-memory apply.
	seq = t.store.txnSeq.Add(1)
	// One scratch buffer, borrowed from the pool, is reused for every op frame
	// and the trailing OpCommit marker. wal.Append copies each encoded payload
	// into its bufio buffer synchronously, so the scratch is safe to reset and
	// reuse for the next op (see encodeScratchPool). The buffer is returned to
	// the pool on every exit path, including encode/append failures.
	scratch := getEncodeScratch()
	defer putEncodeScratch(scratch)
	// ONE contiguous run, not a loop of independent appends (rmp #2302, audit
	// finding E5). Recovery commits the ops carrying a marker's own TxnSeq and
	// discards the buffered prefix as orphaned, which is correct only while a
	// transaction's frames cannot interleave with another's. That property used to
	// rest on the store semaphore that used to be held here; it now rests on the
	// WAL writer itself, which is the component that owns the file. See
	// [wal.Writer.AppendRun] and docs/design-wal-transaction-contiguity.md.
	//
	// The per-op encoding happens INSIDE the run, so the pooled scratch buffer is
	// still reused for every frame and the commit allocates no more than before.
	mark, aerr := t.store.wal.AppendRun(func(emit func([]byte) error) error {
		for _, op := range t.ops {
			payload, enErr := encodeOpTypedV3Into((*scratch)[:0], op, seq, t.store.codec, t.store.wcodec)
			if enErr != nil {
				return enErr
			}
			*scratch = payload // retain the (possibly grown) backing array for reuse
			if err := emit(payload); err != nil {
				return err
			}
		}
		marker := encodeCommitV3Into((*scratch)[:0], seq, commitTS)
		*scratch = marker
		return emit(marker)
	})
	if aerr != nil {
		t.markFinished()
		return seq, true, mark, aerr
	}
	// Frames + marker are buffered. Release the semaphore so the next
	// transaction can append while this one fsyncs (group-commit coalescing).
	t.markFinished()
	return seq, true, mark, nil
}

// Rollback discards buffered ops without touching the WAL or graph.
func (t *Tx[N, W]) Rollback() error {
	defer metrics.Time("store.txn.Rollback").Stop()
	if t.finished {
		metrics.IncCounter("store.txn.Rollback.errors", 1)
		return ErrTxFinished
	}
	t.markFinished()
	t.store.exitWriter()
	return nil
}

// markFinished marks the transaction as having consumed its lifecycle, exactly
// once, so a second Commit / CommitWALOnly / Rollback is rejected with
// [ErrTxFinished] rather than acting twice.
//
// It no longer releases anything. Until rmp #2306 this was releaseAfterAppend and
// it freed the single-writer semaphore here — mid-commit, right after the append
// — so that the coalesced fsync did not hold the writer lock. With the semaphore
// retired there is nothing to free at this point: the writer's registration spans
// the whole transaction and is cleared by [Store.exitWriter] when the commit has
// entirely finished. Rollback, which does no commit, calls both.
func (t *Tx[N, W]) markFinished() {
	t.finished = true
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

// encodeScratchPool recycles the byte buffers used to serialise WAL op
// payloads on the commit hot path. Each [Tx.appendOnly] op borrows a buffer,
// the codec encoders append the payload into it, [wal.Writer.Append] copies
// that payload into its bufio buffer SYNCHRONOUSLY (bufio.Writer.Write copies
// into its internal buffer before returning), and the buffer is then reset to
// length 0 (its capacity retained) and returned to the pool for the next op.
//
// Reuse is safe precisely because Append's copy completes before the borrow
// returns: no reference to the buffer's backing array survives the Append call,
// so the next op's encode cannot observe or corrupt a frame already handed to
// the WAL. This mirrors RocksDB's reused WriteBatch rep buffer — one scratch
// buffer amortised across a transaction's ops and across transactions — and
// removes the per-op `make([]byte, …)` that the txn layer previously paid for
// every frame (#1509).
//
// The pool holds *[]byte (not []byte) so a Put stores no allocation: a []byte
// placed in a sync.Pool directly would box the slice header on every Put.
var encodeScratchPool = sync.Pool{
	New: func() any {
		// 256 bytes covers the overwhelming majority of op frames (version +
		// kind + txnSeq + endpoints + a short label/property) without a grow,
		// matching the historical `headroom + len(label) + 32` sizing. Larger
		// payloads grow the buffer once and that larger capacity is retained
		// in the pool, so the steady state is allocation-free.
		b := make([]byte, 0, 256)
		return &b
	},
}

// getEncodeScratch borrows a zero-length, capacity-retaining scratch buffer.
func getEncodeScratch() *[]byte {
	return encodeScratchPool.Get().(*[]byte)
}

// putEncodeScratch resets the buffer to length 0 (retaining its grown
// capacity) and returns it to the pool. The caller must hold no live reference
// to the buffer's contents — true on the commit path because [wal.Writer.Append]
// has already copied the payload synchronously before this is called.
func putEncodeScratch(b *[]byte) {
	*b = (*b)[:0]
	encodeScratchPool.Put(b)
}

// encodeOpTypedV3Into serialises one op to a v3 ([OpRecordV3]) WAL payload,
// appending into the supplied buffer (which must be length 0) and returning the
// extended slice. It is the pool-aware sibling of [encodeOpTypedV3]: the byte
// layout produced is identical (version + kind + 8-byte txnSeq + v2 body), so
// the on-disk frame is byte-for-byte the same regardless of which encoder built
// the payload.
func encodeOpTypedV3Into[N comparable, W any](buf []byte, op Op[N, W], seq uint64, codec Codec[N], wcodec WeightCodec[W]) ([]byte, error) {
	buf = append(buf, OpRecordV3, byte(op.Kind))
	buf = binary.LittleEndian.AppendUint64(buf, seq)
	return appendOpBodyTyped(buf, op, codec, wcodec)
}

// encodeCommitV3Into serialises the [OpCommit] marker into the supplied buffer
// (which must be length 0), returning the extended slice. Pool-aware sibling of
// [encodeCommitV3]; the bytes produced are identical.
//
// # The commit timestamp, and why it needs no format bump (rmp #2309)
//
// commitTS is the MVCC instant at which this transaction becomes visible, or zero
// for a writer with no MVCC clock (the store's own [Tx.Commit] path). It is
// appended to the [OpCommit] body, which was previously empty, so that recovery can
// DERIVE the clock's floor from the WAL instead of trusting a persisted counter —
// the shape InnoDB and Memgraph both settled on, and the reason no separate counter
// record exists. See docs/design-mvcc-clock-recovery.md.
//
// This is deliberately NOT a format bump, and that was verified against the code
// rather than assumed. The WAL frame header is magic + version + length + crc32c
// (store/wal/format.go), so it carries no per-record shape. Inside the frame,
// recovery's decodeV3 copies everything after the txnSeq word verbatim into
// Op.Body, and [OpCommit]'s body was ignored by the replay state machine. So:
//
//   - an OLDER reader ignores the extra 8 bytes entirely;
//   - a NEWER reader on an OLDER file sees an empty body and contributes nothing to
//     the derived maximum, falling back to the floor.
//
// The compatibility policy is therefore "absent body means no timestamp", which is
// a test obligation rather than a version negotiation. Neither [CurrentVersion] nor
// [OpRecordV3] changes.
func encodeCommitV3Into(buf []byte, seq, commitTS uint64) []byte {
	buf = append(buf, OpRecordV3, byte(OpCommit))
	buf = binary.LittleEndian.AppendUint64(buf, seq)
	return binary.LittleEndian.AppendUint64(buf, commitTS)
}

// encodeOpTypedV3 serialises one op to a v3 ([OpRecordV3]) WAL payload:
// the v2 header (version + kind) plus an 8-byte little-endian transaction
// sequence, followed by the byte-identical v2 body for that kind. The
// sequence groups a transaction's frames so recovery can apply them
// atomically (see [OpRecordV3] and [OpCommit]).
//
// It is the allocating convenience form of [encodeOpTypedV3Into]: it mints a
// fresh, exactly-sized buffer and delegates, so the bytes it returns are
// identical to the pooled commit path's. The commit hot path uses the Into
// form against a pooled buffer (#1509); this form remains the canonical,
// self-contained reference encoder (it is exercised by the byte-identity test).
func encodeOpTypedV3[N comparable, W any](op Op[N, W], seq uint64, codec Codec[N], wcodec WeightCodec[W]) ([]byte, error) {
	const headroom = 2 + 8 + 2 // version + kind + txnSeq + uint16 labelLen
	buf := make([]byte, 0, headroom+len(op.Label)+32)
	return encodeOpTypedV3Into(buf, op, seq, codec, wcodec)
}

// encodeCommitV3 serialises the [OpCommit] marker for a v3 transaction:
// version + kind + the transaction sequence + the MVCC commit timestamp. Recovery
// applies the buffered ops carrying the same sequence when it reads this
// frame; a torn write that loses it discards the whole transaction.
//
// Allocating convenience form of [encodeCommitV3Into]; the bytes are identical.
func encodeCommitV3(seq, commitTS uint64) []byte {
	buf := make([]byte, 0, 2+8+8)
	return encodeCommitV3Into(buf, seq, commitTS)
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
	case OpSetEdgePropertyByHandle, OpDelEdgePropertyByHandle:
		// Edge-property body followed by the 8-byte stable handle. The value is
		// appended only for the Set kind (the guard inside encodeOpEdgeProperty
		// omits it for OpDelEdgePropertyByHandle), so the Del frame is
		// byte-symmetric to OpDelEdgeProperty plus the trailing handle.
		var err error
		if buf, err = encodeOpEdgeProperty(buf, op, codec); err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint64(buf, op.Handle), nil
	case OpRemoveEdgeInstanceByHandle, OpRemoveEdgeByHandle:
		// Edge-no-tail body ([Src, Dst, uint16=0]) followed by the 8-byte stable
		// handle. OpRemoveEdgeByHandle is byte-identical to
		// OpRemoveEdgeInstanceByHandle apart from the kind byte.
		var err error
		if buf, err = encodeOpEdgeNoTail(buf, op, codec); err != nil {
			return nil, err
		}
		return binary.LittleEndian.AppendUint64(buf, op.Handle), nil
	case OpCreateConstraint, OpDropConstraint:
		// Schema record: a one-byte constraint-kind tag followed by three
		// length-prefixed strings (label, property, name). No node codec is
		// involved — a constraint carries no endpoints.
		return appendOpConstraintBody(buf, op)
	case OpCreateIndex, OpDropIndex:
		// Index schema record: a one-byte index-kind tag followed by three
		// length-prefixed strings (name, label, property). No node codec is
		// involved — an index definition carries no endpoints.
		return appendOpIndexBody(buf, op)
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
// sibling body encoders. A schema identifier this long is rejected upstream at
// the DDL boundary (cypher/ir maxSchemaIdentifierLen, #1903); the encoder fails
// stop here as a last line of defence so a caller that bypasses that boundary
// (e.g. the embedded Go API) can never silently truncate a label, property, or
// name and corrupt the WAL — an ACID Consistency/Durability breach. The body is
// independent of the node [Codec] because a constraint has no endpoints.
func appendOpConstraintBody[N comparable, W any](buf []byte, op Op[N, W]) ([]byte, error) {
	if err := checkWALSchemaString("constraint label", op.Label); err != nil {
		return nil, err
	}
	if err := checkWALSchemaString("constraint property", op.Key); err != nil {
		return nil, err
	}
	if err := checkWALSchemaString("constraint name", op.ConstraintName); err != nil {
		return nil, err
	}
	buf = append(buf, byte(op.ConstraintKind))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
	buf = append(buf, op.Label...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Key)))
	buf = append(buf, op.Key...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.ConstraintName)))
	buf = append(buf, op.ConstraintName...)
	return buf, nil
}

// maxWALSchemaStringLen is the largest byte length the uint16 length prefix in
// the schema op-body encoders can represent without truncation. Schema
// identifiers are capped far below this at the DDL boundary (#1903); this bound
// is the encoder's own fail-stop guard.
const maxWALSchemaStringLen = 1<<16 - 1

// checkWALSchemaString rejects a schema identifier whose byte length would
// overflow the uint16 length prefix, converting silent truncation into a
// fail-stop commit error (#1903).
func checkWALSchemaString(what, s string) error {
	if len(s) > maxWALSchemaStringLen {
		return fmt.Errorf("txn: %s is too long to encode (%d bytes; maximum %d)",
			what, len(s), maxWALSchemaStringLen)
	}
	return nil
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
// carries no endpoints. Like [appendOpConstraintBody] it fails stop on a schema
// string too long for the uint16 length prefix (#1903).
func appendOpIndexBody[N comparable, W any](buf []byte, op Op[N, W]) ([]byte, error) {
	if err := checkWALSchemaString("index name", op.ConstraintName); err != nil {
		return nil, err
	}
	if err := checkWALSchemaString("index label", op.Label); err != nil {
		return nil, err
	}
	if err := checkWALSchemaString("index property", op.Key); err != nil {
		return nil, err
	}
	buf = append(buf, byte(op.IndexKind))
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.ConstraintName)))
	buf = append(buf, op.ConstraintName...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
	buf = append(buf, op.Label...)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Key)))
	buf = append(buf, op.Key...)
	return buf, nil
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
func applyOp[N comparable, W any](wv lpg.WriteView[N, W], op Op[N, W]) error {
	switch op.Kind {
	case OpAddEdge:
		// rmp #1871: bump the cache-invalidation generation counter on
		// success. Unconditional (no "already existed" signal available from
		// plain AddEdge) rather than gated: an extra bump past a genuine
		// topology change is unobservable (any later reader sees the same
		// final counter value regardless), so unconditional is the safe
		// default here, never a source of a false cache hit.
		var zero W
		if err := wv.AddEdge(op.Src, op.Dst, zero); err != nil {
			return err
		}
		wv.Graph().BumpTopoGeneration()
	case OpAddEdgeWeighted:
		if err := wv.AddEdge(op.Src, op.Dst, op.Weight); err != nil {
			return err
		}
		wv.Graph().BumpTopoGeneration()
	case OpAddEdgeH:
		// Handle-bearing add: idempotent against a slot already carrying
		// this handle (the snapshot loaded it, or an earlier frame applied
		// it), so snapshot + full-WAL recovery does not double the edge.
		// Only bump on a genuine insert (rmp #1871): a Cypher-driven write
		// already applied this edge eagerly and bumped the generation once,
		// through the engine's own adapter, before ever reaching this
		// replay-time call — inserted is false here for that case, so this
		// does not double-bump (harmless either way, but pointless).
		inserted, err := wv.AddEdgeHIfAbsent(op.Src, op.Dst, op.Weight, op.Handle)
		if err != nil {
			return err
		}
		if inserted {
			wv.Graph().BumpTopoGeneration()
		}
	case OpSetEdgeLabelByHandle:
		wv.SetEdgeLabelByHandle(op.Src, op.Dst, op.Handle, op.Label)
	case OpSetEdgePropertyByHandle:
		return wv.SetEdgePropertyByHandle(op.Src, op.Dst, op.Handle, op.Key, op.Value)
	case OpDelEdgePropertyByHandle:
		wv.DelEdgePropertyByHandle(op.Src, op.Dst, op.Handle, op.Key)
	case OpRemoveEdgeInstanceByHandle:
		wv.RemoveEdgeInstanceByHandle(op.Src, op.Dst, op.Handle)
		wv.Graph().BumpTopoGeneration() // rmp #1871; unconditional, see OpAddEdge above
	case OpRemoveEdgeByHandle:
		// Instance-precise removal: retire the exact parallel slot carrying the
		// handle plus its per-handle metadata (rmp #2018). Unconditional
		// generation bump mirrors OpRemoveEdge.
		wv.RemoveEdgeByHandle(op.Src, op.Dst, op.Handle)
		wv.Graph().BumpTopoGeneration()
	case OpSetNodeLabel:
		return wv.SetNodeLabel(op.Src, op.Label)
	case OpSetEdgeLabel:
		wv.SetEdgeLabel(op.Src, op.Dst, op.Label)
	case OpAddNode:
		return wv.AddNode(op.Src)
	case OpRemoveNode:
		// Logical removal: the mapper entry is permanent, so removal is a
		// tombstone. Strip all labels and properties so the node is
		// unreachable via label/property queries, then tombstone it so it
		// is excluded from live scans and counts. Tombstoning here keeps
		// the in-memory state applied by a committed Tx identical to the
		// state reconstructed by WAL replay (recovery.applyOpCodec does the
		// same), so live and recovered graphs agree. A later OpAddNode for
		// the same key revives it (g.AddNode clears the tombstone).
		// Enumerated through the TRANSACTION'S OWN view (rmp #2320), not through
		// the present: this apply overlaps other writers' applies, so the present
		// carries their uncommitted labels and properties. Stripping those would
		// tear another transaction apart, and missing this transaction's own
		// earlier ops would leave the tombstoned node still label-reachable.
		rv := wv.Read()
		for _, lbl := range rv.NodeLabels(op.Src) {
			wv.RemoveNodeLabel(op.Src, lbl)
		}
		for k := range rv.NodeProperties(op.Src) {
			wv.DelNodeProperty(op.Src, k)
		}
		wv.RemoveNode(op.Src)
	case OpRemoveNodeLabel:
		wv.RemoveNodeLabel(op.Src, op.Label)
	case OpSetNodeProperty:
		return wv.SetNodeProperty(op.Src, op.Key, op.Value)
	case OpDelNodeProperty:
		wv.DelNodeProperty(op.Src, op.Key)
	case OpRemoveEdge:
		// Use the LPG edge removal so a fully-disconnected pair also sheds
		// its per-pair edge labels/properties (matching recovery replay),
		// preventing a later re-add from resurrecting them.
		wv.RemoveEdge(op.Src, op.Dst)
		wv.Graph().BumpTopoGeneration() // rmp #1871; unconditional, see OpAddEdge above
	case OpSetEdgeProperty:
		return wv.SetEdgeProperty(op.Src, op.Dst, op.Key, op.Value)
	case OpDelEdgeProperty:
		wv.DelEdgeProperty(op.Src, op.Dst, op.Key)
	case OpCreateConstraint:
		// The store keeps no constraint registry of its own (constraint
		// enforcement lives in the cypher engine), so the only in-memory effect
		// is to drive the graph's store-direct constraint count. That count
		// makes Graph.HasConstraints true for a txn.Store-direct embedder that
		// never goes through the engine's SetActiveConstraintCount, so a
		// WAL-truncating checkpoint correctly judges its snapshot NOT
		// self-sufficient and retains the OpCreateConstraint frame (#1756).
		wv.Graph().AddStoreConstraint(uint8(op.ConstraintKind), op.Label, op.Key)
	case OpDropConstraint:
		// Mirror the create: drop the store-direct constraint slot so the count
		// falls back to zero once the last constraint is removed.
		wv.Graph().RemoveStoreConstraint(uint8(op.ConstraintKind), op.Label, op.Key)
	case OpCreateIndex:
		// As with constraints, the store keeps no index registry of its own
		// (index maintenance lives in the cypher engine), so the only in-memory
		// effect is to drive the graph's store-direct index count. That count
		// makes Graph.HasIndexes true for a txn.Store-direct embedder that never
		// goes through the engine's index-def registry, so a WAL-truncating
		// checkpoint correctly judges its snapshot NOT self-sufficient and
		// retains the OpCreateIndex frame (#1755). ConstraintName carries the
		// index name for the index ops (see Tx.CreateIndex / Tx.DropIndex).
		wv.Graph().AddStoreIndex(op.ConstraintName)
	case OpDropIndex:
		// Mirror the create: drop the store-direct index slot so the count falls
		// back to zero once the last index is removed.
		wv.Graph().RemoveStoreIndex(op.ConstraintName)
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
