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
// Init drains the matching NodeIDs into a reused inline buffer (no bitmap, no
// iterator); each Next emits one id from it with no additional allocations.
// The dominant singleton/small posting list fits the inline buffer, so a seek
// allocates nothing on the steady-state path.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call.

import (
	"context"
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ErrIndexTypeMismatch is returned by NodeByIndexSeek.Init when the seek
// value's Kind is incompatible with the index's key type.
var ErrIndexTypeMismatch = errors.New("exec: index type mismatch")

// hashLookup is the minimal interface that NodeByIndexSeek requires.
// hash.Index[V] satisfies it for every supported V.
type hashLookup interface {
	// LookupAppend appends the NodeIDs matching the seek value to dst in
	// ascending order and returns the extended slice, draining the index's
	// posting list under its read lock without materialising a bitmap.
	// Returns ErrIndexTypeMismatch when the value kind is incompatible.
	LookupAppend(value expr.Value, dst []uint64) ([]uint64, error)
}

// NodeByIndexSeek is a Volcano leaf operator that performs an equality lookup
// on a property hash index.  Each Row has a single column:
// expr.IntegerValue(nodeID).
//
// NodeByIndexSeek is NOT safe for concurrent use.
type NodeByIndexSeek struct {
	idx   hashLookup
	seek  expr.Value
	ctx   context.Context //nolint:containedctx // stored for per-Next ctx check
	ids   []uint64        // matching NodeIDs, drained once at Init
	pos   int             // cursor into ids
	idbuf [8]uint64       // inline backing for ids — singleton/small seeks stay zero-alloc
	buf   [1]expr.Value   // fixed backing buffer — zero-alloc per Next
}

// NewNodeByIndexSeek creates a NodeByIndexSeek that looks up seekValue in idx.
func NewNodeByIndexSeek(idx hashLookup, seekValue expr.Value) *NodeByIndexSeek {
	return &NodeByIndexSeek{idx: idx, seek: seekValue}
}

// Init performs the index lookup, draining the matching NodeIDs into the
// operator's reused buffer. The dominant singleton/small posting list fits the
// inline idbuf, so a seek allocates nothing after the buffer is established.
func (op *NodeByIndexSeek) Init(ctx context.Context) error {
	op.ctx = ctx
	ids, err := op.idx.LookupAppend(op.seek, op.idbuf[:0])
	if err != nil {
		return err
	}
	op.ids = ids
	op.pos = 0
	return nil
}

// Next emits the next matching NodeID.  Returns (false, nil) at end-of-stream.
func (op *NodeByIndexSeek) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.pos >= len(op.ids) {
		return false, nil
	}
	op.buf[0] = expr.IntegerValue(int64(op.ids[op.pos]))
	op.pos++
	*out = op.buf[:]
	return true, nil
}

// Close releases resources.
func (op *NodeByIndexSeek) Close() error {
	op.ids = nil
	op.pos = 0
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
		LookupAppend(value string, dst []uint64) []uint64
	}
}

// NewStringHashIndex constructs a StringHashIndex.
func NewStringHashIndex(idx interface {
	LookupAppend(value string, dst []uint64) []uint64
}) *StringHashIndex {
	return &StringHashIndex{idx: idx}
}

// LookupAppend implements [hashLookup].
func (h *StringHashIndex) LookupAppend(value expr.Value, dst []uint64) ([]uint64, error) {
	sv, ok := value.(expr.StringValue)
	if !ok {
		return nil, ErrIndexTypeMismatch
	}
	return h.idx.LookupAppend(string(sv), dst), nil
}

// Int64HashIndex adapts hash.Index[int64] to the [hashLookup] interface.
// It accepts only [expr.IntegerValue] seek keys.
type Int64HashIndex struct {
	idx interface {
		LookupAppend(value int64, dst []uint64) []uint64
	}
}

// NewInt64HashIndex constructs an Int64HashIndex.
func NewInt64HashIndex(idx interface {
	LookupAppend(value int64, dst []uint64) []uint64
}) *Int64HashIndex {
	return &Int64HashIndex{idx: idx}
}

// LookupAppend implements [hashLookup].
func (h *Int64HashIndex) LookupAppend(value expr.Value, dst []uint64) ([]uint64, error) {
	iv, ok := value.(expr.IntegerValue)
	if !ok {
		return nil, ErrIndexTypeMismatch
	}
	return h.idx.LookupAppend(int64(iv), dst), nil
}
