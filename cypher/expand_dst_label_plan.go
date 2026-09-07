package cypher

// expand_dst_label_plan.go — pushing a far-endpoint label INTO the expansion
// (rmp #2629).
//
// # The defect
//
// `MATCH (:USER)-[:FRIEND]->(:USER) RETURN count(*)` spent most of its time in a
// per-row filter that re-checked the DESTINATION node's label. The reason is in
// the IR translator, not in a cost decision: [ir.Expand] carries no far-endpoint
// predicate at all (its fields are the child, the three variables, the path
// variable, the relationship types, the sibling relationships, the direction and
// IntoVar), and `matchApplyNodeFilter` in cypher/ir/match.go unconditionally
// wraps the freshly built Expand in
//
//	Selection{LabelPredicate(toVar, labels)}
//
// for every destination node pattern that carries a label. So the label was not
// "not pushed because the planner declined" — there was nowhere to push it TO,
// and nothing that would have pushed it.
//
// Measured at HEAD (2d8bb363), 2 000 :USER nodes, 79 172 :FRIEND edges, one
// process, no profiler: the plan was
//
//	CountRows → ColumnarFilter(rows=79172, removed=0) → columnarExpand → NodeByLabelScan[USER]
//
// and the filter removed NOTHING. A CPU profile of that query attributes 28.7% of
// all samples to the label predicate and a further 26.4% to
// [exec.Chunk.AppendRowFrom] — the filter compacting 79 172 surviving rows from
// the expansion's chunk into its own, so that a `count(*)` which reads no column
// could count them.
//
// # What this does
//
// It gives the destination label a place to live inside the traversal
// ([exec.DstAdmit]) and pushes it there for the one shape where doing so is
// provably neutral in every respect but cost: a SINGLE bare label predicate on
// the Expand's own ToVar, standing alone above the Expand. The Selection is then
// not built at all, so on the target shape the plan loses an operator:
//
//	CountRows → columnarExpand(dst :USER) → NodeByLabelScan[USER]
//
// # Why answer-equivalence is by construction and not by review
//
// The gate closure calls [lpg.ReadView.HasNodeLabelByID] — the SAME accessor both
// forms of the predicate it replaces resolve to. The columnar fast path
// ([makeColumnarLabelPredicate]) calls it directly; the boxed row fallback
// reaches it through [expr.LazyNodeValue.HasLabel] → [lazyNodeResolver.HasNodeLabel].
// One accessor, one snapshot, one answer.
//
// It deliberately does NOT use the label index bitmap, which would be faster
// still: [lpg.Graph.LabelBitmapAsOf] additionally requires the node to be LIVE as
// of the snapshot, where HasNodeLabelByID tests only the label bag. The two agree
// on every destination reachable through a live edge, but "every destination
// reachable through a live edge" is an argument, and this optimisation is not
// worth one. The bitmap variant is left measured and unclaimed.
//
// # Why only ONE predicate
//
// Pushing a predicate into the expansion moves it EARLIER than every predicate
// that stays behind. For pure predicates that cannot change the result multiset —
// a conjunction of side-effect-free tests is commutative — but it can change
// which rows a later predicate is evaluated on at all, and therefore whether a
// later predicate's runtime error is reachable. Restricting the rewrite to a
// lone predicate removes the question instead of answering it, and covers the
// reported shape exactly: `(:A)-[:T]->(:B)` puts the source label in the scan
// leaf and leaves the destination label as the only Selection.
//
// # Gating
//
// Behind EngineOptions.DisableExpandLabelPush → Engine.expandLabelPushEnabled →
// buildOpts.expandLabelPushEnabled (default ENABLED), mirroring
// DisableMinLabelScan / DisableAnchorSwap. The knob exists so the differential
// test can obtain BOTH plans for one query; there is no other way to see the
// pre-change answer.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// expandDstLabelPushCount counts how many times a destination label has been
// pushed into an expansion instead of built as a Selection above it. Tests assert
// on it so an eligibility change cannot silently stop the rewrite firing — the
// runtime counter, never another rendering of the plan.
//
// Process-wide diagnostic counter, matching labelledHopRewriteCount and
// exec.ExpandIntoSeekCount.
var expandDstLabelPushCount atomic.Uint64

// ExpandDstLabelPushCount reports how many times the far-endpoint label push
// (rmp #2629) has fired since process start. It is a diagnostic seam for tests
// that must prove the rewrite actually happened — an EXPLAIN line proves the
// plan shape, this proves the decision — and for operational observability.
// Process-global and monotonic; callers snapshot it before and after a query
// rather than resetting it.
func ExpandDstLabelPushCount() uint64 { return expandDstLabelPushCount.Load() }

// expandDstLabels reports the labels pe requires of exp's destination, when pe is
// EXACTLY a bare label predicate on that destination variable and nothing else.
//
// It is deliberately as narrow as [matchAnchorSite]'s equivalent test: a
// LabelPredicate whose receiver is a plain [ast.Variable] naming exp.ToVar. A
// predicate over any other receiver, a conjunction, a property test, or a
// predicate on a hop whose destination is already bound (IntoVar non-empty, where
// ToVar names a synthetic column an equality Selection also reads) is refused.
func expandDstLabels(pe ast.Expression, exp *ir.Expand) ([]string, bool) {
	if exp == nil || exp.ToVar == "" || exp.IntoVar != "" {
		return nil, false
	}
	lp, isLP := pe.(*ast.LabelPredicate)
	if !isLP || len(lp.Labels) == 0 {
		return nil, false
	}
	recv, isVar := lp.Receiver.(*ast.Variable)
	if !isVar || recv.Name != exp.ToVar {
		return nil, false
	}
	return lp.Labels, true
}

// makeExpandDstLabelAdmit returns the [exec.DstAdmit] enforcing a conjunction of
// labels on an expansion's destination node id.
//
// Every label is tested with [lpg.ReadView.HasNodeLabelByID], which is the
// accessor both the columnar and the boxed form of the Selection this replaces
// resolve to — see the file comment. That is what makes the gate and the
// Selection answer-identical rather than merely intended to be.
//
// The names are copied so a later mutation of the AST slice cannot change the
// built gate, exactly as [makeColumnarLabelPredicate] copies them.
func makeExpandDstLabelAdmit(labels []string, g *lpg.ReadView[string, float64]) exec.DstAdmit {
	if g == nil || len(labels) == 0 {
		return nil
	}
	if len(labels) == 1 {
		// The overwhelmingly common case, kept off the loop so the gate is one
		// call and no range setup per slot.
		name := labels[0]
		return func(dst graph.NodeID) bool { return g.HasNodeLabelByID(dst, name) }
	}
	want := make([]string, len(labels))
	copy(want, labels)
	return func(dst graph.NodeID) bool {
		for _, name := range want {
			if !g.HasNodeLabelByID(dst, name) {
				return false
			}
		}
		return true
	}
}
