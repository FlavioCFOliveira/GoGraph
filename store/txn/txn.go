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

// Store bundles an [lpg.Graph] with a [wal.Writer] and the single-
// writer lock that serialises transactions.
//
// Concurrency: any number of goroutines may call Begin/BeginCtx;
// transactions serialise on the store mutex, so only one Tx is
// active at any moment. Reads on the underlying lpg.Graph remain
// concurrent and lock-free per the lpg/adjlist contracts.
type Store[N comparable, W any] struct {
	mu  sync.Mutex
	g   *lpg.Graph[N, W]
	wal *wal.Writer
}

// NewStore returns a Store wrapping g and wal.
func NewStore[N comparable, W any](g *lpg.Graph[N, W], wlog *wal.Writer) *Store[N, W] {
	return &Store[N, W]{g: g, wal: wlog}
}

// Graph returns the underlying graph.
func (s *Store[N, W]) Graph() *lpg.Graph[N, W] { return s.g }

// Begin opens a new transaction. The returned Tx holds the
// store's single-writer mutex until Commit or Rollback runs.
func (s *Store[N, W]) Begin() *Tx[N, W] {
	tx, _ := s.BeginCtx(context.Background())
	return tx
}

// BeginCtx is the context-aware variant of [Store.Begin]. ctx.Err()
// is checked before acquiring the store mutex; on cancellation returns
// (nil, wrapped ctx.Err). Once the lock is held the transaction
// proceeds; further ctx checks happen at the caller's discretion.
func (s *Store[N, W]) BeginCtx(ctx context.Context) (*Tx[N, W], error) {
	if err := ctx.Err(); err != nil {
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
	if t.finished {
		return ErrTxFinished
	}
	defer t.release()

	// Encode every op as a separate WAL frame so a torn write at
	// any point in the batch only loses tail ops, never partial
	// ones.
	for _, op := range t.ops {
		payload := encodeOp(op)
		if err := t.store.wal.Append(payload); err != nil {
			return err
		}
	}
	if err := t.store.wal.Sync(); err != nil {
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
	if t.finished {
		return ErrTxFinished
	}
	t.release()
	return nil
}

func (t *Tx[N, W]) release() {
	t.finished = true
	t.store.mu.Unlock()
}

// encodeOp serialises one op to a WAL payload. Layout:
//
//	uint8  kind
//	uint16 srcLen
//	[srcLen]byte src
//	uint16 dstLen
//	[dstLen]byte dst
//	uint16 labelLen
//	[labelLen]byte label
//
// Endpoints are serialised via fmt.Sprintf("%v") — sufficient for
// the v1 N types (string, integer) and the test fixtures. A future
// iteration will plug in a typed codec for N.
func encodeOp[N comparable](op Op[N]) []byte {
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

func encodeAny[N comparable](v N) []byte {
	return []byte(stringify(v))
}

// stringify renders a value as a string for WAL payload encoding.
// It exists as a separate function to centralise the encoding rule.
func stringify[N comparable](v N) string {
	return goFormat(v)
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
