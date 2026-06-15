package cypher

// range_seek_plan.go — range-predicate B+tree index seek (#1505).
//
// When a MATCH has a range predicate on a property backed by a BOUND string
// btree index, and the in-range cardinality is a provable, selective win, the
// planner replaces the NodeByLabelScan child of the Selection with a
// NodeByIndexRangeScan. The ORIGINAL Selection predicate Filter then wraps the
// range scan unchanged (seek-superset + residual refilter), which makes the
// substitution unconditionally result-identical with the full scan+filter for
// every null / NaN / cross-type / open-vs-closed-bound case.
//
// # Safety (cypher-expert-consultant, openCypher 9 §3.4, CIP2016-06-14)
//
// A btree range seek is result-identical to NodeByLabelScan+Filter only when
// it returns a SUPERSET of the true matches (the residual Filter then refines
// it). The decisive hazard is comparability-vs-orderability: openCypher `<`/`>`
// across different type groups yields null (the row is dropped by WHERE), while
// a btree is laid out by a total order. The guard that makes the seek a
// provable superset here:
//
//   - The index is a TYPED string btree (the only btree a Cypher CREATE INDEX
//     can build, and now bound+backfilled — see index_binding.go). Strings are
//     comparable only to strings, and every string-valued node for the
//     property is in the index by construction, so a string index + string
//     bound is SUPERSET-COMPLETE with no extra proof. (Integer/float btrees are
//     NOT created by Cypher; were they, the int-vs-float comparability crossing
//     would make an int64 seek a non-superset — deliberately out of scope.)
//   - The bound operand is a plain string literal/param (Kind == KindString).
//     A non-string bound is declined (the scan+filter path yields the correct
//     null/empty result a typed index cannot express).
//   - null / missing properties are never indexed (projectStringPropValue), so
//     they are excluded exactly as the filter excludes them.
//   - The residual Filter (the full original predicate) is ALWAYS retained, so
//     even if the seek over-returns it cannot change the result.
//
// # No-regression (graph-theory-expert)
//
// The seek fires only when the EXACT in-range count R (summed from the sorted
// index, with an early-exit budget) satisfies S = R/N_label ≤ rangeSeekMaxSelectivity
// AND N_label ≥ rangeSeekMinLabelPopulation. The count is exact (not a
// fallback estimate), so the trustworthiness veto is satisfied trivially. A
// non-selective or small-population range keeps today's NodeByLabelScan+Filter.

import (
	"strings"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	// rangeSeekMaxSelectivity is the maximum fraction of the label population
	// the range may match for the seek to fire. The in-memory random-vs-
	// sequential break-even is ~10–30%; firing at the conservative floor of
	// that band (with an EXACT count, so no estimation margin is needed) means
	// the seek either wins or roughly ties — never regresses (graph-theory-
	// expert, #1505).
	rangeSeekMaxSelectivity = 0.10

	// rangeSeekMinLabelPopulation is the minimum label population below which
	// the engine always scans: a sub-1024-node label scan is a few microseconds
	// on a warm cache and the index-descent + bitmap overhead cannot beat it.
	rangeSeekMinLabelPopulation = 1024
)

// boundStringRange is satisfied by a bound string btree index: it exposes the
// range query (Range), the exact early-exit cardinality (RangeCount), and the
// (label, property) coverage (BoundNode). An UNBOUND btree does not satisfy
// BoundNode with ok==true and is therefore never selected — which is correct,
// because an unbound btree is not maintained from the change fan-out and could
// be stale/empty.
type boundStringRange interface {
	Range(lo, hi string) *roaring64.Bitmap
	RangeCount(lo, hi string, budget uint64) (uint64, bool)
	BoundNode() (label, property string, ok bool)
}

// stringRangePred is the extracted range predicate on a single node property:
// the bounds and their inclusivity. An absent bound (nil) is unbounded on that
// side.
type stringRangePred struct {
	propKey string
	lo      *exec.RangeBound // nil = unbounded below
	hi      *exec.RangeBound // nil = unbounded above
}

// buildRangeSeekIfEnabled is the gated entry point: it returns no range seek
// when the optimisation is disabled (EngineOptions.DisableRangeIndexSeek, or
// any build path that does not set bopts.rangeSeekEnabled, such as the write
// path or the public BuildPlanWithMutator). When enabled it delegates to
// [tryBuildRangeSeekChild].
func buildRangeSeekIfEnabled(
	bopts *buildOpts,
	sel *ir.Selection,
	schema map[string]int,
	idxMgr *index.Manager,
	g *lpg.Graph[string, float64],
) (exec.Operator, bool) {
	if bopts == nil || !bopts.rangeSeekEnabled {
		return nil, false
	}
	return tryBuildRangeSeekChild(sel, schema, idxMgr, g)
}

