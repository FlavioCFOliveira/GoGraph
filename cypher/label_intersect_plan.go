package cypher

// label_intersect_plan.go — set-at-a-time multi-label conjunction via Roaring
// bitmap intersection (#2133, planner increment R2-P2 of
// docs/design-bitmap-intersection.md).
//
// A multi-label node pattern lowers to a NodeByLabelScan on one label plus a
// residual LabelPredicate Filter re-checking the rest, per row:
//
//	MATCH (n:LabA:LabB)  →  Selection[LabelPredicate(n, [LabB])]
//	                          └─ NodeByLabelScan[LabA]
//
// The shipped min-label peephole (#2077) re-anchors that scan on the smallest
// label, which is optimal when one label is small and POWERLESS when both are
// large and their intersection is tiny — the selective case users actually write.
// Measured on |LabA| = |LabB| = 100 000 with |LabA ∩ LabB| = 100: 18.01 ms and
// 99 821 allocations to return 100 rows, because the engine materialises ~100 000
// rows to produce 100.
//
// GoGraph stores labels as Roaring bitmaps, so the conjunction is a set
// operation: one k-way AND yields exactly the 100 matching NodeIDs for 2.215 µs
// and 17 allocations. This peephole turns that existing data structure into a
// planner access path — replacing BOTH the scan and the residual Filter, since the
// intersected bitmap already encodes the conjunction.
//
// Neither Neo4j nor Memgraph does this: Neo4j's LOOKUP index gives a token scan
// then filters, and Memgraph's ScanAllByLabel then filters.
//
// # Why the residual Filter may be DROPPED (design §5)
//
// Because the label index is authoritative, which was established by measurement
// rather than assumed: it is maintained on BOTH delete and relabel (|L1 ∩ L2|
// goes 3 → 2 on RemoveNode, 2 → 1 on RemoveNodeLabel). So the bitmap decides
// label membership AND liveness, and the residual LabelPredicate re-checks
// nothing the bitmap has not already decided. Dropping it is precisely what turns
// 99 821 allocations into 17 — the win is the rows never materialised.
//
// This adds NO exposure: a plain single-label MATCH (n:L) already materialises
// its bitmap in Init and iterates it with no residual filter at all, so the
// conjunction inherits the existing contract rather than widening it.
//
// # Guards
//
//   - EXACT counts only, via the same labelCardinalityEstimate + planStaysDefault
//     trustworthiness veto the min-label peephole uses (#2076). A nil or opaque
//     resolver fails closed to today's plan.
//   - EXACT gate. roaring64.AndCardinality computes the true intersection size
//     WITHOUT materialising and WITHOUT allocating (measured 424 ns, 0 allocs), so
//     the decision is exact-or-veto with no new statistic.
//   - SMALLEST-FIRST ordering. label.Index.Intersect clones the FIRST bitmap, so
//     the AND is ordered by ascending exact cardinality: measured 6.0× faster and
//     7.5× lighter than largest-first (§4). The AND is commutative, so the answer
//     is unchanged.
//   - INDEX-SEEK precedence. Like #2077 this recognises only a bare
//     LabelPredicate Selection, and it is invoked after the equality and key-set
//     seeks, so a more selective property seek always wins.
//   - VETO FALLS THROUGH to #2077, never to something worse: the min-label
//     peephole is deliberately left in place as the fallback that makes "never
//     worse than today" true by construction.
//
// # Gating
//
// Behind EngineOptions.DisableBitmapIntersection → Engine.bitmapIntersectEnabled
// → buildOpts.bitmapIntersectEnabled (default ENABLED), a SEPARATE knob from
// DisableMinLabelScan so the differential can vary exactly one thing.

