package exec

// scan_index_btree.go — NodeByIndexRangeScan operator (task-239).
//
// NodeByIndexRangeScan performs a range scan on a B+tree index and emits
// NodeIDs in ascending order as guaranteed by the btree.Index implementation.
//
// # Interval semantics
//
// The operator supports closed, half-open (either end), and open intervals
// via the IncludeLo / IncludeHi flags.  The btree index's Range method is
// fully inclusive ([lo, hi]), so the operator post-filters the boundary
// NodeIDs when exclusive bounds are requested.
//
// # Zero-alloc contract
//
// The bitmap is collected once in Init; Next advances the IntPeekable64
// iterator without further allocation.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call.

import (
	"context"
	"math"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// rangeLookup is the minimal interface that NodeByIndexRangeScan requires.
// Implementations wrap btree.Index[V].Range.
type rangeLookup interface {
	// RangeBitmap returns the bitmap of NodeIDs whose property value falls
	// within the given inclusive bounds [lo, hi].
	RangeBitmap(lo, hi expr.Value) *roaring64.Bitmap
}

// RangeBound carries one endpoint of a range predicate.
type RangeBound struct {
	// Value is the bound's expr.Value.  Nil means unbounded (use the
	// minimum or maximum representable value for the index type).
	Value expr.Value
	// Include determines whether the bound is inclusive (≤ / ≥) or exclusive
	// (< / >).
	Include bool
}

// NodeByIndexRangeScan is a Volcano leaf operator that scans a B+tree index
// over a half-open, closed, or open interval.  Each Row has a single column:
// expr.IntegerValue(nodeID).
//
// NodeByIndexRangeScan is NOT safe for concurrent use.
type NodeByIndexRangeScan struct {
	idx  rangeLookup
	lo   RangeBound
	hi   RangeBound
	ctx  context.Context //nolint:containedctx // stored for per-Next ctx check
	iter roaring64.IntPeekable64
	buf  [1]expr.Value // fixed backing buffer — zero-alloc per Next
}

// NewNodeByIndexRangeScan creates a NodeByIndexRangeScan.
func NewNodeByIndexRangeScan(idx rangeLookup, lo, hi RangeBound) *NodeByIndexRangeScan {
	return &NodeByIndexRangeScan{idx: idx, lo: lo, hi: hi}
}

// Init performs the range lookup and initialises the bitmap iterator.
func (op *NodeByIndexRangeScan) Init(ctx context.Context) error {
	op.ctx = ctx
	bm := op.idx.RangeBitmap(op.lo.Value, op.hi.Value)
	op.iter = bm.Iterator()
	return nil
}

// Next emits the next matching NodeID.  Returns (false, nil) at end-of-stream.
func (op *NodeByIndexRangeScan) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	for op.iter.HasNext() {
		raw := op.iter.Next()
		nodeID := expr.IntegerValue(int64(raw))

		// Enforce exclusive lower bound.
		if op.lo.Value != nil && !op.lo.Include {
			if expr.IsTruthy(nodeID.Equal(op.lo.Value)) {
				continue
			}
		}
		// Enforce exclusive upper bound.
		if op.hi.Value != nil && !op.hi.Include {
			if expr.IsTruthy(nodeID.Equal(op.hi.Value)) {
				continue
			}
		}

		op.buf[0] = nodeID
		*out = op.buf[:]
		return true, nil
	}
	return false, nil
}