// tryBuildRangeSeekChild attempts to build a NodeByIndexRangeScan to replace
// the Selection's NodeByLabelScan child. ok is false (and the caller builds the
// normal scan child) when any guard is unmet: no AST predicate to refilter, the
// child is not a label scan, the predicate is not a single-property string
// range, no covering bound string btree exists, or the range is not a selective
// win.
//
// On success the returned operator emits one column (the node) bound to the
// scan's node variable at the next free schema slot — identical to what
// NodeByLabelScan would bind — so the original predicate Filter the caller
// stacks on top reads the node from the same column.
func tryBuildRangeSeekChild(
	sel *ir.Selection,
	schema map[string]int,
	idxMgr *index.Manager,
	g *lpg.Graph[string, float64],
) (exec.Operator, bool) {
	if idxMgr == nil || g == nil || sel.PredicateExpr == nil {
		// No index, or no AST predicate to build the residual Filter from:
		// without the residual Filter a seek-superset would leak extra rows.
		return nil, false
	}
	// The child must be a NodeByLabelScan: a labelled population is what the
	// selectivity gate (R / N_label) is defined against, and the label gives
	// the index its (label, property) coverage match.
	lblScan, ok := sel.Child.(*ir.NodeByLabelScan)
	if !ok || lblScan.Label == "" {
		return nil, false
	}
	nodeVar := lblScan.NodeVar

	pred, ok := extractStringRangePred(sel.PredicateExpr, nodeVar)
	if !ok {
		return nil, false
	}

	sub, ok := findBoundStringBTree(idxMgr, lblScan.Label, pred.propKey)
	if !ok {
		return nil, false
	}

	// Selectivity gate: exact in-range count with an early-exit budget.
	nLabel := g.NodeIndex().Count(uint32(g.Registry().Intern(lblScan.Label)))
	if nLabel < rangeSeekMinLabelPopulation {
		return nil, false
	}
	budget := uint64(float64(nLabel) * rangeSeekMaxSelectivity)
	lo, hi := rangeBoundStrings(pred)
	count, exact := sub.RangeCount(lo, hi, budget)
	if !exact || count == 0 || count > budget {
		// Over budget (early-exited), unknown, or empty: keep the scan. (An
		// empty range is correct but pointless to seek; the scan+filter yields
		// the same zero rows without an index descent.)
		return nil, false
	}

	loB := exec.RangeBound{}
	hiB := exec.RangeBound{}
	if pred.lo != nil {
		loB = *pred.lo
	}
	if pred.hi != nil {
		hiB = *pred.hi
	}
	op := exec.NewNodeByIndexRangeScan(exec.NewStringRangeIndex(sub), loB, hiB)
	schema[nodeVar] = schemaWidth(schema)
	return op, true
}

// findBoundStringBTree returns the first bound string btree index covering
// (label, propKey). Coverage is the same exact (label, property) match the hash
// path uses; an unbound btree (BoundNode ok == false) is never returned.
func findBoundStringBTree(idxMgr *index.Manager, label, propKey string) (boundStringRange, bool) {
	// Auto-named index first ("<label>_<property>_btree"), matching the naming
	// the DDL parser assigns, then any covering bound btree.
	wantName := strings.ToLower(label) + "_" + strings.ToLower(propKey) + "_btree"
	if sub, err := idxMgr.GetIndex(wantName); err == nil && sub.Kind() == "btree" {
		if br, ok := asBoundStringRange(sub, label, propKey); ok {
			return br, true
		}
	}
	for _, name := range idxMgr.ListIndexes() {
		sub, err := idxMgr.GetIndex(name)
		if err != nil || sub.Kind() != "btree" {
			continue
		}
		if br, ok := asBoundStringRange(sub, label, propKey); ok {
			return br, true
		}
	}
	return nil, false
}

// asBoundStringRange type-asserts sub to a bound string range index and checks
// that it covers exactly (label, propKey). ok is false for an int64 btree (the
// Range signature differs), an unbound btree, or a coverage mismatch.
func asBoundStringRange(sub index.Subscriber, label, propKey string) (boundStringRange, bool) {
	br, ok := sub.(boundStringRange)
	if !ok {
		return nil, false
	}
	bl, bp, bound := br.BoundNode()
	if !bound || bl != label || bp != propKey {
		return nil, false
	}
	return br, true
}

// maxStringSentinel is the upper-bound key for an unbounded-above string range
// count — it mirrors the sentinel exec.StringRangeIndex.RangeBitmap uses, so
// the selectivity count and the executed scan agree on the same key space.
const maxStringSentinel = "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff" +
	"\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"

