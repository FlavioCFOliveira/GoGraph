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
	"math"
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
	// RangeFrom serves an unbounded-above range: a fixed sentinel is not a true
	// maximum for a variable-length string key, so an open-ended scan is the
	// only superset-complete way to serve n.prop >= 'A' (#F-CY1).
	RangeFrom(lo string) *roaring64.Bitmap
	RangeCount(lo, hi string, budget uint64) (uint64, bool)
	RangeCountFrom(lo string, budget uint64) (uint64, bool)
	BoundNode() (label, property string, ok bool)
}

// stringRangePred is the extracted range predicate on a single node property:
// the bounds and their inclusivity. An absent bound (nil) is unbounded on that
// side.
type stringRangePred struct {
	lo      *exec.RangeBound // nil = unbounded below
	hi      *exec.RangeBound // nil = unbounded above
	propKey string
}

// boundNumericRange is satisfied by a bound UNIFIED numeric btree companion
// (#1652): a btree.Index[float64] that indexes both integer- and float-valued
// nodes under one float64 order. It exposes the range query, the exact
// early-exit cardinality, and the (label, property) coverage. An UNBOUND btree
// is never selected (BoundNode ok == false) — it is not maintained from the
// change fan-out and could be stale.
type boundNumericRange interface {
	Range(lo, hi float64) *roaring64.Bitmap
	RangeCount(lo, hi float64, budget uint64) (uint64, bool)
	BoundNode() (label, property string, ok bool)
}

// numericRangePred is the extracted numeric range predicate on a single node
// property: the float64 bounds and their inclusivity. An absent bound (nil) is
// unbounded on that side. A bound that came from an integer or a parameter is
// already coerced to float64 (the unified numeric order). The original AST
// operand is preserved in loVal/hiVal so the executed NodeByIndexRangeScan
// receives an inclusive superset bound (see [tryNumericRangeSeek]).
type numericRangePred struct {
	lo      *numericBound // nil = unbounded below
	hi      *numericBound // nil = unbounded above
	propKey string
}

// numericBound is one endpoint of a numeric range: the float64 value and
// whether the bound is inclusive.
type numericBound struct {
	value   float64
	include bool
}

