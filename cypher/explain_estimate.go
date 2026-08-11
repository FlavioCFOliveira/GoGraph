package cypher

// explain_estimate.go — cardinality-estimate annotations for the physical EXPLAIN
// rendering (task #2099, design docs/optimizer-activation-design.md §2.1 and
// docs/statistics-design.md). It turns the estimate-provenance providers that
// shipped INERT in #2076 / #2083 / #2097 / #2098 (labelCardinalityEstimate,
// degreeCardinalityEstimate, statsEqualityEstimate, statsRangeEstimate) into a
// USEFUL, OBSERVABLE planner signal by annotating each operator line of
// [Engine.Explain]'s rendered plan with an estimated row count and its
// provenance tag (exact / stats / heuristic).
//
// # Display-only
//
// Nothing here touches execution, results, or plan choice: it is consulted only
// by the EXPLAIN renderer ([explainWithIndexesNode]) to append a suffix to a
// line that would be printed anyway. The estimate providers remain the sole
// authority on the numbers, and the trustworthiness classification
// ([estimate.trustworthy]) still governs which plan the READ path builds — that
// path is unchanged. An absent, dirty, or stale statistic yields an estFallback
// estimate, which this module renders as NO annotation (never a fabricated
// exact), so a reader can always trust-rank what they see.
//
// # Snapshot consistency
//
// The estimate reads follow the established EXPLAIN precedent (the reorder /
// anchor-swap / min-label peepholes in [Engine.Explain]): they read live, not
// under a View barrier, because EXPLAIN is a diagnostic that executes nothing —
// there is no query the numbers must be consistent WITH, and a consistent
// snapshot is not required for a rendering (see the comment in [Engine.Explain]).
// Each provider read is individually lock-free and tear-free (the count store's
// atomics, the statistics Collector's atomic-pointer snapshot, the label index
// cardinality, and [lpg.Graph.LiveOrder]). The Run path still consults these same
// providers under its View, exactly as their design mandates.

