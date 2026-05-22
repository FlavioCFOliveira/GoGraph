// Package txn provides the transactional surface (Begin / Commit /
// Rollback) layered over an [lpg.Graph] and a [wal.Writer].
//
// A transaction buffers mutations in a per-Tx slice. Commit appends
// each mutation as a single WAL frame, fsyncs the WAL, and only then
// applies the mutations to the in-memory graph — so a process crash
// between Commit's WAL sync and the in-memory apply is recoverable
// by replaying the WAL into a fresh graph.
//
// Single-writer is enforced by a per-store mutex acquired in Begin
// and released in Commit or Rollback; reads on the underlying graph
// remain lock-free in the lpg / adjlist contracts.
//
// # Constructor matrix
//
// The package exposes three constructors that trade durability of
// edge weights for backwards-compatibility:
//
//   - [NewStore] — legacy fmt.Sprintf codec, no weight codec; emits
//     v1 untagged frames and only [OpAddEdge]. [Tx.AddEdge] with a
//     non-zero weight returns [ErrNoWeightCodec]; zero-weight calls
//     buffer an [OpAddEdge] record.
//   - [NewStoreWithCodec] — typed N codec, no weight codec; emits v2
//     tagged frames and only [OpAddEdge]. Same weight semantics as
//     [NewStore].
//   - [NewStoreWithOptions] — typed N codec plus typed W codec; emits
//     v2 frames with [OpAddEdgeWeighted] for every [Tx.AddEdge] call
//     (the weight payload is written even when the caller passes the
//     zero value of W, so the wire shape stays unambiguous).
package txn

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	"gograph/graph/lpg"
	"gograph/internal/metrics"
	"gograph/store/wal"
)

// ErrTxFinished is returned by operations on a transaction that has
// already been committed or rolled back.
var ErrTxFinished = errors.New("txn: transaction already finished")

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
	// OpRecordV1 is the logical version of legacy untagged records.
	// The byte is never written to disk; the constant exists so call
	// sites can name the version they expect.
	OpRecordV1 uint8 = 0
	// OpRecordV2 is the magic byte that marks the start of a v2-tagged
	// op record. See the package doc above for the rationale.
	OpRecordV2 uint8 = 0xFE
)

// codecHolder is the type-erased view of [Codec] used by Store so the
// Store struct itself does not need to be parameterised on whether the
// codec is the legacy fmt fallback or a typed implementation. Methods
// on the holder are called from the Commit fast path; the indirection
// is a single interface dispatch per op.
type codecHolder[N comparable] interface {
	Codec[N]
}

// Options carries the codecs used by [NewStoreWithOptions]. Both
// fields are required: Codec serialises endpoint identifiers and
// WeightCodec serialises edge weights for [OpAddEdgeWeighted] records.
//
// A nil WeightCodec is rejected by [NewStoreWithOptions]; callers that
// do not need durable weights should use [NewStoreWithCodec] (or
// [NewStore]) instead.
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
	legacy bool
}

// NewStore returns a Store wrapping g and wal. The store emits v1
// (untagged, fmt.Sprintf-based) WAL payloads so that callers that
// existed prior to the typed codec introduction observe byte-identical
// on-disk frames.
//
// The returned store has no [WeightCodec]; [Tx.AddEdge] called with a
// non-zero weight returns [ErrNoWeightCodec]. Callers that need
// durable weighted edges should use [NewStoreWithOptions].
//
// New code that does not need durable weights but does want a stable
// endpoint encoding should prefer [NewStoreWithCodec], which installs
// a typed [Codec] and emits v2 (tagged) frames that survive arbitrary
// N types.
func NewStore[N comparable, W any](g *lpg.Graph[N, W], wlog *wal.Writer) *Store[N, W] {
	return &Store[N, W]{
		g:      g,
		wal:    wlog,
		codec:  legacyFmtCodec[N]{},
		legacy: true,
	}
}