// buildRangeSeekIfEnabled is the gated entry point: it returns no range seek
// when the optimisation is disabled (EngineOptions.DisableRangeIndexSeek, or any
// build path that does not set bopts.rangeSeekEnabled, such as the public
// BuildPlanWithMutator). The Engine's WRITE path DOES set it as of #2225 — it
// previously did not, which planned every statement carrying a write clause with
// a bare label scan; see [planGates]. When enabled this delegates to
// [tryBuildRangeSeekChild].
//
// The STARTS WITH prefix rewrite (#2127) carries its OWN gate
// (bopts.prefixSeekEnabled, from EngineOptions.DisablePrefixIndexSeek) rather
// than riding this one, so the differential harness can toggle the prefix
// rewrite alone and leave the `>=`/`<` seek active in BOTH arms — two arms that
// differ in exactly one variable. A prefix seek still requires the range seek to
// be enabled at all, since it is built by the same peephole.
func buildRangeSeekIfEnabled(
	bopts *buildOpts,
	sel *ir.Selection,
	schema map[string]int,
	idxMgr *index.Manager,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
) (exec.Operator, bool) {
	if bopts == nil || !bopts.rangeSeekEnabled {
		return nil, false
	}
	return tryBuildRangeSeekChild(sel, schema, idxMgr, g, params,
		bopts.prefixSeekEnabled, bopts.bitmapIntersectEnabled)
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
// prefixSeek admits the STARTS WITH prefix rewrite (#2127); when false the
// predicate is not recognised at all and the plan is byte-identical to the one
// built before that change. intersectSeek admits composing several indexed
// conjuncts into one intersected probe (#2134); it rides the same
// EngineOptions.DisableBitmapIntersection knob as the label intersection, since
// both are the same lever — a set operation over Roaring bitmaps — and a caller
// disabling one means to disable the other.
func tryBuildRangeSeekChild(
	sel *ir.Selection,
	schema map[string]int,
	idxMgr *index.Manager,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	prefixSeek bool,
	intersectSeek bool,
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

	// Conjunctive indexed properties FIRST (#2134): when two or more conjuncts
	// constrain DIFFERENT indexed properties of this node, compose their bitmaps by
	// intersection rather than using one index and filtering the rest per row. It is
	// tried before the single-property seeks because those recognise only one
	// property and would claim the shape with just one of the available indexes,
	// leaving the other conjunct to the row filter. Every part passes the same
	// shipped gate, and the residual Filter is retained as always, so a declined
	// composition falls straight through to the single-property paths below.
	if intersectSeek {
		if op, ok := tryIndexIntersectionSeek(sel, schema, idxMgr, g, lblScan, nodeVar, params, prefixSeek); ok {
			return op, true
		}
	}
	// Try the string-btree path first (a string range over a string-typed
	// index). When the predicate is not a string range — typically a numeric
	// range n.age > 30 — fall through to the unified numeric companion.
	if op, ok := tryStringRangeSeek(sel, schema, idxMgr, g, lblScan, nodeVar, prefixSeek); ok {
		return op, true
	}
	return tryNumericRangeSeek(sel, schema, idxMgr, g, lblScan, nodeVar, params)
}

// tryStringRangeSeek builds a NodeByIndexRangeScan over a bound string btree
// when the Selection predicate is a single-property string range and the
// in-range count is a selective win. See [tryBuildRangeSeekChild] for the
// shared preconditions (label scan, residual filter).
func tryStringRangeSeek(
	sel *ir.Selection,
	schema map[string]int,
	idxMgr *index.Manager,
	g *lpg.Graph[string, float64],
	lblScan *ir.NodeByLabelScan,
	nodeVar string,
	prefixSeek bool,
) (exec.Operator, bool) {
	pred, ok := extractStringRangePred(sel.PredicateExpr, nodeVar, prefixSeek)
	if !ok {
		return nil, false
	}

	sub, ok := findBoundStringBTree(idxMgr, lblScan.Label, pred.propKey)
	if !ok {
		return nil, false
	}

	// The selectivity count must probe the SAME key space the executed scan
	// walks: for an unbounded-above range the scan is open-ended (RangeFrom),
	// so the count uses RangeCountFrom — not a fixed sentinel that would
	// mis-count (and, in the executed scan, silently drop) any key sorting
	// above it (#F-CY1). "" is a true minimum for the string order, so an
	// unbounded-below lower bound needs no such treatment.
	loKey := ""
	if pred.lo != nil {
		if sv, okSv := pred.lo.Value.(expr.StringValue); okSv {
			loKey = string(sv)
		}
	}
	var countFn func(budget uint64) (uint64, bool)
	if pred.hi != nil {
		hiKey := ""
		if sv, okSv := pred.hi.Value.(expr.StringValue); okSv {
			hiKey = string(sv)
		}
		countFn = func(b uint64) (uint64, bool) { return sub.RangeCount(loKey, hiKey, b) }
	} else {
		countFn = func(b uint64) (uint64, bool) { return sub.RangeCountFrom(loKey, b) }
	}
	if !rangeCountWinsFn(g, lblScan.Label, countFn) {
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

// rangeCountWins applies the shared selectivity/population gate: the label
// population must be at least rangeSeekMinLabelPopulation, and the EXACT
// in-range count (early-exit at budget) must be non-empty and within
// rangeSeekMaxSelectivity of the population. count is the type-specific
// RangeCount closure (string or float64). The count is INCLUSIVE [lo, hi]
// (a tiny over-count of at most the two boundary values when a bound is
// exclusive), which only makes the gate marginally more conservative; the
// residual Selection Filter re-checks every row regardless.
func rangeCountWins[K any](
	g *lpg.Graph[string, float64],
	label string,
	rangeCount func(lo, hi K, budget uint64) (uint64, bool),
	lo, hi K,
) bool {
	return rangeCountWinsFn(g, label, func(b uint64) (uint64, bool) { return rangeCount(lo, hi, b) })
}

// rangeCountWinsFn is the closure form of the selectivity/population gate: it
// applies the shared population floor and selectivity ceiling to an arbitrary
// exact-count-with-budget closure. The string path uses it directly to select
// between the bounded [lo,hi] count and the open-ended RangeCountFrom for an
// unbounded-above range (#F-CY1); the generic [rangeCountWins] delegates here.
func rangeCountWinsFn(
	g *lpg.Graph[string, float64],
	label string,
	rangeCount func(budget uint64) (uint64, bool),
) bool {
	budget, ok := rangeSeekBudget(g, label)
	if !ok {
		return false
	}
	count, exact := rangeCount(budget)
	return rangeCountWithinBudget(count, exact, budget)
}

// rangeSeekBudget returns the exact-count budget the shipped gate allows for
// label, and ok == false when the label population is below
// rangeSeekMinLabelPopulation — in which case NO count can change the verdict and
// the caller must not take one.
//
// It is factored out of [rangeCountWinsFn] so the conjunctive intersection path
// can derive the SAME budget once for a whole predicate and hand it to each
// conjunct's count, instead of counting without a bound and comparing afterwards
// (#2266). Sharing the derivation is what keeps the two paths from drifting: the
// population floor and the selectivity ceiling are defined here and nowhere else.
func rangeSeekBudget(g *lpg.Graph[string, float64], label string) (uint64, bool) {
	nLabel := g.NodeIndex().Count(uint32(g.Registry().Intern(label)))
	if nLabel < rangeSeekMinLabelPopulation {
		return 0, false
	}
	return uint64(float64(nLabel) * rangeSeekMaxSelectivity), true
}

// rangeCountWithinBudget is the selectivity half of the shipped gate, applied to
// a count already taken against budget: the count must be exact (not
// early-exited), non-empty, and within budget.
//
// Over budget, unknown, or empty: keep the scan. (An empty range is correct but
// pointless to seek; the scan+filter yields the same zero rows without an index
// descent.)
func rangeCountWithinBudget(count uint64, exact bool, budget uint64) bool {
	return exact && count != 0 && count <= budget
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

// extractStringRangePred extracts a single-property string range predicate from
// an AST expression: either one comparison (n.prop <op> "lit") or a two-sided
// AND of two comparisons on the SAME property. Returns ok == false for any
// other shape, a non-string literal, a mixed-property AND, or a bound operand
// that is not a plain string literal.
// The descent structure is load-bearing for SOUNDNESS, not just for tidiness:
// the ONLY shapes reachable are a direct comparison and a top-level AND of two
// direct comparisons. That is what excludes a NEGATED predicate, which selects
// the COMPLEMENT of the range and would make the seek a non-superset:
// `NOT (n.p STARTS WITH 'ab')` presents an *ast.UnaryOp, which
// [extractSingleStringCmp] declines, and `NOT x AND y` declines for the same
// reason on its left arm. An OR arrives as a *ast.BinaryOp whose operator is not
// in the accepted set and is declined too. Widening this descent — pushing
// through NOT, or accepting OR — would break the superset invariant that every
// caller of this file relies on. See docs/design-prefix-range-seek.md §5.1.
func extractStringRangePred(e ast.Expression, nodeVar string, prefixSeek bool) (stringRangePred, bool) {
	if bo, ok := e.(*ast.BinaryOp); ok && strings.EqualFold(bo.Operator, "AND") {
		left, lok := extractSingleStringCmp(bo.Left, nodeVar, prefixSeek)
		right, rok := extractSingleStringCmp(bo.Right, nodeVar, prefixSeek)
		if lok && rok && left.propKey == right.propKey {
			return mergeRangeBounds(left, right)
		}
		return stringRangePred{}, false
	}
	return extractSingleStringCmp(e, nodeVar, prefixSeek)
}

// startsWithOp is the AST operator string the parser emits for a prefix
// predicate (cypher/parser/visitor.go).
const startsWithOp = "STARTS WITH"

// prefixSuccessor returns the least string strictly greater than EVERY string
// having p as a prefix — the exclusive upper bound of the prefix range
// [p, succ(p)). ok is false when no finite successor exists (p is empty, or
// every byte of p is 0xFF), in which case the caller must leave the upper bound
// UNBOUNDED so the scan runs open-ended (see [boundFor]).
//
// # Why the BYTE successor and not a code-point increment
//
// The btree's total order for a string key is cmp.Compare on the Go string —
// byte-wise lexicographic, no collation, no normalisation (graph/index/btree/
// bplus.go) — and openCypher's STARTS WITH is strings.HasPrefix, a byte test
// (cypher/expr/eval.go). Both sides of the rewrite therefore already share ONE
// byte basis, so incrementing a byte matches both without a translation step.
// It also strictly dominates a code-point increment: it exists for every
// non-empty prefix that is not all-0xFF (a code-point increment has no
// successor when the last code point is U+10FFFF, forcing a much weaker
// unbounded scan), and it is tighter — for a prefix ending in U+00FF (C3 BF) it
// yields C3 C0 where the code-point form yields C4 80. succ is a comparison key
// only: it is never stored, never returned, and never decoded, so it does not
// matter that it may not be valid UTF-8.
//
// # Proof that [p, succ] is a superset, and how tight it is
//
// Let i be the last index with p[i] < 0xFF, so succ = p[:i] ++ (p[i]+1). For any
// s with HasPrefix(s, p): p ≤ s because a prefix never exceeds the string it
// prefixes; and s < succ because bytes 0..i-1 agree while succ[i] = p[i]+1 >
// p[i] = s[i], so the first differing byte settles it.
//
// The converse very nearly holds, which is what keeps the seek tight: any key k
// with p ≤ k < succ must have p as a prefix (bytes 0..i must equal p's, and
// bytes i+1.. of p are all 0xFF by the choice of i, so k ≥ p forces those to
// match as well). Hence the CLOSED interval the operator actually walks —
// NodeByIndexRangeScan emits the inclusive superset and ignores Include, see
// #F-EXEC1 — over-returns at most the single key equal to succ, which the
// residual Filter removes. See docs/design-prefix-range-seek.md §3.
func prefixSuccessor(p string) (string, bool) {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == 0xFF {
			continue
		}
		b := []byte(p[:i+1])
		b[i]++
		return string(b), true
	}
	return "", false
}

// extractSingleStringCmp extracts one comparison "nodeVar.prop <op> stringLit"
// (or its mirror "stringLit <op> nodeVar.prop") with op ∈ {=,>,>=,<,<=}. For a
// range operator the returned stringRangePred has exactly one of lo/hi set; for
// "=" it has BOTH, set to the same value — the degenerate closed range [v, v].
//
// # Why equality is served here as well as by a string hash index
//
// A string equality normally reaches the hash index, so this looks redundant. It
// is not: a user who asks for a BTREE on a string property — a reasonable thing
// to do when the same property also serves range predicates — used to get a FULL
// LABEL SCAN for an equality on it, because only the numeric extractor
// degenerated "=" into [v, v] (rmp #2169) and this one rejected the operator
// outright. Measured at 20 000 nodes: 4.27 ms and 59 831 allocs/op, the
// allocation count tracking the node population, against 3.2 µs and 63 allocs
// for the same equality on a hash index (rmp #2231).
//
// The degenerate range is exact for EQUALITY specifically. GoGraph orders strings
// by code point (UTF-8 byte order), so [v, v] selects precisely the keys whose
// bytes equal v's — no collation question arises, because no two distinct strings
// compare equal under a byte order. That reasoning does NOT extend to inequality
// over a collation-sensitive alphabet, which is why only "=" is added here and
// the range operators keep their existing one-sided treatment (round-4 finding
// C3 leaves the collation ruling open).
//
// As on the numeric path, the seek's result is treated as a SUPERSET: the range
// seek's residual Selection Filter is always retained
// (see [tryBuildRangeSeekChild]) and re-applies the exact property predicate, so
// the seek can only narrow what the filter examines, never change what it admits.
// # Why the prefix predicate is served here too (#2127)
//
// `n.prop STARTS WITH 'p'` IS a range predicate: the prefix set is exactly the
// half-open interval [p, succ(p)) of the byte order the btree is laid out in
// (see [prefixSuccessor] for the construction and its proof). Before this it
// full-scanned the label and refiltered every row: measured at 50 000 nodes,
// 11.28 ms and 149 829 allocs/op — the allocation count tracking the label
// population, the signature of a scan — against 38.23 µs and 566 allocs for the
// identical predicate written as `>= 'p' AND < 'succ'`, a 295× / 265× gap on the
// same 100-row answer (rmp #2126).
//
// Two boundaries are deliberate. Only `n.prop STARTS WITH lit` is admitted: the
// operator is NOT symmetric, so the mirrored `lit STARTS WITH n.prop` tests a
// literal against a property-valued PREFIX and describes no range over n.prop.
// And ENDS WITH / CONTAINS are excluded by proof, not by omission — their match
// set is not an interval of the byte order ("ax" < "b" < "bx" with the outer two
// matching and the middle one not), so the tightest sound interval degenerates
// to the whole index: not unsound, but useless, and vetoed by the selectivity
// gate anyway (docs/design-prefix-range-seek.md §5.3).
func extractSingleStringCmp(e ast.Expression, nodeVar string, prefixSeek bool) (stringRangePred, bool) {
	bo, ok := e.(*ast.BinaryOp)
	if !ok {
		return stringRangePred{}, false
	}
	op := bo.Operator
	isPrefix := prefixSeek && op == startsWithOp
	if !isPrefix && op != "=" && op != ">" && op != ">=" && op != "<" && op != "<=" {
		return stringRangePred{}, false
	}
	// Property on the left: n.prop <op> lit.
	if propKey, isProp := nodePropKey(bo.Left, nodeVar); isProp {
		if sv, isStr := stringLiteral(bo.Right); isStr {
			return boundFor(propKey, op, sv, false), true
		}
		return stringRangePred{}, false
	}
	// Property on the right: lit <op> n.prop — flip the operator. STARTS WITH has
	// no meaningful flip (see the doc comment), so the mirrored form is declined.
	if propKey, isProp := nodePropKey(bo.Right, nodeVar); isProp {
		if isPrefix {
			return stringRangePred{}, false
		}
		if sv, isStr := stringLiteral(bo.Left); isStr {
			return boundFor(propKey, op, sv, true), true
		}
		return stringRangePred{}, false
	}
	return stringRangePred{}, false
}

// boundFor builds a stringRangePred for "prop op value", flipping the operator's
// side when the property was on the right of the comparison (mirrored == true:
// "value op prop" ≡ "prop op' value" with op' the reverse). A range operator
// yields one bound; "=" yields the degenerate closed range [value, value], and
// STARTS WITH yields [value, succ(value)) (see [extractSingleStringCmp]).
func boundFor(propKey, op string, value expr.StringValue, mirrored bool) stringRangePred {
	if op == startsWithOp {
		// A prefix is a range: lower bound the prefix itself, upper bound its
		// exclusive successor. When no finite successor exists — the empty prefix,
		// or an all-0xFF prefix — hi stays nil, which routes the executed scan to
		// the index's OPEN-ENDED RangeFrom and the gate to RangeCountFrom (#F-CY1),
		// so the counted and the walked key space stay the same. The empty prefix
		// then spans every indexed key and the selectivity gate declines it, which
		// is right: `s STARTS WITH ''` is true of every string, so there is nothing
		// for a seek to narrow.
		//
		// Include on the upper bound is metadata only — NodeByIndexRangeScan emits
		// the inclusive [lo, hi] superset and the residual Filter enforces exactness
		// (#F-EXEC1) — so the closed walk over [value, succ] admits at most the one
		// extra key equal to succ, which the Filter then rejects.
		lo := exec.RangeBound{Value: value, Include: true}
		succ, ok := prefixSuccessor(string(value))
		if !ok {
			return stringRangePred{propKey: propKey, lo: &lo}
		}
		hi := exec.RangeBound{Value: expr.StringValue(succ), Include: false}
		return stringRangePred{propKey: propKey, lo: &lo, hi: &hi}
	}
	if op == "=" {
		// Equality is symmetric, so the mirror needs no flip. Two SEPARATE bounds
		// are built rather than one shared pointer: mergeRangeBounds and the
		// consumers treat lo and hi as independently owned.
		lo := exec.RangeBound{Value: value, Include: true}
		hi := exec.RangeBound{Value: value, Include: true}
		return stringRangePred{propKey: propKey, lo: &lo, hi: &hi}
	}
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

// ─────────────────────────────────────────────────────────────────────────────
// Numeric range seek (#1652) — unified float64 companion
// ─────────────────────────────────────────────────────────────────────────────

// tryNumericRangeSeek builds a NodeByIndexRangeScan over the UNIFIED numeric
// btree companion when the Selection predicate is a single-property numeric
// range (n.age > 30, with integer OR float literals, or numeric PARAMETER
// bounds n.age > $min) and the in-range count is a selective win.
//
// # Safety (cypher-expert-consultant, #1652)
//
// The seek is result-identical to NodeByLabelScan+Filter because:
//
//   - The companion indexes BOTH integer- and float-valued nodes under one
//     float64 order, so it is a SUPERSET of every numeric match — never the
//     non-superset an int64-only index would be (which would drop float-valued
//     matches).
//   - The original AST predicate is ALWAYS retained as a residual Filter on
//     top (stacked by the caller in buildOperator), so any over-return is
//     removed and null / NaN / cross-type / open-vs-closed-bound cases resolve
//     exactly as the full scan+filter would.
//   - The operator returns the inclusive [lo, hi] superset (it does not enforce
//     open bounds itself — see NodeByIndexRangeScan, #F-EXEC1). Exact
//     open/closed semantics are enforced solely by the residual Filter.
//   - NaN and null/missing are never indexed (projectNumericPropValue), and a
//     numeric bound (never NaN) over the btree's total order never returns the
//     NaN key even if one existed.
//
// PARAMETER bounds are admitted here even though the string path declines them:
// they are safe (superset + residual filter) and are the common shape of a
// numeric range. The parameter is resolved against params at build time; a
// missing or non-numeric parameter declines the seek (the scan+filter path is
// correct).
func tryNumericRangeSeek(
	sel *ir.Selection,
	schema map[string]int,
	idxMgr *index.Manager,
	g *lpg.Graph[string, float64],
	lblScan *ir.NodeByLabelScan,
	nodeVar string,
	params map[string]expr.Value,
) (exec.Operator, bool) {
	pred, ok := extractNumericRangePred(sel.PredicateExpr, nodeVar, params)
	if !ok {
		return nil, false
	}

	sub, ok := findBoundNumericBTree(idxMgr, lblScan.Label, pred.propKey)
	if !ok {
		return nil, false
	}

	lo, hi := rangeBoundFloats(pred)
	if !rangeCountWins(g, lblScan.Label, sub.RangeCount, lo, hi) {
		return nil, false
	}

	// The operator returns the inclusive [lo, hi] superset; the residual
	// Selection Filter enforces the exact open/closed predicate (#F-EXEC1). An
	// unbounded side stays nil (the adapter widens it to ∓∞). Include is set for
	// documentation only — the operator no longer enforces it.
	loB := exec.RangeBound{}
	hiB := exec.RangeBound{}
	if pred.lo != nil {
		loB = exec.RangeBound{Value: expr.FloatValue(pred.lo.value), Include: true}
	}
	if pred.hi != nil {
		hiB = exec.RangeBound{Value: expr.FloatValue(pred.hi.value), Include: true}
	}
	op := exec.NewNodeByIndexRangeScan(exec.NewFloat64RangeIndex(sub), loB, hiB)
	schema[nodeVar] = schemaWidth(schema)
	return op, true
}

// findBoundNumericBTree returns the first bound numeric btree companion
// covering (label, propKey). It probes the deterministic internal companion
// name ("<label>_<property>_btree_num") first, then any covering bound numeric
// btree as a fallback. An unbound btree (BoundNode ok == false) and a string
// btree (whose Range signature differs) are never returned.
func findBoundNumericBTree(idxMgr *index.Manager, label, propKey string) (boundNumericRange, bool) {
	wantName := numericBTreeName(label, propKey)
	if sub, err := idxMgr.GetIndex(wantName); err == nil && sub.Kind() == "btree" {
		if br, ok := asBoundNumericRange(sub, label, propKey); ok {
			return br, true
		}
	}
	for _, name := range idxMgr.ListIndexes() {
		sub, err := idxMgr.GetIndex(name)
		if err != nil || sub.Kind() != "btree" {
			continue
		}
		if br, ok := asBoundNumericRange(sub, label, propKey); ok {
			return br, true
		}
	}
	return nil, false
}

// asBoundNumericRange type-asserts sub to a bound numeric range index and
// checks that it covers exactly (label, propKey). ok is false for a string
// btree (the Range signature differs), an unbound btree, or a coverage
// mismatch.
func asBoundNumericRange(sub index.Subscriber, label, propKey string) (boundNumericRange, bool) {
	br, ok := sub.(boundNumericRange)
	if !ok {
		return nil, false
	}
	bl, bp, bound := br.BoundNode()
	if !bound || bl != label || bp != propKey {
		return nil, false
	}
	return br, true
}

// rangeBoundFloats returns the lo/hi float64 keys for the EXACT count query,
// using -∞ for an unbounded lower bound and +∞ for an unbounded upper bound —
// matching exec.Float64RangeIndex.RangeBitmap. The count uses the INCLUSIVE
// [lo, hi] keys; inclusivity is enforced at execution by the residual Filter,
// and the count being a slight upper bound only makes the selectivity gate
// marginally more conservative (see [rangeCountWins]).
func rangeBoundFloats(pred numericRangePred) (lo, hi float64) {
	lo = math.Inf(-1)
	hi = math.Inf(1)
	if pred.lo != nil {
		lo = pred.lo.value
	}
	if pred.hi != nil {
		hi = pred.hi.value
	}
	return lo, hi
}

// extractNumericRangePred extracts a single-property numeric range predicate
// from an AST expression: either one comparison (n.prop <op> numeric) or a
// two-sided AND of two comparisons on the SAME property. The numeric operand
// may be an integer literal, a float literal, or a parameter resolving to a
// numeric value. Returns ok == false for any other shape, a non-numeric
// operand, a mixed-property AND, or a parameter that is absent / non-numeric.
func extractNumericRangePred(e ast.Expression, nodeVar string, params map[string]expr.Value) (numericRangePred, bool) {
	if bo, ok := e.(*ast.BinaryOp); ok && strings.EqualFold(bo.Operator, "AND") {
		left, lok := extractSingleNumericCmp(bo.Left, nodeVar, params)
		right, rok := extractSingleNumericCmp(bo.Right, nodeVar, params)
		if lok && rok && left.propKey == right.propKey {
			return mergeNumericRangeBounds(left, right)
		}
		return numericRangePred{}, false
	}
	return extractSingleNumericCmp(e, nodeVar, params)
}

// extractSingleNumericCmp extracts one comparison "nodeVar.prop <op> numeric"
// (or its mirror "numeric <op> nodeVar.prop") with op ∈ {=,>,>=,<,<=}. For a
// range operator the returned numericRangePred has exactly one of lo/hi set; for
// "=" it has BOTH, set to the same value — the degenerate closed range [v, v].
//
// # Why equality is served here rather than by a numeric hash index
//
// A Cypher hash index is string-only (projectStringPropValue rejects every
// non-string payload), so before rmp #2169 a numeric equality — including the
// inline form MATCH (a:P {id: 250}), which desugars to this same predicate —
// full-scanned the label even when the property carried a btree index whose
// numeric companion could answer it. Only equality degenerated: the identical
// predicate written as "a.id >= 250 AND a.id <= 250" already seeked.
//
// The source comment at cypher/api.go proposed a float64 hash index. That would
// be WRONG for openCypher, whose numeric equality is cross-type: 5 = 5.0 is
// TRUE, so an int64-keyed hash would silently miss float-valued matches and a
// float64-keyed one would have to bucket ints by their float image, which is
// lossy above 2^53. The unified float64 btree companion already ships and
// already indexes integer- and float-valued nodes under ONE numeric order, so
// the closed range [v, v] over it is a SUPERSET of every value equal to v under
// Cypher semantics, for both int and float properties and across the two.
//
// Above 2^53 distinct int64 values share a float64 image, so the range returns
// extra candidates. That is safe by construction and not by luck: the range
// seek's residual Selection Filter is ALWAYS retained (see
// [tryBuildRangeSeekChild]) and applies the exact int/float comparator, so
// 2^53+1 = 2^53.0 still evaluates FALSE despite the shared bucket. The seek can
// only ever narrow what the filter examines, never change what it admits.
func extractSingleNumericCmp(e ast.Expression, nodeVar string, params map[string]expr.Value) (numericRangePred, bool) {
	bo, ok := e.(*ast.BinaryOp)
	if !ok {
		return numericRangePred{}, false
	}
	op := bo.Operator
	if op != "=" && op != ">" && op != ">=" && op != "<" && op != "<=" {
		return numericRangePred{}, false
	}
	// Property on the left: n.prop <op> numeric.
	if propKey, isProp := nodePropKey(bo.Left, nodeVar); isProp {
		if f, isNum := numericOperand(bo.Right, params); isNum {
			return numericBoundFor(propKey, op, f, false), true
		}
		return numericRangePred{}, false
	}
	// Property on the right: numeric <op> n.prop — flip the operator.
	if propKey, isProp := nodePropKey(bo.Right, nodeVar); isProp {
		if f, isNum := numericOperand(bo.Left, params); isNum {
			return numericBoundFor(propKey, op, f, true), true
		}
		return numericRangePred{}, false
	}
	return numericRangePred{}, false
}

// numericBoundFor builds a numericRangePred for "prop op value", flipping the
// operator's side when the property was on the right of the comparison
// (mirrored == true). A range operator yields one bound; "=" yields the
// degenerate closed range [value, value] (see [extractSingleNumericCmp]).
func numericBoundFor(propKey, op string, value float64, mirrored bool) numericRangePred {
	if op == "=" {
		// Equality is symmetric, so mirroring is a no-op. Both bounds are
		// inclusive and distinct values so neither aliases the other.
		lo := numericBound{value: value, include: true}
		hi := numericBound{value: value, include: true}
		return numericRangePred{propKey: propKey, lo: &lo, hi: &hi}
	}
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
	nb := numericBound{value: value, include: op == ">=" || op == "<="}
	switch op {
	case ">", ">=":
		return numericRangePred{propKey: propKey, lo: &nb}
	default: // "<", "<="
		return numericRangePred{propKey: propKey, hi: &nb}
	}
}

// mergeNumericRangeBounds combines two one-sided predicates on the same
// property into a two-sided range. ok is false when both bounds are on the
// same side (e.g. n.p > 1 AND n.p > 2 is not a closed range; let the
// scan+filter handle it).
func mergeNumericRangeBounds(a, b numericRangePred) (numericRangePred, bool) {
	out := numericRangePred{propKey: a.propKey}
	switch {
	case a.lo != nil && b.hi != nil:
		out.lo, out.hi = a.lo, b.hi
	case a.hi != nil && b.lo != nil:
		out.lo, out.hi = b.lo, a.hi
	default:
		return numericRangePred{}, false
	}
	return out, true
}

// numericOperand returns (float64, true) when e is an integer literal, a float
// literal, or a parameter resolving to a numeric value. An integer and a float
// map onto the same float64 numeric order. A finite numeric value is required:
// a NaN operand declines (the range it would describe is empty under the total
// order, and the scan+filter path yields the correct empty result). An
// OverflowIntLit (an integer beyond int64) declines: the residual filter would
// still be correct, but the bound cannot be represented as a same-class scalar
// here, so the scan+filter path handles it. A parameter that is absent or
// non-numeric declines.
func numericOperand(e ast.Expression, params map[string]expr.Value) (float64, bool) {
	switch lit := e.(type) {
	case *ast.IntLiteral:
		return float64(lit.Value), true
	case *ast.FloatLiteral:
		if math.IsNaN(lit.Value) {
			return 0, false
		}
		return lit.Value, true
	case *ast.Parameter:
		v, ok := params[lit.Name]
		if !ok || v == nil {
			return 0, false
		}
		switch n := v.(type) {
		case expr.IntegerValue:
			return float64(n), true
		case expr.FloatValue:
			if math.IsNaN(float64(n)) {
				return 0, false
			}
			return float64(n), true
		}
	}
	return 0, false
}
