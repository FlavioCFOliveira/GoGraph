package cypher

// index_intersect_plan.go — conjunctive indexed property predicates composed by
// bitmap intersection (#2134, design §8 of docs/design-bitmap-intersection.md).
//
// `WHERE n.a > 1 AND n.b < 9` used at most ONE index and filtered the other
// conjunct per row, because the range-seek peephole extracts a range on a SINGLE
// property (a two-sided AND on the same property, never a conjunction across
// properties). But RangeBitmap already returns a Roaring bitmap, so two ordinary
// single-property indexes compose by intersection:
//
//	NodeByIndexRangeScan over  bitmap(a ∈ (1, ∞)) ∩ bitmap(b ∈ (-∞, 9))
//
// That is the answer Memgraph needs a dedicated COMPOSITE index type to give, and
// GoGraph gets it with **no new index type and no new statistic** — any two
// single-property indexes on the same label compose.
//
// # Superset discipline — the residual Filter is MANDATORY here
//
// This is the one sharp difference from the label intersection (#2133), where the
// bitmap is exact and the residual Filter is dropped. A range-index bitmap is a
// SUPERSET by design: NodeByIndexRangeScan emits the inclusive [lo, hi] interval
// and cannot enforce an open bound (#F-EXEC1). Intersection PRESERVES the superset
// property —
//
//	Bᵃ ⊇ Aᵃ and Bᵇ ⊇ Aᵇ  ⟹  Bᵃ ∩ Bᵇ ⊇ Aᵃ ∩ Aᵇ
//
// — so the composed probe is sound, but the exact predicate must still be
// re-applied to every surviving row. The caller retains the ORIGINAL Selection
// predicate as the residual Filter exactly as it does for a single range seek, so
// this file changes only which candidates the Filter examines, never which rows it
// admits.
//
// # Gate: the shipped per-predicate gate, per conjunct
//
// Every participating conjunct must independently pass the shipped range-seek gate
// (rangeCountWinsFn: label population ≥ rangeSeekMinLabelPopulation, exact
// in-range count ≤ rangeSeekMaxSelectivity of it). That is deliberately
// conservative and needs no new statistic: a conjunct whose own range covers most
// of the label would cost more to probe and AND than the rows it removes, so it is
// left to the residual Filter instead of being intersected. Parts are ANDed in
// ascending exact-count order so the cheapest bitmap is materialised first, the
// same ordering rule #2133 established for labels.

