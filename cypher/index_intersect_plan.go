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
// (label population ≥ rangeSeekMinLabelPopulation, exact in-range count ≤
// rangeSeekMaxSelectivity of it). That is deliberately conservative and needs no
// new statistic: a conjunct whose own range covers most of the label would cost
// more to probe and AND than the rows it removes, so it is left to the residual
// Filter instead of being intersected. Parts are ANDed in ascending exact-count
// order so the cheapest bitmap is materialised first, the same ordering rule #2133
// established for labels.
//
// # The gate is BUDGETED, and that is not an optimisation detail (#2266)
//
// The first cut took each conjunct's count with an INFINITE budget and compared it
// to the ceiling afterwards. That inverted the whole point of the ceiling. The
// btree's RangeCount stops accumulating the moment the running total exceeds the
// budget it is given, so a bounded count answers "more than the ceiling" in
// O(budget) probes; an unbounded one walks the entire range to produce a number
// whose only use is to be found too large. The broader the conjunct — that is, the
// more certainly it will be REFUSED — the more the refusal cost.
//
// That cost lands on the PLANNING path, so it is paid once per query rather than
// once per row, and it is paid by every shape the recogniser inspects including
// the ones it declines. It is paid TWICE per build, because indexSeekWouldFire
// runs this same peephole as a throwaway probe before the real build does. It was
// measured at 41× the plan time of a shape that composes nothing (122 µs against
// 3.0 µs) for a predicate whose third conjunct covered 95% of a 20 000-node label.
//
// The remedy needs no new constant and no new statistic, because the ceiling that
// bounds the count is the SAME ceiling the gate already compares against:
// [rangeSeekBudget] derives it once for the whole predicate, and every conjunct's
// count is taken against it. Two consequences are load-bearing:
//
//   - The population floor is now checked ONCE, up front, before any counting.
//     Below the floor no count can change the verdict, so taking one is waste by
//     construction.
//   - An accepted part's count is still EXACT, because acceptance requires
//     count ≤ budget and RangeCount early-exits only above it. The ascending-count
//     ordering of the ANDed parts is therefore byte-identical to what the
//     unbudgeted version produced. A refused conjunct's count is not exact, but a
//     refused conjunct never enters the ordering.
//
// The relationship count-store (graph/index/count) was considered first, per the
// task's preference for a maintained O(1) cell. It does not apply: it holds the
// E/D/T RELATIONSHIP statistics keyed by interned label and type ids, explicitly
// does not hold the node statistic N(label), and holds nothing at all about a
// property's value distribution — so no cell of it can answer "how many index
// entries fall in [lo, hi]". The dirty-cell hazard that would otherwise need
// handling therefore never arises here.

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
	// Cheapest possible gate, and it must come FIRST: only a top-level AND can hold
	// two conjuncts, so anything else is declined before a single allocation.
	//
	// This ordering is not cosmetic. The first cut called flattenAndSpine
	// unconditionally, and that allocates a one-element slice for every predicate it
	// is handed — including the overwhelming majority that are not conjunctions at
	// all. It showed up as a real, deterministic regression on the curated suite:
	// IC4 and IC9 went 311 → 315 allocs/op (p=0.002, ±0%), with matching B/op bumps
	// on IC2/IC3/IC7/IC11/IC14. A planner probe that runs on every Selection has to
	// be free when it does not apply.
	bo, isBinary := sel.PredicateExpr.(*ast.BinaryOp)
	if !isBinary || !equalFoldAnd(bo.Operator) {
		return nil, false
	}
	// The gate's own ceiling, derived ONCE for the whole predicate and then handed
	// to every conjunct's count so no count can run away (#2266). Two cheap checks
	// are hoisted ahead of all counting by doing it here:
	//
	//   - The population floor. Below it the answer is "decline" whatever the
	//     counts say, so a sub-floor label now costs one label-population lookup
	//     instead of a full range count per indexed conjunct.
	//   - The label population lookup itself, which interns the label string and
	//     probes the label index. It used to be repeated for every conjunct.
	//
	// This is the same budget the single-property range seeks use, from the same
	// derivation, so a conjunct admitted here is admitted on exactly the terms
	// [tryStringRangeSeek] and [tryNumericRangeSeek] would admit it on.
	budget, aboveFloor := rangeSeekBudget(g, lblScan.Label)
	if !aboveFloor {
		return nil, false
	}
	// One entry per DISTINCT property, first-recognised-bounds-win. Keeping only
	// the first conjunct per property is sound because every bound retained is a
	// necessary condition of the conjunction, so the probe stays a superset; any
	// further conjunct on the same property is simply left to the residual Filter.
	//
	// The spine is walked ITERATIVELY over stack-resident arrays, with no closure and
	// no slice handed to another function. That shape is deliberate and was arrived at
	// by measurement, in three steps: materialising the spine into a slice cost one
	// allocation per predicate inspected; hoisting that behind an AND pre-check still
	// left three, because a visitor CLOSURE capturing a growable accumulator forces
	// both to the heap; only a closure-free walk over fixed local arrays is actually
	// free. The curated suite measured each step — IC4/IC9 at 315, then 314, against a
	// 311 baseline — because this probe runs on every Selection the planner sees, so
	// "free when it does not apply" has to be literal.
	//
	// The bounds are generous for real predicates and are treated as limits, not
	// assumptions: a spine or property list deeper than these simply composes the
	// parts found so far and leaves the rest to the residual Filter, which is always
	// sound because every retained bound is a necessary condition.
	var stack [8]ast.Expression
	var partBuf [4]indexRangeConjunct
	parts := partBuf[:0]
	stack[0] = sel.PredicateExpr
	top := 1
	for top > 0 {
		top--
		e := stack[top]
		if inner, isOp := e.(*ast.BinaryOp); isOp && equalFoldAnd(inner.Operator) {
			if top+2 <= len(stack) {
				stack[top] = inner.Left
				stack[top+1] = inner.Right
				top += 2
			}
			continue
		}
		part, recognised := recogniseIndexedConjunct(e, nodeVar, lblScan.Label, idxMgr, budget, params, prefixSeek)
		if !recognised || len(parts) == len(partBuf) {
			continue
		}
		duplicate := false
		for i := range parts {
			if parts[i].propKey == part.propKey {
				duplicate = true
				break
			}
		}
		if !duplicate {
			parts = append(parts, part)
		}
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
//
// budget is the ceiling [rangeSeekBudget] derived for the label, already known to
// be above the population floor. It is BOTH the value the count is compared
// against and the early-exit limit the count is taken with, so a conjunct too
// broad to be admitted is refused after O(budget) cardinality probes instead of
// after walking its whole range (#2266).
func recogniseIndexedConjunct(
	e ast.Expression,
	nodeVar, label string,
	idxMgr *index.Manager,
	budget uint64,
	params map[string]expr.Value,
	prefixSeek bool,
) (indexRangeConjunct, bool) {
	// String / prefix conjunct over a bound string btree.
	if pred, ok := extractSingleStringCmp(e, nodeVar, prefixSeek); ok {
		if sub, found := findBoundStringBTree(idxMgr, label, pred.propKey); found {
			lo, hi := boundsOf(pred.lo, pred.hi)
			count, exact := budgetedStringRangeCount(sub, pred, budget)
			if rangeCountWithinBudget(count, exact, budget) {
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
			count, exact := sub.RangeCount(lo, hi, budget)
			if rangeCountWithinBudget(count, exact, budget) {
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

// budgetedStringRangeCount returns the number of index entries the predicate's
// range covers, EARLY-EXITING at budget: over budget it returns (budget+1, false)
// and the caller declines, having paid O(budget) cardinality probes rather than
// walking the whole range (#2266).
//
// It uses the open-ended count when the upper bound is unbounded, so the counted
// key space matches the space the executed scan walks (#F-CY1). That case is the
// one that most needs the budget: an unbounded-above range over a high-cardinality
// string index visits every remaining key in the btree.
func budgetedStringRangeCount(sub boundStringRange, pred stringRangePred, budget uint64) (uint64, bool) {
	lo := ""
	if pred.lo != nil {
		if sv, ok := pred.lo.Value.(expr.StringValue); ok {
			lo = string(sv)
		}
	}
	if pred.hi == nil {
		return sub.RangeCountFrom(lo, budget)
	}
	hi := ""
	if sv, ok := pred.hi.Value.(expr.StringValue); ok {
		hi = string(sv)
	}
	return sub.RangeCount(lo, hi, budget)
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