// rangeBoundStrings returns the lo/hi string keys for the EXACT count query,
// using "" for an unbounded lower bound and the maximal string sentinel for an
// unbounded upper bound — matching exec.StringRangeIndex.RangeBitmap. The count
// uses the INCLUSIVE [lo,hi] keys (a tiny over-count of at most the two
// boundary values when a bound is exclusive); inclusivity is enforced at
// execution by the NodeByIndexRangeScan operator, and the residual Selection
// Filter re-checks every row regardless, so the count being a slight upper
// bound only makes the selectivity gate marginally more conservative — never
// wrong.
func rangeBoundStrings(pred stringRangePred) (lo, hi string) {
	lo = ""
	hi = maxStringSentinel
	if pred.lo != nil {
		if sv, ok := pred.lo.Value.(expr.StringValue); ok {
			lo = string(sv)
		}
	}
	if pred.hi != nil {
		if sv, ok := pred.hi.Value.(expr.StringValue); ok {
			hi = string(sv)
		}
	}
	return lo, hi
}

// extractStringRangePred extracts a single-property string range predicate from
// an AST expression: either one comparison (n.prop <op> "lit") or a two-sided
// AND of two comparisons on the SAME property. Returns ok == false for any
// other shape, a non-string literal, a mixed-property AND, or a bound operand
// that is not a plain string literal.
func extractStringRangePred(e ast.Expression, nodeVar string) (stringRangePred, bool) {
	if bo, ok := e.(*ast.BinaryOp); ok && strings.EqualFold(bo.Operator, "AND") {
		left, lok := extractSingleStringCmp(bo.Left, nodeVar)
		right, rok := extractSingleStringCmp(bo.Right, nodeVar)
		if lok && rok && left.propKey == right.propKey {
			return mergeRangeBounds(left, right)
		}
		return stringRangePred{}, false
	}
	return extractSingleStringCmp(e, nodeVar)
}

// extractSingleStringCmp extracts one comparison "nodeVar.prop <op> stringLit"
// (or its mirror "stringLit <op> nodeVar.prop") with op ∈ {>,>=,<,<=}. The
// returned stringRangePred has exactly one of lo/hi set.
func extractSingleStringCmp(e ast.Expression, nodeVar string) (stringRangePred, bool) {
	bo, ok := e.(*ast.BinaryOp)
	if !ok {
		return stringRangePred{}, false
	}
	op := bo.Operator
	if op != ">" && op != ">=" && op != "<" && op != "<=" {
		return stringRangePred{}, false
	}
	// Property on the left: n.prop <op> lit.
	if propKey, isProp := nodePropKey(bo.Left, nodeVar); isProp {
		if sv, isStr := stringLiteral(bo.Right); isStr {
			return boundFor(propKey, op, sv, false), true
		}
		return stringRangePred{}, false
	}
	// Property on the right: lit <op> n.prop — flip the operator.
	if propKey, isProp := nodePropKey(bo.Right, nodeVar); isProp {
		if sv, isStr := stringLiteral(bo.Left); isStr {
			return boundFor(propKey, op, sv, true), true
		}
		return stringRangePred{}, false
	}
	return stringRangePred{}, false
}

// boundFor builds a one-sided stringRangePred for "prop op value", flipping the
// operator's side when the property was on the right of the comparison
// (mirrored == true: "value op prop" ≡ "prop op' value" with op' the reverse).
func boundFor(propKey, op string, value expr.StringValue, mirrored bool) stringRangePred {
	if mirrored {
		switch op {
		case ">":
			op = "<"
		case ">=":
			op = "<="
		case "<":
			op = ">"
		case "<=":
			op = ">="
		}
	}
	rb := exec.RangeBound{Value: value, Include: op == ">=" || op == "<="}
	switch op {
	case ">", ">=":
		return stringRangePred{propKey: propKey, lo: &rb}
	default: // "<", "<="
		return stringRangePred{propKey: propKey, hi: &rb}
	}
}

// mergeRangeBounds combines two one-sided predicates on the same property into
// a two-sided range. ok is false when both bounds are on the same side (e.g.
// n.p > 1 AND n.p > 2 is not a closed range; let the scan+filter handle it).
func mergeRangeBounds(a, b stringRangePred) (stringRangePred, bool) {
	out := stringRangePred{propKey: a.propKey}
	switch {
	case a.lo != nil && b.hi != nil:
		out.lo, out.hi = a.lo, b.hi
	case a.hi != nil && b.lo != nil:
		out.lo, out.hi = b.lo, a.hi
	default:
		return stringRangePred{}, false
	}
	return out, true
}

// nodePropKey returns (propKey, true) when e is nodeVar.<key>.
func nodePropKey(e ast.Expression, nodeVar string) (string, bool) {
	prop, ok := e.(*ast.Property)
	if !ok {
		return "", false
	}
	v, ok := prop.Receiver.(*ast.Variable)
	if !ok || v.Name != nodeVar {
		return "", false
	}
	return prop.Key, true
}

// stringLiteral returns (value, true) when e is a plain string literal. A
// parameter or any other expression is declined: the seek is a build-time
// decision and only a literal string can be a same-class scalar bound here
// (parameter range seeks are deliberately out of scope for this increment).
func stringLiteral(e ast.Expression) (expr.StringValue, bool) {
	if sl, ok := e.(*ast.StringLiteral); ok {
		return expr.StringValue(sl.Value), true
	}
	return "", false
}
