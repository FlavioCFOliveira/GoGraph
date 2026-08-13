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

// HashLookup is the minimal interface that NodeByIndexSeek requires.
// hash.Index[V] satisfies it for every supported V.
//
// Exported so the engine's build path can name it when it chooses between the
// guarded and unguarded seek forms in one place (rmp #2423).
type HashLookup interface {
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
	idx  HashLookup
	seek expr.Value
	// admit, when non-nil, is consulted for every candidate the index returns and
	// drops the ones it rejects.
	//
	// # Why a seek needs a residual predicate at all (rmp #2423)
	//
	// This operator replaces a LABELLED scan: the planner rewrites
	// Selection(n.prop = v) over NodeByLabelScan(L) into a bare seek on the index
	// covering (L, prop). That is sound only if membership in the index implies the
	// label, and it does NOT: an index is a CANDIDATE source that over-reports,
	// because removing a label leaves the node's entries in that label's property
	// indexes behind. Without a residual check the engine returned a node for the
	// pattern (n:Person) whose labels(n) was the EMPTY LIST — one row contradicting
	// itself.
	//
	// The label scan has never had this problem: it resolves through
	// LabelBitmapAsOf, which filters the over-reporting bitmap against the reader's
	// snapshot. This predicate is how the seek path gets the same guarantee, and the
	// planner DECLINES the rewrite altogether when it cannot supply one — an
	// unverifiable rewrite must not become a silent wrong answer.
	admit func(nodeID uint64) bool
	ctx   context.Context //nolint:containedctx // stored for per-Next ctx check
	buf   [1]expr.Value   // fixed backing buffer — zero-alloc per Next
	ids   []uint64        // matching NodeIDs, drained once at Init
	idbuf [8]uint64       // inline backing for ids — singleton/small seeks stay zero-alloc
	pos   int             // cursor into ids
}

// NewNodeByIndexSeek creates a NodeByIndexSeek that looks up seekValue in idx.
//
// It carries NO residual predicate, so it is correct only where the index's
// candidates need no further qualification. A rewrite that dropped a label must use
// [NewNodeByIndexSeekAdmitting]; see [NodeByIndexSeek.admit].
func NewNodeByIndexSeek(idx HashLookup, seekValue expr.Value) *NodeByIndexSeek {
	return &NodeByIndexSeek{idx: idx, seek: seekValue}
}

// NewNodeByIndexSeekAdmitting is [NewNodeByIndexSeek] with a residual predicate
// applied to every candidate the index returns — the label check a rewrite over a
// labelled scan leaf owes (rmp #2423).
//
// admit must be non-nil; a caller with nothing to verify uses [NewNodeByIndexSeek]
// so the distinction stays visible at the call site rather than hiding behind a nil.
func NewNodeByIndexSeekAdmitting(idx HashLookup, seekValue expr.Value, admit func(nodeID uint64) bool) *NodeByIndexSeek {
	return &NodeByIndexSeek{idx: idx, seek: seekValue, admit: admit}
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
	// The residual predicate is applied HERE, in place, rather than per Next: the
	// filter runs once per candidate either way, and doing it at Init keeps Next's
	// zero-allocation single-branch shape and lets rowCountHint-style consumers see the
	// true count. Compacting in place reuses the same backing array, so a guarded
	// seek allocates exactly what an unguarded one does.
	if op.admit != nil {
		kept := ids[:0]
		for _, id := range ids {
			if op.admit(id) {
				kept = append(kept, id)
			}
		}
		ids = kept
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

// StringHashIndex adapts hash.Index[string] to the [HashLookup] interface.
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

// LookupAppend implements [HashLookup].
func (h *StringHashIndex) LookupAppend(value expr.Value, dst []uint64) ([]uint64, error) {
	sv, ok := value.(expr.StringValue)
	if !ok {
		return nil, ErrIndexTypeMismatch
	}
	return h.idx.LookupAppend(string(sv), dst), nil
}

// Int64HashIndex adapts hash.Index[int64] to the [HashLookup] interface.
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

// LookupAppend implements [HashLookup].
func (h *Int64HashIndex) LookupAppend(value expr.Value, dst []uint64) ([]uint64, error) {
	iv, ok := value.(expr.IntegerValue)
	if !ok {
		return nil, ErrIndexTypeMismatch
	}
	return h.idx.LookupAppend(int64(iv), dst), nil
}