// NewStoreWithCodec returns a Store wrapping g and wal that encodes
// node identifiers via the supplied typed [Codec]. Each WAL payload is
// emitted in the v2 format: a one-byte version tag ([OpRecordV2]),
// then the [OpKind], then the codec-encoded src and dst values
// inline, then a uint16 little-endian label length and the label
// bytes. The frame is the dual of the v2 branch in
// [store/recovery.Decode], which detects the version tag and walks
// the body back through the same codec.
//
// codec must not be nil. The function does not validate that codec
// is non-legacy; passing the legacy fmt codec is undefined behaviour.
//
// The returned store has no [WeightCodec]; [Tx.AddEdge] called with a
// non-zero weight returns [ErrNoWeightCodec]. Callers that need
// durable weighted edges should use [NewStoreWithOptions].
func NewStoreWithCodec[N comparable, W any](g *lpg.Graph[N, W], wlog *wal.Writer, codec Codec[N]) *Store[N, W] {
	defer metrics.Time("store.txn.NewStoreWithCodec")()
	return &Store[N, W]{
		g:      g,
		wal:    wlog,
		codec:  codec,
		legacy: isLegacyCodec[N](codec),
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
		legacy: isLegacyCodec[N](opts.Codec),
	}
}

// Codec returns the [Codec] installed on the Store. The returned value
// is the same one passed to [NewStoreWithCodec], or the internal legacy
// codec installed by [NewStore]. Callers should treat the return as
// read-only.
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
	t.ops = append(t.ops, Op[N, W]{Kind: OpAddEdgeWeighted, Src: src, Dst: dst, Weight: w})
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

// Commit fsync-appends every buffered op to the WAL and only then
// applies it to the in-memory graph.
func (t *Tx[N, W]) Commit() error {
	defer metrics.Time("store.txn.Commit")()
	if t.finished {
		metrics.IncCounter("store.txn.Commit.errors", 1)
		return ErrTxFinished
	}
	defer t.release()

	// Encode every op as a separate WAL frame so a torn write at
	// any point in the batch only loses tail ops, never partial
	// ones.
	for _, op := range t.ops {
		var payload []byte
		if t.store.legacy {
			payload = encodeOpLegacy(op)
		} else {
			var enErr error
			payload, enErr = encodeOpTyped(op, t.store.codec, t.store.wcodec)
			if enErr != nil {
				metrics.IncCounter("store.txn.Commit.errors", 1)
				return enErr
			}
		}
		if err := t.store.wal.Append(payload); err != nil {
			metrics.IncCounter("store.txn.Commit.errors", 1)
			return err
		}
	}
	if err := t.store.wal.Sync(); err != nil {
		metrics.IncCounter("store.txn.Commit.errors", 1)
		return err
	}
	// Apply to the in-memory graph after durability is secured.
	for _, op := range t.ops {
		if err := applyOp(t.store.g, op); err != nil {
			metrics.IncCounter("store.txn.Commit.applyErrors", 1)
			return err
		}
	}
	return nil
}