import (
	"sort"
	"strings"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// indexIntersectBuildCount counts how many times the planner has composed two or
// more single-property indexes into one intersected access path. A diagnostic seam
// read only by the in-package tests, so a green test cannot mean "the path never
// fired". Process-global and monotonic.
var indexIntersectBuildCount atomic.Uint64

// equalFoldAnd reports whether op is the AND operator, case-insensitively, matching
// how the range-seek extractors recognise it.
func equalFoldAnd(op string) bool { return strings.EqualFold(op, "AND") }

// indexRangeConjunct is one indexed conjunct's contribution: the property it
// constrains, the index to probe, the bounds, and the EXACT number of index
// entries in that range (used both to order the AND and to gate it).
type indexRangeConjunct struct {
	index   exec.IndexRangePart
	propKey string
	count   uint64
}

// tryIndexIntersectionSeek composes two or more indexed conjuncts on DIFFERENT
// properties of the same node into one intersected range scan.
//
// ok is false — leaving the caller to try the single-property range seeks and then
// the plain scan — when the predicate is not a conjunction, when fewer than two
// distinct properties have a covering bound index whose range passes the shipped
// gate, or when the child is not a label scan.
func tryIndexIntersectionSeek(
	sel *ir.Selection,
	schema map[string]int,
	idxMgr *index.Manager,
	g *lpg.Graph[string, float64],
	lblScan *ir.NodeByLabelScan,
	nodeVar string,
	params map[string]expr.Value,
	prefixSeek bool,
) (exec.Operator, bool) {
	conjuncts := flattenAndSpine(sel.PredicateExpr)
	if len(conjuncts) < 2 {
		return nil, false
	}

	// One entry per DISTINCT property, first-recognised-bounds-win. Keeping only
	// the first conjunct per property is sound because every bound retained is a
	// necessary condition of the conjunction, so the probe stays a superset; any
	// further conjunct on the same property is simply left to the residual Filter.
	seen := make(map[string]bool, len(conjuncts))
	parts := make([]indexRangeConjunct, 0, len(conjuncts))
	for _, c := range conjuncts {
		part, ok := recogniseIndexedConjunct(c, nodeVar, lblScan.Label, idxMgr, g, params, prefixSeek)
		if !ok || seen[part.propKey] {
			continue
		}
		seen[part.propKey] = true
		parts = append(parts, part)
	}
	if len(parts) < 2 {
		return nil, false
	}

	// Ascending exact count: the cheapest bitmap is built first and the AND shrinks
	// from there, the same ordering rule the label intersection uses. Ties break on
	// the property key so the plan is stable run to run.
	sort.SliceStable(parts, func(a, b int) bool {
		if parts[a].count != parts[b].count {
			return parts[a].count < parts[b].count
		}
		return parts[a].propKey < parts[b].propKey
	})

	extra := make([]exec.IndexRangePart, 0, len(parts)-1)
	for _, p := range parts[1:] {
		extra = append(extra, p.index)
	}
	op := exec.NewNodeByIndexIntersectionScan(
		parts[0].index.Index, parts[0].index.Lo, parts[0].index.Hi, extra)
	schema[nodeVar] = schemaWidth(schema)
	indexIntersectBuildCount.Add(1)
	return op, true
}

// recogniseIndexedConjunct maps ONE conjunct to an index probe: a single-property
// comparison (string or numeric, including the STARTS WITH prefix form) whose
// property carries a covering bound index, and whose exact in-range count passes
// the shipped range-seek gate.
func recogniseIndexedConjunct(
	e ast.Expression,
	nodeVar, label string,
	idxMgr *index.Manager,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	prefixSeek bool,
) (indexRangeConjunct, bool) {
	// String / prefix conjunct over a bound string btree.
	if pred, ok := extractSingleStringCmp(e, nodeVar, prefixSeek); ok {
		if sub, found := findBoundStringBTree(idxMgr, label, pred.propKey); found {
			lo, hi := boundsOf(pred.lo, pred.hi)
			count, exact := exactStringRangeCount(sub, pred)
			if exact && rangeCountWinsFn(g, label, func(b uint64) (uint64, bool) {
				if count > b {
					return b + 1, false
				}
				return count, true
			}) {
				return indexRangeConjunct{
					propKey: pred.propKey,
					count:   count,
					index: exec.IndexRangePart{
						Index: exec.NewStringRangeIndex(sub), Lo: lo, Hi: hi,
					},
				}, true
			}
		}
	}
	// Numeric conjunct over the unified float64 companion.
	if pred, ok := extractSingleNumericCmp(e, nodeVar, params); ok {
		if sub, found := findBoundNumericBTree(idxMgr, label, pred.propKey); found {
			lo, hi := rangeBoundFloats(pred)
			count, exact := sub.RangeCount(lo, hi, ^uint64(0))
			if exact && rangeCountWinsFn(g, label, func(b uint64) (uint64, bool) {
				if count > b {
					return b + 1, false
				}
				return count, true
			}) {
				loB, hiB := exec.RangeBound{}, exec.RangeBound{}
				if pred.lo != nil {
					loB = exec.RangeBound{Value: expr.FloatValue(pred.lo.value), Include: true}
				}
				if pred.hi != nil {
					hiB = exec.RangeBound{Value: expr.FloatValue(pred.hi.value), Include: true}
				}
				return indexRangeConjunct{
					propKey: pred.propKey,
					count:   count,
					index: exec.IndexRangePart{
						Index: exec.NewFloat64RangeIndex(sub), Lo: loB, Hi: hiB,
					},
				}, true
			}
		}
	}
	return indexRangeConjunct{}, false
}

// exactStringRangeCount returns the exact number of index entries the predicate's
// range covers, using the open-ended count when the upper bound is unbounded so the
// counted key space matches the space the executed scan walks (#F-CY1).
func exactStringRangeCount(sub boundStringRange, pred stringRangePred) (uint64, bool) {
	const fullBudget = ^uint64(0)
	lo := ""
	if pred.lo != nil {
		if sv, ok := pred.lo.Value.(expr.StringValue); ok {
			lo = string(sv)
		}
	}
	if pred.hi == nil {
		return sub.RangeCountFrom(lo, fullBudget)
	}
	hi := ""
	if sv, ok := pred.hi.Value.(expr.StringValue); ok {
		hi = string(sv)
	}
	return sub.RangeCount(lo, hi, fullBudget)
}

// boundsOf converts the extracted optional bounds to the operator's value form; a
// nil side becomes the zero RangeBound, which the adapters read as unbounded.
func boundsOf(lo, hi *exec.RangeBound) (exec.RangeBound, exec.RangeBound) {
	var loB, hiB exec.RangeBound
	if lo != nil {
		loB = *lo
	}
	if hi != nil {
		hiB = *hi
	}
	return loB, hiB
}

// flattenAndSpine returns the conjuncts of an arbitrarily nested AND spine.
//
// It descends ONLY through AND. Anything else — a NOT, an OR, a comparison — is
// returned as a single opaque conjunct, so a negated or disjunctive predicate can
// never be mistaken for a conjunction whose parts may be intersected
// independently. That refusal-by-default is the same discipline the label
// intersection and the range seek rely on, and it is what keeps every retained
// bound a necessary condition of the whole predicate.
func flattenAndSpine(e ast.Expression) []ast.Expression {
	bo, ok := e.(*ast.BinaryOp)
	if !ok || !equalFoldAnd(bo.Operator) {
		if e == nil {
			return nil
		}
		return []ast.Expression{e}
	}
	return append(flattenAndSpine(bo.Left), flattenAndSpine(bo.Right)...)
}