import (
	"sort"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// labelIntersectBuildCount counts how many times the planner has answered a
// multi-label conjunction by intersecting bitmaps. It is a diagnostic seam read
// only by the in-package differential test, so a green test cannot mean "the
// path never fired". Process-global and monotonic; tests snapshot it around a
// query rather than resetting it.
var labelIntersectBuildCount atomic.Uint64

// # How the gate was arrived at
//
// The first cut carried a tuned selectivity ceiling (50% of the smallest label).
// Two measurements killed it and produced the rule that replaced it.
//
// First, the ceiling declined cases the cost model says must be served:
// `MATCH (n:LabA:LabB:Tiny)` with Tiny ⊆ LabA has an intersection EQUAL to the
// smallest label, and a ratio gate vetoed it. So a ratio was the wrong shape of
// condition.
//
// Second — and this is what fixed it — removing the ceiling entirely made the
// peephole pre-empt the COLUMNAR filter chain on shapes where it had nothing to
// win. `MATCH (n:Nested:Big)` with Nested ⊂ Big is served today by a
// ColumnarFilter over a 10-row scan; the intersection also yields 10 rows, so it
// removes no rows at all and merely costs the caller column-major execution.
//
// The honest boundary is therefore a STRICT reduction in ROWS SCANNED:
// |L₁ ∩ … ∩ L_k| < min|Lᵢ|. It is exactly symmetric with the rule the columnar
// recogniser already applies to the min-label re-anchor — "columnar execution
// only removes a constant factor from each scanned row, so it must never pre-empt
// the re-anchor" (api.go) — which says a rewrite may pre-empt another only when it
// removes ROWS rather than a constant factor per row. Applied to the intersection
// that same rule sometimes cuts AGAINST it, and the gate now says so instead of
// assuming the newest peephole always wins.
//
// The count is exact and free (AndCardinality: 424 ns, zero allocations), so the
// condition is decidable without materialising anything. The EXACTNESS veto is
// retained alongside it: every participating label's cardinality must be an exact
// count (planStaysDefault, #2076), the shape must match, and at least two labels
// must participate. Every veto falls through to the shipped min-label plan.

// labelCandidate is one participating label with its exact live cardinality and
// the tie-break keys that make the ordering total.
type labelCandidate struct {
	name    string
	id      uint32
	count   int64
	synIdx  int // position in the written pattern, the final tie-break
	hasID   bool
	ordered bool // set once the candidate has been placed, for readability in tests
}

// pickLabelIntersection is the pure planner decision: it recognises the same
// bare-multi-label-LabelPredicate Selection shape pickMinLabel does, and returns
// the participating labels ordered SMALLEST-FIRST when the intersection should be
// served set-at-a-time.
//
// ok is false — meaning "fall through to the min-label peephole and then the
// default plan" — when the shape does not match, when fewer than two labels
// participate, when any cardinality is untrustworthy, or when the exact
// intersection is not selective enough to justify the path.
func pickLabelIntersection(
	sel *ir.Selection,
	labelSrc labelResolverIface,
	bmSrc exec.LabelIntersectResolver,
) (nodeVar string, ordered []string, ok bool) {
	// Reuse the min-label recogniser so the two peepholes can never disagree
	// about what shape they are looking at.
	nodeVar, scanLabel, extra, shapeOK := pickMinLabelShape(sel)
	if !shapeOK {
		return "", nil, false
	}
	names := make([]string, 0, len(extra)+1)
	names = append(names, scanLabel)
	names = append(names, extra...)
	if len(names) < 2 {
		// A single label is a plain scan; there is nothing to intersect.
		return "", nil, false
	}

	// Trustworthiness veto: every candidate's cardinality must be exact (#2076).
	ests := make([]estimate, len(names))
	for i, n := range names {
		ests[i] = labelCardinalityEstimate(labelSrc, n)
	}
	if planStaysDefault(ests...) {
		return "", nil, false
	}

	idSrc, _ := labelSrc.(labelIDResolver)
	cands := make([]labelCandidate, len(names))
	for i, n := range names {
		c := labelCandidate{name: n, count: int64(ests[i].rows), synIdx: i}
		if idSrc != nil {
			if id, found := idSrc.ResolveLabelID(n); found {
				c.id, c.hasID = id, true
			}
		}
		cands[i] = c
	}

	// SMALLEST-FIRST, with a total order so plans are stable run to run: exact
	// count, then label id, then written position. The count ordering is what
	// makes Intersect clone the cheapest bitmap (§4).
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].count != cands[b].count {
			return cands[a].count < cands[b].count
		}
		if cands[a].hasID != cands[b].hasID {
			return cands[a].hasID
		}
		if cands[a].id != cands[b].id {
			return cands[a].id < cands[b].id
		}
		return cands[a].synIdx < cands[b].synIdx
	})

	ordered = make([]string, len(cands))
	for i := range cands {
		cands[i].ordered = true
		ordered[i] = cands[i].name
	}

	// An empty label makes the conjunction provably empty: fire immediately, so
	// the plan is an empty bitmap scan rather than a full scan of a populated
	// label followed by an all-dropping filter.
	if cands[0].count == 0 {
		return nodeVar, ordered, true
	}

	// THE GATE: a STRICT reduction in rows scanned.
	//
	// The intersection is worth taking exactly when it scans strictly fewer rows
	// than the best single-label scan available — that is, when
	// |L₁ ∩ … ∩ L_k| < min|Lᵢ|. The count is exact and free (AndCardinality
	// allocates nothing), so the condition is decidable without materialising.
	//
	// Why STRICT, and why this is the honest boundary rather than a tuned ratio:
	// when the intersection equals the smallest label the min-label scan already
	// scans exactly the rows the answer needs, so there are no rows left to remove.
	// All the intersection could still save is the per-row label re-check — and the
	// columnar filter chain removes that too, without giving up column-major
	// execution. Claiming the shape in that regime would take rows away from
	// nothing and cost the caller the unboxed path: measured, `MATCH (n:Nested:Big)`
	// with Nested ⊂ Big is served by ColumnarFilter over a 10-row scan, which the
	// intersection cannot improve on.
	//
	// So the rule is symmetric with the one the columnar recogniser already applies
	// to the min-label re-anchor (api.go: "columnar execution only removes a
	// constant factor from each scanned row, so it must never pre-empt the
	// re-anchor"): a rewrite may pre-empt another only when it removes ROWS, not
	// when it removes a constant factor. Here that cuts against the intersection,
	// and the gate says so.
	//
	// A resolver that cannot produce the per-label bitmaps declines: nothing about
	// the conjunction is then known, and the shipped plan is the honest answer.
	if bmSrc == nil {
		return "", nil, false
	}
	inter, gateOK := exactIntersectionCardinality(bmSrc, ordered)
	if !gateOK {
		return "", nil, false
	}
	if inter >= uint64(cands[0].count) {
		return "", nil, false
	}
	return nodeVar, ordered, true
}

