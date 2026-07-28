package exec

// scan_index_btree.go — NodeByIndexRangeScan operator (task-239).
//
// NodeByIndexRangeScan performs a range scan on a B+tree index and emits
// NodeIDs in ascending order as guaranteed by the btree.Index implementation.
//
// # Interval semantics — inclusive superset (#F-EXEC1)
//
// The operator emits the INCLUSIVE [lo, hi] superset the index returns. It does
// NOT enforce exclusive (open) bounds itself, and cannot: it holds only a
// bitmap of NodeIDs, not the property values those NodeIDs carry, so it has no
// way to compare a node's value against the bound. Exact open/closed semantics
// are the caller's responsibility via a residual predicate Filter stacked on
// top — which the Cypher planner always applies (see cypher/range_seek_plan.go,
// where the original range predicate is retained as a Selection over the scan).
// Callers driving this operator directly must apply the same residual filter
// for exact bounds; the RangeBound.Include flags record the caller's intended
// inclusivity but are not enforced here.
//
// (A prior version post-filtered exclusive bounds by comparing the emitted
// NodeID to the property-value bound — meaningless, since the two are unrelated
// — which could drop a node whose ID happened to equal a numeric bound. Removed:
// the residual Filter is the only correct enforcement point.)
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
	// Include records the caller's intended inclusivity (≤ / ≥ vs < / >). It is
	// metadata only: NodeByIndexRangeScan always emits the inclusive [lo, hi]
	// superset and relies on a residual predicate Filter for exact open/closed
	// semantics (see the NodeByIndexRangeScan type doc, #F-EXEC1).
	Include bool
}

// NodeByIndexRangeScan is a Volcano leaf operator that scans a B+tree index
// over a half-open, closed, or open interval.  Each Row has a single column:
// expr.IntegerValue(nodeID).
//
// NodeByIndexRangeScan is NOT safe for concurrent use.
type NodeByIndexRangeScan struct {
	idx  rangeLookup
	ctx  context.Context //nolint:containedctx // stored for per-Next ctx check
	iter roaring64.IntPeekable64
	buf  [1]expr.Value // fixed backing buffer — zero-alloc per Next
	// extra holds the ADDITIONAL indexed conjuncts of a composed intersection
	// (#2134). Empty for the ordinary single-index range scan.
	extra []IndexRangePart
	lo    RangeBound
	hi    RangeBound
}

// IndexRangePart is one indexed conjunct's contribution to a composed
// intersection: the index to probe and the bounds to probe it over (#2134).
type IndexRangePart struct {
	Index rangeLookup
	Lo    RangeBound
	Hi    RangeBound
}

// NewNodeByIndexRangeScan creates a NodeByIndexRangeScan.
func NewNodeByIndexRangeScan(idx rangeLookup, lo, hi RangeBound) *NodeByIndexRangeScan {
	return &NodeByIndexRangeScan{idx: idx, lo: lo, hi: hi}
}

// NewNodeByIndexIntersectionScan composes SEVERAL single-property indexes into one
// access path by intersecting their range bitmaps (#2134).
//
// This is what lets `WHERE n.a > 1 AND n.b < 9` be answered from two ordinary
// single-property indexes — the answer Memgraph needs a dedicated COMPOSITE index
// type for. No new index type and no new statistic are involved: RangeBitmap
// already returns a Roaring bitmap, so the conjunction is a set operation.
//
// # Superset discipline (design §8)
//
// Unlike the label intersection, each part is only a SUPERSET of its conjunct's
// true matches — the operator emits the inclusive [lo, hi] interval and cannot
// enforce an open bound (see the type doc, #F-EXEC1). Intersection PRESERVES that:
// if Bᵃ ⊇ Aᵃ and Bᵇ ⊇ Aᵇ then Bᵃ ∩ Bᵇ ⊇ Aᵃ ∩ Aᵇ. So the composed scan is a sound
// superset — and the caller's residual Filter remains MANDATORY, exactly as it is
// for a single range scan.
//
// parts must hold at least one entry beyond the primary; the primary's own bounds
// are given by lo/hi. Parts are ANDed in the order supplied, which the planner
// orders by ascending exact cardinality so the cheapest bitmap is materialised
// first.
func NewNodeByIndexIntersectionScan(idx rangeLookup, lo, hi RangeBound, parts []IndexRangePart) *NodeByIndexRangeScan {
	return &NodeByIndexRangeScan{idx: idx, lo: lo, hi: hi, extra: parts}
}

// Init performs the range lookup — intersecting the additional indexed conjuncts
// when this is a composed scan (#2134) — and initialises the bitmap iterator.
func (op *NodeByIndexRangeScan) Init(ctx context.Context) error {
	op.ctx = ctx
	bm := op.idx.RangeBitmap(op.lo.Value, op.hi.Value)
	for i := range op.extra {
		if bm.IsEmpty() {
			// Early exit: an empty intermediate cannot grow, so the remaining
			// probes are pure waste. The same short-circuit label.Index.Intersect
			// applies to the label AND.
			break
		}
		other := op.extra[i].Index.RangeBitmap(op.extra[i].Lo.Value, op.extra[i].Hi.Value)
		bm.And(other)
	}
	op.iter = bm.Iterator()
	return nil
}

// Next emits the next NodeID in the inclusive [lo, hi] superset. Returns
// (false, nil) at end-of-stream. Exact open/closed enforcement is the caller's
// residual-filter responsibility (see the type doc); this operator emits every
// NodeID the index's range bitmap contains.
func (op *NodeByIndexRangeScan) Next(out *Row) (bool, error) {
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
// An unbounded lower bound is "" (the true minimum of the string order); an
// unbounded (or non-string) upper bound routes to the index's open-ended
// RangeFrom scan rather than a fixed sentinel — no fixed key is a true maximum
// for a variable-length string, so a sentinel cap would silently drop any key
// sorting above it (#F-CY1).
type StringRangeIndex struct {
	idx interface {
		Range(lo, hi string) *roaring64.Bitmap
		RangeFrom(lo string) *roaring64.Bitmap
	}
}

// NewStringRangeIndex constructs a StringRangeIndex.
func NewStringRangeIndex(idx interface {
	Range(lo, hi string) *roaring64.Bitmap
	RangeFrom(lo string) *roaring64.Bitmap
}) *StringRangeIndex {
	return &StringRangeIndex{idx: idx}
}

// RangeBitmap implements [rangeLookup]. An unbounded-above range (nil/NULL or a
// non-string upper bound) scans open-ended via RangeFrom so the bitmap is a
// genuine superset of every key >= lo (#F-CY1); a bounded upper uses the
// inclusive [lo, hi] Range.
func (r *StringRangeIndex) RangeBitmap(lo, hi expr.Value) *roaring64.Bitmap {
	var loVal string
	if lo != nil && !expr.IsNull(lo) {
		if sv, ok := lo.(expr.StringValue); ok {
			loVal = string(sv)
		}
	}
	if hi == nil || expr.IsNull(hi) {
		return r.idx.RangeFrom(loVal)
	}
	sv, ok := hi.(expr.StringValue)
	if !ok {
		return r.idx.RangeFrom(loVal)
	}
	return r.idx.Range(loVal, string(sv))
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