// Close releases resources.
func (op *NodeByIndexRangeScan) Close() error {
	op.iter = nil
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Int64RangeIndex — production adapter over btree.Index[int64]
// ─────────────────────────────────────────────────────────────────────────────

// Int64RangeIndex adapts btree.Index[int64] to the [rangeLookup] interface.
// Nil bounds are treated as ±∞ using math.MinInt64 / math.MaxInt64.
type Int64RangeIndex struct {
	idx interface {
		Range(lo, hi int64) *roaring64.Bitmap
	}
}

// NewInt64RangeIndex constructs an Int64RangeIndex.
func NewInt64RangeIndex(idx interface {
	Range(lo, hi int64) *roaring64.Bitmap
}) *Int64RangeIndex {
	return &Int64RangeIndex{idx: idx}
}

// RangeBitmap implements [rangeLookup].
func (r *Int64RangeIndex) RangeBitmap(lo, hi expr.Value) *roaring64.Bitmap {
	var loVal, hiVal int64
	const minInt64 = int64(-1 << 63)
	const maxInt64 = int64(1<<63 - 1)

	if lo == nil || expr.IsNull(lo) {
		loVal = minInt64
	} else if iv, ok := lo.(expr.IntegerValue); ok {
		loVal = int64(iv)
	} else {
		loVal = minInt64
	}
	if hi == nil || expr.IsNull(hi) {
		hiVal = maxInt64
	} else if iv, ok := hi.(expr.IntegerValue); ok {
		hiVal = int64(iv)
	} else {
		hiVal = maxInt64
	}
	return r.idx.Range(loVal, hiVal)
}

// StringRangeIndex adapts btree.Index[string] to the [rangeLookup] interface.
// Nil bounds are treated as "" (empty) / "\xff…" (all bytes 0xff, 256 chars).
type StringRangeIndex struct {
	idx interface {
		Range(lo, hi string) *roaring64.Bitmap
	}
}

// NewStringRangeIndex constructs a StringRangeIndex.
func NewStringRangeIndex(idx interface {
	Range(lo, hi string) *roaring64.Bitmap
}) *StringRangeIndex {
	return &StringRangeIndex{idx: idx}
}

// RangeBitmap implements [rangeLookup].
func (r *StringRangeIndex) RangeBitmap(lo, hi expr.Value) *roaring64.Bitmap {
	const maxStr = "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
		"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"

	var loVal, hiVal string
	if lo == nil || expr.IsNull(lo) {
		loVal = ""
	} else if sv, ok := lo.(expr.StringValue); ok {
		loVal = string(sv)
	}
	if hi == nil || expr.IsNull(hi) {
		hiVal = maxStr
	} else if sv, ok := hi.(expr.StringValue); ok {
		hiVal = string(sv)
	} else {
		hiVal = maxStr
	}
	return r.idx.Range(loVal, hiVal)
}

// Float64RangeIndex adapts btree.Index[float64] to the [rangeLookup] interface
// for the UNIFIED numeric range seek (#1652). It backs a CREATE INDEX (btree)
// companion that indexes BOTH integer- and float-valued nodes under one
// float64 total order (openCypher orders integers and floats in a single
// numeric order), so a numeric range bound is a SUPERSET-complete probe over
// every numeric node — never a non-superset the way an int64-only index would
// be (it would silently drop the float-valued matches).
//
// Both an integer and a float bound are coerced to float64: a Cypher
// IntegerValue bound and a FloatValue bound address the same numeric key
// space. The float64 widening of a large int64 bound can lose precision and
// make the probe over-return at the boundary, but the engine ALWAYS retains
// the original AST predicate as a residual Filter on top of the range scan, so
// any false positive is removed and the result is identical to a label
// scan+filter (cypher-expert-consultant, #1652). Nil / non-numeric bounds are
// treated as ±∞ so an unbounded side returns the whole numeric population.
//
// NaN is never a bound here (the extractor only admits finite numeric
// literals/parameters) and is never indexed (projectNumericPropValue excludes
// it), so a NaN node can neither be a key nor be returned: the btree's total
// order places NaN below every real value and Range with a non-NaN lower bound
// never returns it, and the residual Filter is the final backstop.
type Float64RangeIndex struct {
	idx interface {
		Range(lo, hi float64) *roaring64.Bitmap
	}
}

// NewFloat64RangeIndex constructs a Float64RangeIndex.
func NewFloat64RangeIndex(idx interface {
	Range(lo, hi float64) *roaring64.Bitmap
}) *Float64RangeIndex {
	return &Float64RangeIndex{idx: idx}
}

// RangeBitmap implements [rangeLookup]. A nil or NULL bound, or one that is
// neither an integer nor a float, is treated as the corresponding numeric
// infinity so an unbounded (or undecodable) side spans the whole numeric range
// — the residual Filter then enforces the exact predicate.
func (r *Float64RangeIndex) RangeBitmap(lo, hi expr.Value) *roaring64.Bitmap {
	loVal := numericBound(lo, math.Inf(-1))
	hiVal := numericBound(hi, math.Inf(1))
	return r.idx.Range(loVal, hiVal)
}

// numericBound coerces a range-bound expr.Value to float64, returning fallback
// (∓∞) when the bound is nil, NULL, or not a numeric value. An IntegerValue and
// a FloatValue map onto the same float64 numeric order.
func numericBound(v expr.Value, fallback float64) float64 {
	if v == nil || expr.IsNull(v) {
		return fallback
	}
	switch n := v.(type) {
	case expr.IntegerValue:
		return float64(n)
	case expr.FloatValue:
		return float64(n)
	default:
		return fallback
	}
}