// CommitWALOnly fsync-appends every buffered op to the WAL but does NOT
// apply the ops to the in-memory graph. Use this when the caller has
// already applied mutations eagerly (e.g. [walMutatorAdapter]) and only
// needs WAL durability without a second in-memory pass.
func (t *Tx[N, W]) CommitWALOnly() error {
	defer metrics.Time("store.txn.CommitWALOnly")()
	if t.finished {
		metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
		return ErrTxFinished
	}
	defer t.release()

	for _, op := range t.ops {
		var payload []byte
		if t.store.legacy {
			payload = encodeOpLegacy(op)
		} else {
			var enErr error
			payload, enErr = encodeOpTyped(op, t.store.codec, t.store.wcodec)
			if enErr != nil {
				metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
				return enErr
			}
		}
		if err := t.store.wal.Append(payload); err != nil {
			metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
			return err
		}
	}
	if err := t.store.wal.Sync(); err != nil {
		metrics.IncCounter("store.txn.CommitWALOnly.errors", 1)
		return err
	}
	return nil
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

// encodeOpLegacy serialises one op to a v1 (untagged) WAL payload.
// Layout:
//
//	uint8  kind
//	uint16 srcLen
//	[srcLen]byte src   (fmt.Sprintf("%v") of op.Src)
//	uint16 dstLen
//	[dstLen]byte dst   (fmt.Sprintf("%v") of op.Dst)
//	uint16 labelLen
//	[labelLen]byte label
//
// Endpoints are serialised via fmt.Sprintf("%v") — sufficient for the
// v1 N types (string, integer) and the test fixtures. This function
// is preserved bit-for-bit so call sites using [NewStore] continue to
// produce WAL frames identical to the ones written prior to the typed
// codec introduction. Weighted ops cannot reach this encoder because
// [NewStore] never installs a [WeightCodec]; [Tx.AddEdge] refuses
// non-zero weights up-front with [ErrNoWeightCodec].
func encodeOpLegacy[N comparable, W any](op Op[N, W]) []byte {
	src := encodeAny(op.Src)
	dst := encodeAny(op.Dst)
	label := []byte(op.Label)
	buf := make([]byte, 1+2+len(src)+2+len(dst)+2+len(label))
	buf[0] = byte(op.Kind)
	off := 1
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(src)))
	off += 2
	copy(buf[off:], src)
	off += len(src)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(dst)))
	off += 2
	copy(buf[off:], dst)
	off += len(dst)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(label)))
	off += 2
	copy(buf[off:], label)
	return buf
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

	var err error
	switch op.Kind {
	case OpAddNode, OpRemoveNode, OpRemoveNodeLabel:
		var zero N
		if buf, err = codec.Encode(buf, op.Src); err != nil {
			return nil, err
		}
		if buf, err = codec.Encode(buf, zero); err != nil {
			return nil, err
		}
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
		buf = append(buf, op.Label...)

	case OpSetNodeProperty, OpDelNodeProperty:
		var zero N
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

	case OpRemoveEdge:
		if buf, err = codec.Encode(buf, op.Src); err != nil {
			return nil, err
		}
		if buf, err = codec.Encode(buf, op.Dst); err != nil {
			return nil, err
		}
		buf = binary.LittleEndian.AppendUint16(buf, 0)

	case OpSetEdgeProperty, OpDelEdgeProperty:
		if buf, err = codec.Encode(buf, op.Src); err != nil {
			return nil, err
		}
		if buf, err = codec.Encode(buf, op.Dst); err != nil {
			return nil, err
		}
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Key)))
		buf = append(buf, op.Key...)
		if op.Kind == OpSetEdgeProperty {
			buf = encodePropertyValue(buf, op.Value)
		}

	case OpAddEdgeWeighted:
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

	default: // OpAddEdge, OpSetNodeLabel, OpSetEdgeLabel
		if buf, err = codec.Encode(buf, op.Src); err != nil {
			return nil, err
		}
		if buf, err = codec.Encode(buf, op.Dst); err != nil {
			return nil, err
		}
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
		buf = append(buf, op.Label...)
	}
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
// [lpg.PropBool], [lpg.PropTime], [lpg.PropBytes]. For [lpg.PropString] and
// [lpg.PropBytes] the value is prefixed with a uint32 LE length. For
// [lpg.PropInt64] the value is a signed varint. For [lpg.PropFloat64] the value
// is a uint64 LE IEEE-754 bit pattern. For [lpg.PropBool] the value is a
// single byte (0 or 1). For [lpg.PropTime] the value is the UTC Unix nanoseconds
// as a signed varint.
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
	default:
		return lpg.PropertyValue{}, buf, errors.New("txn: unknown property kind")
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

func encodeAny[N comparable](v N) []byte {
	return []byte(goFormat(v))
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
	case OpSetNodeLabel:
		return g.SetNodeLabel(op.Src, op.Label)
	case OpSetEdgeLabel:
		g.SetEdgeLabel(op.Src, op.Dst, op.Label)
	case OpAddNode:
		return g.AddNode(op.Src)
	case OpRemoveNode:
		// Logical removal: mapper entry is permanent; remove all labels and
		// properties so the node is unreachable via label/property queries.
		for _, lbl := range g.NodeLabels(op.Src) {
			g.RemoveNodeLabel(op.Src, lbl)
		}
		for k := range g.NodeProperties(op.Src) {
			g.DelNodeProperty(op.Src, k)
		}
	case OpRemoveNodeLabel:
		g.RemoveNodeLabel(op.Src, op.Label)
	case OpSetNodeProperty:
		return g.SetNodeProperty(op.Src, op.Key, op.Value)
	case OpDelNodeProperty:
		g.DelNodeProperty(op.Src, op.Key)
	case OpRemoveEdge:
		g.AdjList().RemoveEdge(op.Src, op.Dst)
	case OpSetEdgeProperty:
		g.SetEdgeProperty(op.Src, op.Dst, op.Key, op.Value)
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
