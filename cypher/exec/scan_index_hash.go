package exec

// scan_index_hash.go — NodeByIndexSeek operator (task-238).
//
// NodeByIndexSeek performs an exact-match lookup on a hash index.  The
// lookup key is an expr.Value; the operator converts it to the concrete
// Go type expected by the index at Init time so that each Next call is
// allocation-free.
//
// # Type mismatch
//
// When the provided expr.Value cannot be adapted to the index's key type the
// operator returns ErrIndexTypeMismatch.
//
// # Zero-alloc contract
//
// The Roaring bitmap iterator is advanced one step per Next call with no
// additional allocations after Init.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call.

import (
	"context"
	"errors"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ErrIndexTypeMismatch is returned by NodeByIndexSeek.Init when the seek
// value's Kind is incompatible with the index's key type.
var ErrIndexTypeMismatch = errors.New("exec: index type mismatch")

// hashLookup is the minimal interface that NodeByIndexSeek requires.
// hash.Index[V] satisfies it for every supported V.
type hashLookup interface {
	// LookupBitmap returns the Roaring bitmap of NodeIDs matching the seek
	// value, or an empty bitmap when no match exists.  Returns
	// ErrIndexTypeMismatch when the value kind is incompatible.
	LookupBitmap(value expr.Value) (*roaring64.Bitmap, error)
}

// NodeByIndexSeek is a Volcano leaf operator that performs an equality lookup
// on a property hash index.  Each Row has a single column:
// expr.IntegerValue(nodeID).
//
// NodeByIndexSeek is NOT safe for concurrent use.
type NodeByIndexSeek struct {
	idx  hashLookup
	seek expr.Value
	ctx  context.Context //nolint:containedctx // stored for per-Next ctx check
	iter roaring64.IntPeekable64
	buf  [1]expr.Value // fixed backing buffer — zero-alloc per Next
}

// NewNodeByIndexSeek creates a NodeByIndexSeek that looks up seekValue in idx.
func NewNodeByIndexSeek(idx hashLookup, seekValue expr.Value) *NodeByIndexSeek {
	return &NodeByIndexSeek{idx: idx, seek: seekValue}
}

// Init performs the index lookup and initialises the bitmap iterator.
func (op *NodeByIndexSeek) Init(ctx context.Context) error {
	op.ctx = ctx
	bm, err := op.idx.LookupBitmap(op.seek)
	if err != nil {
		return err
	}
	op.iter = bm.Iterator()
	return nil
}

// Next emits the next matching NodeID.  Returns (false, nil) at end-of-stream.
func (op *NodeByIndexSeek) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if !op.iter.HasNext() {
		return false, nil
	}
	op.buf[0] = expr.IntegerValue(int64(op.iter.Next()))
	*out = op.buf[:]
	return true, nil
}

// Close releases resources.
func (op *NodeByIndexSeek) Close() error {
	op.iter = nil
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// StringHashIndex — production adapter over hash.Index[string]
// ─────────────────────────────────────────────────────────────────────────────

// StringHashIndex adapts hash.Index[string] to the [hashLookup] interface.
// It accepts only [expr.StringValue] seek keys; other kinds return
// [ErrIndexTypeMismatch].
type StringHashIndex struct {
	idx interface {
		Lookup(value string) *roaring64.Bitmap
	}
}

// NewStringHashIndex constructs a StringHashIndex.
func NewStringHashIndex(idx interface {
	Lookup(value string) *roaring64.Bitmap
}) *StringHashIndex {
	return &StringHashIndex{idx: idx}
}

// LookupBitmap implements [hashLookup].
func (h *StringHashIndex) LookupBitmap(value expr.Value) (*roaring64.Bitmap, error) {
	sv, ok := value.(expr.StringValue)
	if !ok {
		return nil, ErrIndexTypeMismatch
	}
	return h.idx.Lookup(string(sv)), nil
}

// Int64HashIndex adapts hash.Index[int64] to the [hashLookup] interface.
// It accepts only [expr.IntegerValue] seek keys.
type Int64HashIndex struct {
	idx interface {
		Lookup(value int64) *roaring64.Bitmap
	}
}

// NewInt64HashIndex constructs an Int64HashIndex.
func NewInt64HashIndex(idx interface {
	Lookup(value int64) *roaring64.Bitmap
}) *Int64HashIndex {
	return &Int64HashIndex{idx: idx}
}

// LookupBitmap implements [hashLookup].
func (h *Int64HashIndex) LookupBitmap(value expr.Value) (*roaring64.Bitmap, error) {
	iv, ok := value.(expr.IntegerValue)
	if !ok {
		return nil, ErrIndexTypeMismatch
	}
	return h.idx.Lookup(int64(iv)), nil
}