// exactIntersectionCardinality returns the EXACT size of the labels'
// intersection, computed without materialising the result where possible.
//
// For two labels that is one roaring64.AndCardinality — zero allocations. For
// three or more there is no k-way cardinality primitive, so the bound is taken
// over the two SMALLEST labels, which is sound because it is an UPPER bound:
// |L₁ ∩ … ∩ L_k| ≤ |L₁ ∩ L₂|, so a conjunction admitted on that bound can only
// shrink as the remaining labels are ANDed in (design §3.1).
//
// ok is false when the resolver cannot produce the per-label bitmaps, in which
// case the caller keeps today's plan.
func exactIntersectionCardinality(bmSrc exec.LabelIntersectResolver, ordered []string) (uint64, bool) {
	if len(ordered) < 2 {
		return 0, false
	}
	a := bmSrc.ResolveLabelsBitmap(ordered[:1])
	b := bmSrc.ResolveLabelsBitmap(ordered[1:2])
	if a == nil || b == nil {
		return 0, false
	}
	return a.AndCardinality(b), true
}

// buildLabelIntersectionIfEnabled is the gated entry point, invoked from the
// Selection case of buildOperator AFTER the equality and key-set index seeks (so
// a more selective property seek always wins) and BEFORE
// buildMinLabelScanIfEnabled (which it strictly dominates when the gate admits,
// and which remains the fallback when it vetoes).
//
// It returns (op, true, nil) when the conjunction is served set-at-a-time — a
// NodeByLabelScan over the intersected bitmap, with NO residual Filter, because
// the bitmap subsumes it — and (nil, false, nil) when the optimisation is
// disabled or the pattern is not eligible.
func buildLabelIntersectionIfEnabled(
	sel *ir.Selection,
	labelSrc labelResolverIface,
	schema map[string]int,
	bopts *buildOpts,
) (exec.Operator, bool, error) {
	if bopts == nil || !bopts.bitmapIntersectEnabled {
		return nil, false, nil
	}
	adapter := &execLabelAdapter{labelSrc: labelSrc}
	nodeVar, ordered, ok := pickLabelIntersection(sel, labelSrc, adapter)
	if !ok {
		return nil, false, nil
	}
	// Bind the node at the next free schema slot — identical to what the default
	// NodeByLabelScan build leaves, so anything above reads the same column.
	schema[nodeVar] = schemaWidth(schema)
	op := exec.NewNodeByLabelIntersectionScan(ordered, adapter)
	labelIntersectBuildCount.Add(1)
	return op, true, nil
}

