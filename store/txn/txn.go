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
package txn

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"

	"gograph/graph/lpg"
	"gograph/internal/metrics"
	"gograph/store/wal"
)

// ErrTxFinished is returned by operations on a transaction that has
// already been committed or rolled back.
var ErrTxFinished = errors.New("txn: transaction already finished")

// OpKind enumerates the mutation kinds supported by a transaction.
type OpKind uint8

// Mutation kinds supported by a transaction.
const (
	OpAddEdge OpKind = iota + 1
	OpSetNodeLabel
	OpSetEdgeLabel
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
	legacy bool
}

// NewStore returns a Store wrapping g and wal. The store emits v1
// (untagged, fmt.Sprintf-based) WAL payloads so that callers that
// existed prior to the typed codec introduction observe byte-identical
// on-disk frames.
//
// New code should prefer [NewStoreWithCodec], which installs a typed
// [Codec] and emits v2 (tagged) frames that survive arbitrary N
// types — strings with embedded length bytes, composite identifiers,
// unicode boundaries, and so on.
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
func NewStoreWithCodec[N comparable, W any](g *lpg.Graph[N, W], wlog *wal.Writer, codec Codec[N]) *Store[N, W] {
	defer metrics.Time("store.txn.NewStoreWithCodec")()
	return &Store[N, W]{
		g:      g,
		wal:    wlog,
		codec:  codec,
		legacy: isLegacyCodec[N](codec),
	}
}

// Codec returns the [Codec] installed on the Store. The returned value
// is the same one passed to [NewStoreWithCodec], or the internal legacy
// codec installed by [NewStore]. Callers should treat the return as
// read-only.
func (s *Store[N, W]) Codec() Codec[N] { return s.codec }

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
type Op[N comparable] struct {
	Kind     OpKind
	Src, Dst N
	Label    string
}

// Tx is an in-progress transaction.
type Tx[N comparable, W any] struct {
	store    *Store[N, W]
	ops      []Op[N]
	finished bool
}

// AddEdge buffers an AddEdge(src, dst, _) operation on the graph.
// The edge weight uses the zero value of W — full weighted-edge
// transaction support is scheduled for a future iteration.
func (t *Tx[N, W]) AddEdge(src, dst N) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N]{Kind: OpAddEdge, Src: src, Dst: dst})
	return nil
}

// SetNodeLabel buffers a SetNodeLabel(node, label) operation.
func (t *Tx[N, W]) SetNodeLabel(node N, label string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N]{Kind: OpSetNodeLabel, Src: node, Label: label})
	return nil
}

// SetEdgeLabel buffers a SetEdgeLabel(src, dst, label) operation.
// The underlying edge must exist at apply time; otherwise the
// underlying SetEdgeLabel call is a documented no-op.
func (t *Tx[N, W]) SetEdgeLabel(src, dst N, label string) error {
	if t.finished {
		return ErrTxFinished
	}
	t.ops = append(t.ops, Op[N]{Kind: OpSetEdgeLabel, Src: src, Dst: dst, Label: label})
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
			payload = encodeOpTyped(op, t.store.codec)
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
		applyOp(t.store.g, op)
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
// codec introduction.
func encodeOpLegacy[N comparable](op Op[N]) []byte {
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
// the supplied codec. Layout:
//
//	uint8  version   (always [OpRecordV2])
//	uint8  kind
//	codec  src       (codec-encoded, self-delimiting)
//	codec  dst       (codec-encoded, self-delimiting)
//	uint16 labelLen
//	[labelLen]byte label
//
// The codec is responsible for the framing of src and dst, so the
// payload has no per-field length prefix at this level. The label
// trailer is identical to the v1 trailer.
func encodeOpTyped[N comparable](op Op[N], codec Codec[N]) []byte {
	// Allocate with a conservative head room: header + label trailer
	// plus a few bytes per endpoint. The codec may extend beyond this
	// estimate; append handles the regrowth.
	const headroom = 2 + 2 // version + kind + uint16 labelLen
	buf := make([]byte, 0, headroom+len(op.Label)+32)
	buf = append(buf, OpRecordV2, byte(op.Kind))
	buf = codec.Encode(buf, op.Src)
	buf = codec.Encode(buf, op.Dst)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(op.Label)))
	return append(buf, op.Label...)
}

func encodeAny[N comparable](v N) []byte {
	return []byte(goFormat(v))
}

func applyOp[N comparable, W any](g *lpg.Graph[N, W], op Op[N]) {
	switch op.Kind {
	case OpAddEdge:
		var zero W
		g.AddEdge(op.Src, op.Dst, zero)
	case OpSetNodeLabel:
		g.SetNodeLabel(op.Src, op.Label)
	case OpSetEdgeLabel:
		g.SetEdgeLabel(op.Src, op.Dst, op.Label)
	}
}
