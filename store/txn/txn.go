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
// transactions serialise on the store mutex, so only one Tx is
// active at any moment. Reads on the underlying lpg.Graph remain
// concurrent and lock-free per the lpg/adjlist contracts.
type Store[N comparable, W any] struct {
	mu     sync.Mutex
	g      *lpg.Graph[N, W]
	wal    *wal.Writer
	codec  codecHolder[N]
	wcodec WeightCodec[W]

	// txnSeq is the last assigned transaction sequence number. A
	// Commit/CommitWALOnly increments it once and stamps the
	// value into every v3 op frame and the trailing [OpCommit] marker, so
	// recovery can group a transaction's frames and apply them atomically.
	// It is incremented only while the store mutex is held (the single-
	// writer lock acquired in Begin), so the atomic type is for safe
	// publication rather than contended access.
	txnSeq atomic.Uint64
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
	return &Store[N, W]{
		g:     g,
		wal:   wlog,
		codec: codec,
	}
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
	return &Store[N, W]{
		g:      g,
		wal:    wlog,
		codec:  opts.Codec,
		wcodec: opts.WeightCodec,
	}
}

// Codec returns the [Codec] installed on the Store. The returned value
// is the same one passed to [NewStoreWithCodec] or [NewStoreWithOptions].
// Callers should treat the return as read-only.
func (s *Store[N, W]) Codec() Codec[N] { return s.codec }

// WeightCodec returns the [WeightCodec] installed on the Store, or nil
// if the store was constructed without one. Callers should treat the
// return as read-only.
func (s *Store[N, W]) WeightCodec() WeightCodec[W] { return s.wcodec }

// Graph returns the underlying graph.
func (s *Store[N, W]) Graph() *lpg.Graph[N, W] { return s.g }

// Begin opens a new transaction. The returned Tx holds the
// store's single-writer mutex until Commit or Rollback runs.
func (s *Store[N, W]) Begin() *Tx[N, W] {
	defer metrics.Time("store.txn.Begin")()
	tx, _ := s.BeginCtx(context.Background())
	return tx
}

// BeginCtx is the context-aware variant of [Store.Begin]. ctx.Err()
// is checked before acquiring the store mutex; on cancellation returns
// (nil, wrapped ctx.Err). Once the lock is held the transaction
// proceeds; further ctx checks happen at the caller's discretion.
func (s *Store[N, W]) BeginCtx(ctx context.Context) (*Tx[N, W], error) {
	defer metrics.Time("store.txn.BeginCtx")()
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("store.txn.BeginCtx.errors", 1)
		return nil, err
	}
	s.mu.Lock()
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
}

// Tx is an in-progress transaction.
type Tx[N comparable, W any] struct {
	store    *Store[N, W]
	ops      []Op[N, W]
	finished bool
}

// AddEdge buffers an AddEdge(src, dst, w) operation on the graph.
//
// If the store was constructed with a [WeightCodec] (via
// [NewStoreWithOptions]), the operation is recorded as an
// [OpAddEdgeWeighted] frame carrying w on commit. If the store has no
// weight codec, AddEdge accepts a zero-value w (which buffers an
// [OpAddEdge] frame, producing a zero-weight edge on replay) and
// returns [ErrNoWeightCodec] for any non-zero w. Callers needing
// durable weighted edges must use [NewStoreWithOptions].
func (t *Tx[N, W]) AddEdge(src, dst N, w W) error {
	if t.finished {
		return ErrTxFinished
	}
	if t.store.wcodec == nil {
		if !isZero(w) {
			return ErrNoWeightCodec
		}
		t.ops = append(t.ops, Op[N, W]{Kind: OpAddEdge, Src: src, Dst: dst})
		return nil
	}
	// Weight-codec store: emit a handle-bearing OpAddEdgeH so the edge keeps
	// a stable per-edge identity across recovery (Stage 2). The handle is
	// minted now, from the graph's monotone counter, so the value is fixed
	// in the WAL frame before the commit fsync; replay re-inserts it via
	// AddEdgeHIfAbsent (idempotent against a snapshot that already loaded
	// it). The legacy zero-weight OpAddEdge path above is unchanged.
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
	defer t.release()

	if err := t.appendAndSync(); err != nil {
		metrics.IncCounter("store.txn.Commit.errors", 1)
		return err
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
func (t *Tx[N, W]) CommitWALOnly() error {
	defer metrics.Time("store.txn.CommitWALOnly")()
	if t.finished {
		metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
		return ErrTxFinished
	}
	defer t.release()

	if err := t.appendAndSync(); err != nil {
		metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
		return err
	}
	return nil
}

// appendAndSync writes the transaction's ops to the WAL and fsyncs them.
//
// Every op is encoded as a v3 frame carrying a fresh per-transaction
// sequence ([Store.txnSeq]), and an [OpCommit] marker frame for the same
// sequence is appended after the last op; a single [wal.Writer.Sync] then
// makes the whole batch durable. The marker is the atomicity boundary:
// bufio may auto-flush a prefix of frames to the OS before the Sync, but
// that is benign — durability is gated on the fsync, and recovery discards
// any op frames not followed by a durable matching marker, so a torn batch
// recovers all-or-nothing.
func (t *Tx[N, W]) appendAndSync() error {
	if len(t.ops) == 0 {
		// Empty commit: preserve the historical no-op-with-Sync behaviour
		// (flush any prior buffered tail) without writing a lone marker.
		return t.store.wal.Sync()
	}
	seq := t.store.txnSeq.Add(1)
	for _, op := range t.ops {
		payload, enErr := encodeOpTypedV3(op, seq, t.store.codec, t.store.wcodec)
		if enErr != nil {
			return enErr
		}
		if err := t.store.wal.Append(payload); err != nil {
			return err
		}
	}
	if err := t.store.wal.Append(encodeCommitV3(seq)); err != nil {
		return err
	}
	return t.store.wal.Sync()
}

// Rollback discards buffered ops without touching the WAL or graph.
func (t *Tx[N, W]) Rollback() error {
	defer metrics.Time("store.txn.Rollback")()
	if t.finished {
		metrics.IncCounter("store.txn.Rollback.errors", 1)
		return ErrTxFinished
	}
	t.release()
	return nil
}

func (t *Tx[N, W]) release() {
	t.finished = true
	t.store.mu.Unlock()
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
	default: // OpAddEdge, OpSetNodeLabel, OpSetEdgeLabel
		return encodeOpEdgeWithLabel(buf, op, codec)
	}
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
	elems := make([]lpg.PropertyValue, 0, count)
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
		g.SetEdgePropertyByHandle(op.Src, op.Dst, op.Handle, op.Key, op.Value)
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