// parallelIntersectionSource extends the intersection to the MORSEL-PARALLEL
// fused scan (#2133).
//
// Without it the headline query shape would not benefit at all. A bare
// `MATCH (n:A:B) RETURN n` is intercepted by [tryBuildParallelScanProject] before
// the Selection build this file's peephole hooks into, and that path anchors on
// the leaf's SINGLE label — walking all 100 000 members of one label in parallel
// and re-checking the rest per row in each worker. Parallelising the waste is
// still the waste: measured, the plain projection form stayed on
// ParallelScanProject while only the aggregate form picked up the intersection.
//
// So when the parallel leaf's optional Selection is exactly the bare multi-label
// LabelPredicate this file recognises, and the same exact gate admits, the row
// SOURCE becomes the intersected bitmap and the caller DROPS the Selection —
// because the bitmap subsumes it (§5). Workers then divide ~100 rows between them
// instead of ~100 000.
//
// It deliberately calls [pickLabelIntersection] rather than re-deriving the
// shape: in the parallel path sel.Child IS the NodeByLabelScan leaf, so the same
// recogniser and the same gate apply verbatim and the two entry points cannot
// diverge.
//
// ok is false when the optimisation is disabled, the shape does not match, the
// Selection belongs to a different variable, or the gate vetoes — in every case
// the caller keeps its existing single-label source and its Selection.
func parallelIntersectionSource(
	sel *ir.Selection,
	leafVar string,
	labelSrc labelResolverIface,
	bopts *buildOpts,
) (walker *lpgLabelWalker, card uint64, ok bool) {
	if bopts == nil || !bopts.bitmapIntersectEnabled || sel == nil {
		return nil, 0, false
	}
	nodeVar, ordered, picked := pickLabelIntersection(sel, labelSrc, &execLabelAdapter{labelSrc: labelSrc})
	if !picked || nodeVar != leafVar {
		return nil, 0, false
	}
	return newLabelIntersectionWalker(ordered, labelSrc)
}

// newLabelIntersectionWalker builds a morsel source over the INTERSECTION of
// names, the k-way counterpart of [newLabelWalker]. names must already be ordered
// smallest-first by the planner (see [LabelIntersectResolver]).
func newLabelIntersectionWalker(names []string, labelSrc labelResolverIface) (*lpgLabelWalker, uint64, bool) {
	if labelSrc == nil || len(names) < 2 {
		return nil, 0, false
	}
	isect, isIntersect := labelSrc.(exec.LabelIntersectResolver)
	if !isIntersect {
		return nil, 0, false
	}
	bm := isect.ResolveLabelsBitmap(names)
	if bm == nil {
		return nil, 0, false
	}
	card := bm.GetCardinality()
	return &lpgLabelWalker{bm: bm, card: int(card)}, card, true
}

// compile-time assertions that the production resolvers satisfy the intersection
// contract, so a signature drift is a build error rather than a silent fallback
// to the scan-and-filter plan.
var (
	_ exec.LabelIntersectResolver = (*lpgLabelResolver)(nil)
	_ exec.LabelIntersectResolver = (*execLabelAdapter)(nil)
	_                             = roaring64.New
)