import (
	"fmt"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// estimateAnnotation renders a trustworthy estimate as a plan-line suffix, or the
// empty string when the estimate is not trustworthy (estFallback / an unvalidated
// estHeuristic still carries a number, but only estExact/estStats/estHeuristic
// are shown — estFallback is omitted so no fabricated exact is ever printed).
//
// The forms are:
//
//	(est. rows=42, exact)       an exact, maintained count
//	(est. rows~17, heuristic)   a principled formula (1/NDV * N) — approximate
//
// A leading space is included so the caller appends it directly. estStats is
// rendered by [estimateAnnotationWithError], which also prints the certified
// error term.
func estimateAnnotation(e estimate) string {
	switch e.source {
	case estExact:
		return fmt.Sprintf(" (est. rows=%d, exact)", estRows(e.rows))
	case estStats:
		// A stats estimate without an explicit error term still prints its tag;
		// callers with the certified error use estimateAnnotationWithError.
		return fmt.Sprintf(" (est. rows~%d, stats)", estRows(e.rows))
	case estHeuristic:
		return fmt.Sprintf(" (est. rows~%d, heuristic)", estRows(e.rows))
	default: // estFallback — absent or stale statistic; omit rather than fabricate.
		return ""
	}
}

// estimateAnnotationWithError renders a range estimate, appending the certified
// absolute selectivity error term (delta = 1/B + Delta/N, design
// docs/statistics-design.md §3) for an estStats verdict. A demoted (estFallback)
// range estimate is omitted, exactly like [estimateAnnotation].
func estimateAnnotationWithError(e estimate, absErr float64) string {
	if e.source == estStats {
		return fmt.Sprintf(" (est. rows~%d, stats, err=%.4f)", estRows(e.rows), absErr)
	}
	return estimateAnnotation(e)
}

// estRows rounds a float estimate to the nearest non-negative integer for
// display. Row counts are conceptually whole; the providers already clamp to
// [0, N], so a negative value cannot arise, but the guard keeps the render total.
func estRows(rows float64) int64 {
	if rows < 0 || math.IsNaN(rows) {
		return 0
	}
	return int64(math.Round(rows))
}

// scanEstimateAnnotation returns the estimate suffix for a leaf scan operator:
// AllNodesScan renders the exact total live-node count ([lpg.Graph.LiveOrder]),
// and NodeByLabelScan the exact live count for its label (labelCardinalityEstimate).
// Both are estExact. A nil resolver or graph yields no annotation.
func scanEstimateAnnotation(plan ir.LogicalPlan, labelSrc *lpgLabelResolver, g *lpg.ReadView[string, float64]) string {
	switch p := plan.(type) {
	case *ir.AllNodesScan:
		if g == nil {
			return ""
		}
		return estimateAnnotation(estimate{rows: float64(g.LiveOrder()), source: estExact})
	case *ir.NodeByLabelScan:
		if labelSrc == nil {
			return ""
		}
		return estimateAnnotation(labelCardinalityEstimate(labelSrc, p.Label))
	default:
		return ""
	}
}

// labelScanAnnotation returns the exact live-count suffix for a NodeByLabelScan
// on a specific label. It is used both for the general scan leaf and for the
// min-label-rewritten leaf (which scans the chosen smaller label).
func labelScanAnnotation(labelSrc *lpgLabelResolver, label string) string {
	if labelSrc == nil {
		return ""
	}
	return estimateAnnotation(labelCardinalityEstimate(labelSrc, label))
}

// selectionEstimateAnnotation returns the estimate suffix for a Selection whose
// child is a scan leaf, derived from the predicate shape:
//
//   - An equality n.prop = literal → the exact per-value MCV count when the
//     literal is a tracked heavy hitter (estExact), else the 1/NDV * N average
//     (estHeuristic).
//   - A single range comparison n.prop <op> x → the equi-depth histogram estimate
//     (estStats) with its certified error, or estFallback (omitted) when the
//     statistic is absent or stale.
//
// It returns the empty string for any other predicate shape (a bare label
// predicate, a compound AND/OR, an IS NULL, …) or a child that is not a scan
// leaf — the honest "no derivable estimate" case.
func selectionEstimateAnnotation(sel *ir.Selection, labelSrc *lpgLabelResolver, params map[string]expr.Value) string {
	if sel.PredicateExpr == nil || labelSrc == nil {
		return ""
	}
	nodeVar, label, isScan := scanLeafNodeVar(sel.Child)
	if !isScan {
		return ""
	}
	// Equality predicate: MCV-exact or 1/NDV heuristic.
	if prop, lit, ok := extractEqFromAST(sel.PredicateExpr, nodeVar, params); ok && lit != nil {
		return estimateAnnotation(statsEqualityEstimate(labelSrc, label, prop, lit))
	}
	// Single range comparison: equi-depth histogram estimate + certified error.
	if prop, op, bound, ok := extractRangeComparison(sel.PredicateExpr, nodeVar, params); ok {
		e, absErr := statsRangeEstimate(labelSrc, label, prop, op, bound)
		return estimateAnnotationWithError(e, absErr)
	}
	return ""
}

// extractRangeComparison decomposes a single comparison predicate
// (nodeVar.prop <op> x, or its mirror x <op> nodeVar.prop) into the property key,
// the equivalent [stats.Op], and the bound value, for op ∈ {<, <=, >, >=}. It
// mirrors the operator when the property is on the right so the returned op is
// always oriented as "prop <op> bound". ok is false for any other shape,
// including a two-sided AND range (a single-op histogram estimate is not defined
// for it here) — that case falls through to no annotation.
func extractRangeComparison(e ast.Expression, nodeVar string, params map[string]expr.Value) (prop string, op stats.Op, bound expr.Value, ok bool) {
	bo, isBO := e.(*ast.BinaryOp)
	if !isBO {
		return "", 0, nil, false
	}
	var sop stats.Op
	switch bo.Operator {
	case "<":
		sop = stats.OpLt
	case "<=":
		sop = stats.OpLe
	case ">":
		sop = stats.OpGt
	case ">=":
		sop = stats.OpGe
	default:
		return "", 0, nil, false
	}
	// Property on the left: prop <op> bound.
	if pk, isProp := nodePropKey(bo.Left, nodeVar); isProp {
		if v, err := astLiteralToValue(bo.Right, params); err == nil && v != nil {
			return pk, sop, v, true
		}
		return "", 0, nil, false
	}
	// Property on the right: bound <op> prop ≡ prop <op'> bound.
	if pk, isProp := nodePropKey(bo.Right, nodeVar); isProp {
		if v, err := astLiteralToValue(bo.Left, params); err == nil && v != nil {
			return pk, mirrorStatsOp(sop), v, true
		}
	}
	return "", 0, nil, false
}

// mirrorStatsOp reverses a comparison operator's sense, used when the property
// operand sits on the right of the comparison (x < n.p ≡ n.p > x).
func mirrorStatsOp(op stats.Op) stats.Op {
	switch op {
	case stats.OpLt:
		return stats.OpGt
	case stats.OpLe:
		return stats.OpGe
	case stats.OpGt:
		return stats.OpLt
	case stats.OpGe:
		return stats.OpLe
	default:
		return op
	}
}

// expandEstimateAnnotation returns the estimate suffix for an Expand: the exact
// count-store degree D(label, relType, dir) — the expected total number of rows
// the expansion emits when driven by every node of the source label — when the
// expand has exactly one relationship type, a directed traversal (outgoing or
// incoming), and a resolvable source label. A dirty D cell yields estFallback
// (omitted), and any unresolvable shape (multi-type, undirected, unknown source
// label) yields no annotation.
func expandEstimateAnnotation(exp *ir.Expand, labelSrc *lpgLabelResolver) string {
	if labelSrc == nil || len(exp.RelTypes) != 1 {
		return ""
	}
	var dir count.Direction
	switch exp.Direction {
	case ir.DirectionOutgoing:
		dir = count.Out
	case ir.DirectionIncoming:
		dir = count.In
	default:
		// Undirected (DirectionBoth) has no single D cell; omit.
		return ""
	}
	label, ok := expandFromLabel(exp)
	if !ok {
		return ""
	}
	return estimateAnnotation(degreeCardinalityEstimate(labelSrc, label, exp.RelTypes[0], dir))
}

// expandFromLabel finds the label of the Expand's source node by descending the
// child subplan (through any residual Selection wrappers) to the NodeByLabelScan
// that binds FromVar. ok is false when the source is not a single labelled scan
// (an unlabelled AllNodesScan, a chained expand, a join, …) — in which case no
// single-label degree estimate is defined.
func expandFromLabel(exp *ir.Expand) (string, bool) {
	child := exp.Child
	for {
		switch c := child.(type) {
		case *ir.NodeByLabelScan:
			if c.NodeVar == exp.FromVar {
				return c.Label, true
			}
			return "", false
		case *ir.Selection:
			child = c.Child
		default:
			return "", false
		}
	}
}

// rangeSeekInRangeCount recomputes the EXACT in-range index count for a Selection
// that the range-seek peephole (#1505) chose to serve as a NodeByIndexRangeScan.
// It reuses the same predicate extraction and covering-btree lookup the seek
// builder uses, then re-runs the exact count with an unbounded budget so the
// returned value is the true in-range cardinality (not the early-exit gate count).
// It is called ONLY when the seek has already fired, so the range is provably
// selective (≤ rangeSeekMaxSelectivity of the label) and the count is cheap. ok
// is false only if the shape is not recoverable, in which case the leaf renders
// without an annotation.
// prefixSeek must carry the same STARTS WITH gate the build used, so the shape
// recovered here is the shape that actually fired (#2127).
func rangeSeekInRangeCount(sel *ir.Selection, idxMgr *index.Manager, g *lpg.ReadView[string, float64], params map[string]expr.Value, prefixSeek bool) (int64, bool) {
	if idxMgr == nil || g == nil || sel.PredicateExpr == nil {
		return 0, false
	}
	lblScan, ok := sel.Child.(*ir.NodeByLabelScan)
	if !ok || lblScan.Label == "" {
		return 0, false
	}
	const fullBudget = ^uint64(0) // never early-exit: count the whole (selective) range.

	// String range over a bound string btree.
	if pred, okPred := extractStringRangePred(sel.PredicateExpr, lblScan.NodeVar, params, prefixSeek); okPred {
		if sub, okSub := findBoundStringBTree(idxMgr, lblScan.Label, pred.propKey); okSub {
			lo := ""
			if pred.lo != nil {
				if sv, okSv := pred.lo.Value.(expr.StringValue); okSv {
					lo = string(sv)
				}
			}
			if pred.hi != nil {
				hi := ""
				if sv, okSv := pred.hi.Value.(expr.StringValue); okSv {
					hi = string(sv)
				}
				if cnt, exact := sub.RangeCount(lo, hi, fullBudget); exact {
					return int64(cnt), true
				}
			} else if cnt, exact := sub.RangeCountFrom(lo, fullBudget); exact {
				return int64(cnt), true
			}
		}
	}

	// Numeric range over the unified float64 btree companion.
	if pred, okPred := extractNumericRangePred(sel.PredicateExpr, lblScan.NodeVar, params); okPred {
		if sub, okSub := findBoundNumericBTree(idxMgr, lblScan.Label, pred.propKey); okSub {
			lo, hi := rangeBoundFloats(pred)
			if cnt, exact := sub.RangeCount(lo, hi, fullBudget); exact {
				return int64(cnt), true
			}
		}
	}
	return 0, false
}

// rangeSeekLeafAnnotation renders the exact in-range count for a rewritten
// NodeByIndexRangeScan leaf as an estExact estimate, or the empty string when the
// count cannot be recovered.
func rangeSeekLeafAnnotation(sel *ir.Selection, idxMgr *index.Manager, g *lpg.ReadView[string, float64], params map[string]expr.Value, prefixSeek bool) string {
	if cnt, ok := rangeSeekInRangeCount(sel, idxMgr, g, params, prefixSeek); ok {
		return estimateAnnotation(estimate{rows: float64(cnt), source: estExact})
	}
	return ""
}
