package cypher

// anchor_swap_plan.go — the single-edge anchor-swap peephole (task #2090, the
// P-anchor peephole of docs/reordering-design.md §2), built on the result-
// identical expand reversal (task #2089, §6) and gated by the #2089a
// reverse-expand cost measurement (§5, §5.1). It is the first REAL consumer of
// the count-store's D(label, relType, dir) degree statistics.
//
// # What it does
//
// For a single-edge pattern whose written form anchors (scans) one endpoint and
// expands to the other, it may instead anchor the OTHER endpoint and traverse
// the SAME relationship from there, when the count-store says that examines
// fewer edges. For `MATCH (a:A)<-[:R]-(b:B)` the written plan is
//
//	Selection[(a:A)? no — (b:B)] → Expand{from:a, R, Incoming, to:b} → NodeByLabelScan[a:A]
//
// i.e. scan A, then walk each a's INCOMING R-edges (D(A,R,IN) of them). The swap
// re-roots onto b:
//
//	Selection[(a:A)] → Expand{from:b, R, Outgoing, to:a} → NodeByLabelScan[b:B]
//
// i.e. scan B, then walk each b's OUTGOING R-edges (D(B,R,OUT) of them). The two
// produce the identical (a, b, r) multiset — it is precisely the plan the IR
// translator emits for the openCypher-equivalent mirror pattern `(b:B)-[:R]->(a:A)`
// (see [reverseSingleEdgeDir]); only emission order changes, and that is proven
// unobserved by [SuppressReorder].
//
// # SYMMETRIC since #2150 (was OUT-ward only — the #2089a verdict, §5.1)
//
// The swap now fires in BOTH directions: it may flip a written DirIn expand to
// DirOut, and — since #2150 — a written DirOut expand to DirIn. Undirected (Both)
// patterns remain vetoed, because their mirror is also Both (the swap would
// re-anchor without changing the traversal) and D(label, relType, BOTH) is not a
// modelled cell.
//
// WHY IT WAS OUT-WARD ONLY, and what changed. #2089a measured that a DirIn expand
// pays a per-in-edge cost of Θ(out-degree of that edge's SOURCE): the reverse path
// had to SCAN the source's whole forward out-range to recover the canonical edge id
// (Expand.lookupFwdEdgePos), plus a second such scan under a type filter
// (Expand.reverseEdgePassesFilter). That overhead is invisible to the aggregate
// D(label,relType,dir), so a reverse-INTRODUCING swap could be slower than the
// written order and could not be faithfully costed. An OUT-ward swap instead
// REMOVES the reverse overhead, so the modelled win was a lower bound on the real
// win — no regression. The reverse-CSR build cost is O(V+E) but paid
// unconditionally on every expand (csrPairFromGraph builds both directions), so it
// cancels between the two anchors.
//
// #2142 made both lookups binary searches over a destination-ordered run, and that
// is a change in KIND, not merely in magnitude. An unmodelled Θ(out-degree) term
// can exceed any constant margin; a Θ(log out-degree) term cannot. #2150 measured
// the residual end-to-end on this machine — an in-edge costs
// 2.01 ns + 5.76 ns·log₂(d) more than an out-edge, i.e. 3% at degree 4 rising to
// only 15% at degree 1024, against a margin that already demands 100% headroom.
//
// The restriction is therefore lifted, and the reverse direction is made SOUND
// rather than merely cheap. The probe term is modelled explicitly
// (anchorSwapProbeCost × [probeDepthEstimate]), and the no-regression guarantee
// comes from a STRUCTURAL bound rather than from that estimate being tight: a probe
// depth cannot exceed 64 levels, so the per-in-edge penalty is at most
// 12·64 + 4 = 772 < 800 = anchorSwapEdgeCost. An in-edge therefore truly costs less
// than twice a modelled out-edge, for any graph, and a margin that already demands a
// 2× modelled win absorbs it. See [probeDepthEstimate] for the full chain. This is
// what "a cost model that carries the residual log term" required; the observation
// that the term got smaller would not have been enough on its own.
//
// # The admissibility gate (design §1, §2)
//
// A swap is admitted only when ALL hold, with the count-store read from ONE
// snapshot (the read-path build runs under lpg.Graph.View's visibility barrier,
// which is exclusive against a committing writer, so every count and its dirty
// flag are consistent):
//
//   - Directed: the written expand is DirIn (Incoming) or DirOut (Outgoing).
//     Undirected (Both) is vetoed.
//   - Exactly one relationship type: D is a per-type statistic; an any-type or
//     multi-type expand has no single exact D cell → not a candidate.
//   - Every cost input EstExact ∧ ¬dirty. Both node counts N(fromLabel),
//     N(toLabel) (always exact), plus the two degree cells the direction needs:
//     D(fromLabel,R,IN) and D(toLabel,R,OUT) for an OUT-ward swap, or
//     D(fromLabel,R,OUT) and D(toLabel,R,IN) for an IN-ward one. A dirty D cell
//     (a relabel dirties the IN family — count-store design §3.3.1) yields
//     EstFallback, which the trustworthiness veto rejects. This is where the
//     count-store's dirty-veto is first exercised.
//
//     The IN-ward direction needs D(fromLabel,R,OUT) TWICE — as the written cost
//     AND as the probe-depth input — so the same veto covers both; without an
//     exact, non-dirty from-side out-degree the probe depth is unknown and the
//     candidate is not costable at all.
//   - Order-safe: SuppressReorder(spine) == false (§4), reusing the #2092
//     predicate. Baked into the candidate set at parse time.
//   - Strict cost win under the margin: modeledCost(anchor-B)·margin ≤
//     modeledCost(anchor-A) and strictly smaller, with cost = c_s·N + c_e·D.
//
// # Gating
//
// Behind EngineOptions.DisableAnchorSwap → Engine.anchorSwapEnabled →
// buildOpts.anchorSwap (default ENABLED), mirroring DisableHashJoin /
// DisableMinLabelScan / DisableJoinReorder. Only the read path (Engine.Run)
// populates the swap map; every other build path leaves it nil, so the peephole
// is inert on writes and whenever a count is not exact.

import (
	"math"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
)

// The IN-ward swap's no-regression proof rests on ONE inequality: the largest
// possible per-in-edge probe penalty must stay below one edge's modelled cost, so
// that a true in-edge costs under twice a modelled out-edge and the 2× margin
// absorbs it (see [probeDepthEstimate]). A binary search over a 64-bit-addressable
// adjacency has depth at most 64, so the requirement is
//
//	anchorSwapProbeCost·64 + anchorSwapProbeBase < anchorSwapEdgeCost
//
// This declaration FAILS TO COMPILE if that ever stops holding: the conversion of a
// negative untyped constant to uint is a compile-time error. It is here rather than
// in a test because a test can be skipped and a broken cost model would then admit
// swaps that regress.
const _ = uint(anchorSwapEdgeCost - (anchorSwapProbeCost*64 + anchorSwapProbeBase) - 1)

// anchorSwapBuildCount counts how many times the planner has re-rooted a
// single-edge pattern onto its other endpoint. It is a diagnostic seam read only
// by the in-package differential test to assert the swap fired (or, under a
// guard, did not). Process-global and monotonic; tests snapshot it before/after
// a query rather than resetting it.
var anchorSwapBuildCount atomic.Uint64

const (
	// anchorSwapNodeCost is c_s, the modeled cost of scanning one node.
	// anchorSwapEdgeCost is c_e, the modeled cost of examining one edge. #2089a
	// measured a node scan at α ≈ 1.8 ns and an OUT edge at β_out ≈ 19 ns on
	// Apple M4, so an examined edge is ~an order of magnitude costlier than a
	// scanned node. The RATIO (≈ 1 : 8 here, rounded conservatively toward the
	// scan so the candidate's scan term is never under-credited), not the
	// absolute values, is what the no-regression argument rests on; the margin
	// below absorbs cross-machine ratio drift. See docs/reordering-design.md §5.1.
	// The pair is scaled by 100 against the 1 : 8 originally shipped so the
	// per-in-edge probe term (#2150, anchorSwapProbeCost) stays a legible small
	// integer beside them rather than a fraction. Cost is linear and the margin is
	// multiplicative, so scaling BOTH terms identically leaves every OUT-ward
	// decision unchanged — which P3's regression tests re-verify.
	anchorSwapNodeCost = 100
	anchorSwapEdgeCost = 800
	// anchorSwapProbeCost is the modelled cost, per in-edge and per level of probe
	// depth, of recovering an in-edge's canonical forward position. A DirIn
	// traversal probes the source's forward range TWICE per in-edge — once in
	// Expand.reverseEdgePassesFilter and once in Expand.lookupFwdEdgePos[ByHandle]
	// — and both are binary searches since #2142, so the cost is
	// 2·c_p·log₂(out-degree of that edge's source). This constant folds in the
	// factor 2: it is charged once per level, for the pair.
	//
	// MEASURED at HEAD on Apple M4 (#2150). Fixture: n=4000 A-nodes and 4000
	// B-nodes, a_i → b_{(i+j) mod n} for j < d, so out-degree(a) = in-degree(b) = d
	// and BOTH `MATCH (a:A)-[:R]->(b:B)` and `MATCH (b:B)<-[:R]-(a:A)` scan n nodes
	// and walk n·d edges — the only difference is the reverse path. Sweeping
	// d ∈ {4, 16, 64, 256, 1024} and fitting the DIFFERENCE of the two arms'
	// ns-per-edge against log₂(d) — the difference, because per-edge cost grows with
	// d from cache pressure in BOTH arms and that growth would otherwise be
	// attributed to the probe — gives
	//
	//	in-edge penalty = 2.01 ns + 5.76 ns · log₂(d)
	//
	// against a marginal out-edge cost of ≈400 ns (the slope of total time against
	// edge count at the top of the sweep). Relative to anchorSwapEdgeCost = 800 that
	// is 5.76/400·800 = 11.5 per level, rounded UP to 12, and an intercept of
	// 2.01/400·800 = 4.0. Rounding up is the safe direction: it over-charges the
	// reverse path, so a modelled win understates the real one.
	//
	// Do NOT carry #2089a's β_out ≈ 19 ns forward. It predates #2142 and measured
	// the edge walk alone, whereas these constants are calibrated end-to-end, which
	// is the regime the planner's decision actually lands in.
	anchorSwapProbeCost = 12
	// anchorSwapProbeBase is the direction-independent part of the in-edge penalty
	// (the 4.0 above): the reverse path's bookkeeping that does not scale with probe
	// depth.
	anchorSwapProbeBase = 4
	// anchorSwapMargin is the constant-factor guard (design §1.5): the swap fires
	// only when the candidate's modeled cost times this margin still does not
	// exceed the written-order cost. 2 means "fire only on a ≥2× modeled win",
	// which keeps the peephole to clear wins (the hub cases) and leaves marginal
	// cases in written order, so a wrong cost ratio cannot turn a modeled win into
	// a real regression.
	anchorSwapMargin = 2
)

// anchorSite is a structurally-matched single-edge pattern eligible for the
// anchor swap. It holds pointers INTO the immutable cached plan (stable across
// the plan's lifetime) plus the resolved endpoint/relationship names.
type anchorSite struct {
	topSel    *ir.Selection       // Selection{LabelPredicate(toVar,[toLabel])}
	exp       *ir.Expand          // the single-edge Expand (its Child is scan)
	scan      *ir.NodeByLabelScan // NodeByLabelScan{fromVar, fromLabel}
	fromVar   string
	fromLabel string
	toVar     string
	toLabel   string
	relType   string
}

// matchAnchorSite recognises the single-edge shape
//
//	Selection{LabelPredicate(toVar,[toLabel])}
//	  Expand{FromVar:fromVar, RelTypes:[relType], To:toVar}
//	    NodeByLabelScan{fromVar, fromLabel}
//
// and returns the resolved site. It matches for ANY expand direction — the
// direction restriction (OUT-ward only) is applied later by
// [computeAnchorSwaps], so the same matcher serves the general reversal (#2089)
// and the gated peephole (#2090). It declines (ok=false) any shape that is not a
// standalone, single-label, single-type, single-hop pattern:
//
//   - the top must be a single-label LabelPredicate on the expand's ToVar (a
//     property Selection or a multi-label predicate above the expand → decline,
//     so the swap never has to relocate extra endpoint constraints);
//   - BOTH endpoints must be NAMED: an anonymous pattern head has the empty
//     variable name, which no relocated label predicate can read (rmp #2603 — see
//     the guard's comment in the body);
//   - the expand must carry exactly one relationship type (D is per-type), have
//     no named-path variable (reversing a path changes its node order → not
//     result-identical), and no sibling relationship variables (it must be the
//     first and only hop);
//   - the expand's child must be a bare single-label NodeByLabelScan of the
//     expand's FromVar (a property/extra-label Selection below → decline).
func matchAnchorSite(sel *ir.Selection) (anchorSite, bool) {
	lp, ok := sel.PredicateExpr.(*ast.LabelPredicate)
	if !ok || len(lp.Labels) != 1 {
		return anchorSite{}, false
	}
	toVarExpr, ok := lp.Receiver.(*ast.Variable)
	if !ok {
		return anchorSite{}, false
	}
	exp, ok := sel.Child.(*ir.Expand)
	if !ok {
		return anchorSite{}, false
	}
	if exp.ToVar != toVarExpr.Name {
		return anchorSite{}, false
	}
	// BOTH endpoints must be NAMED (rmp #2603). The reversal relocates the
	// from-label from the access path onto a Selection predicate, and a predicate
	// can only reach a node THROUGH ITS NAME — whereas a NodeByLabelScan enforces
	// the same label structurally, with no name involved. An anonymous pattern HEAD
	// has no name at all: ir.matchNodeScan leaves its NodeVar "" (only a non-head
	// node gets a synthetic `__anon_N`), and the physical builder never registers ""
	// for an Expand destination either — it substitutes the synthesised key
	// `__anon_to_N`. So the mirror of `MATCH (:A)-[:R]->(b:B)` would carry
	// Selection{LabelPredicate(Variable{""}, [A])} over a re-rooted Expand whose
	// destination is bound under `__anon_to_N`: the receiver resolves to no column,
	// evaluates to NULL, and the Filter drops EVERY row — a silent wrong answer with
	// no error and no notification. Declining the site keeps the written order, which
	// is correct for the shape; recovering the optimisation needs the mirror to bind
	// that endpoint to a readable name, which is a separate change.
	if exp.FromVar == "" || toVarExpr.Name == "" {
		return anchorSite{}, false
	}
	if len(exp.RelTypes) != 1 || exp.PathVar != "" || len(exp.SiblingRelVars) != 0 {
		return anchorSite{}, false
	}
	scan, ok := exp.Child.(*ir.NodeByLabelScan)
	if !ok || scan.NodeVar != exp.FromVar {
		return anchorSite{}, false
	}
	return anchorSite{
		topSel:    sel,
		exp:       exp,
		scan:      scan,
		fromVar:   exp.FromVar,
		fromLabel: scan.Label,
		toVar:     toVarExpr.Name,
		toLabel:   lp.Labels[0],
		relType:   exp.RelTypes[0],
	}, true
}

// reverseSingleEdgeDir flips a traversal direction for a re-rooted single edge:
// Incoming ⇄ Outgoing (the same directed edge, walked from the other endpoint),
// and Both stays Both (an undirected edge is symmetric). This is the
// data-direction-fidelity rule of §6: re-rooting flips the START endpoint, never
// the semantic edge direction, so `(a)<-[r]-(b)` rooted at b is `(b)-[r]->(a)` —
// the same edge b→a.
func reverseSingleEdgeDir(d ir.Direction) ir.Direction {
	switch d {
	case ir.DirectionIncoming:
		return ir.DirectionOutgoing
	case ir.DirectionOutgoing:
		return ir.DirectionIncoming
	default:
		return ir.DirectionBoth
	}
}

// mirrorAnchorSite builds the result-identical reversal of a matched site: the
// same single edge re-rooted onto the other endpoint (task #2089, §6). The
// mirror scans the to-label, expands the same relationship in the flipped
// direction back to the from-var, and re-checks the from-label as the top
// Selection — exactly the plan the translator emits for the openCypher-mirror
// pattern. Relationship-uniqueness is trivially preserved (a single edge), all
// variable bindings (fromVar, toVar, relVar) are preserved, and the semantic
// edge direction is preserved by [reverseSingleEdgeDir].
func mirrorAnchorSite(s *anchorSite) ir.LogicalPlan {
	newScan := ir.NewNodeByLabelScan(s.toVar, s.toLabel)
	newExp := ir.NewExpand(
		s.toVar,
		s.exp.RelVar,
		[]string{s.relType},
		reverseSingleEdgeDir(s.exp.Direction),
		s.fromVar,
		newScan,
	)
	lp := &ast.LabelPredicate{
		Receiver: &ast.Variable{Name: s.fromVar},
		Labels:   []string{s.fromLabel},
	}
	return ir.NewSelectionExpr(lp.String(), lp, newExp)
}

// collectAnchorSwapCandidates returns every structurally-qualifying,
// ORDER-SAFE single-edge anchor site in the plan, in a stable pre-order. It is a
// pure function of the immutable plan (structure + spine order-safety), so it is
// computed once at parse time and memoised in the plan-cache entry; the per-query
// cost gate ([computeAnchorSwaps]) consumes its output. The spine handed to
// [SuppressReorder] is the site's ancestor chain, nearest ancestor first.
func collectAnchorSwapCandidates(root ir.LogicalPlan) []anchorSite {
	if root == nil {
		return nil
	}
	var out []anchorSite
	var stack []ir.LogicalPlan // ancestors, root-first (top-down)
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil {
			return
		}
		if sel, ok := p.(*ir.Selection); ok {
			if site, ok := matchAnchorSite(sel); ok {
				// Spine nearest-first = the ancestor stack reversed. The site's
				// reorder point is the Selection itself, so its spine is its
				// ancestors (not including itself).
				spine := make([]ir.LogicalPlan, len(stack))
				for i := range stack {
					spine[i] = stack[len(stack)-1-i]
				}
				// A NamedPath ancestor reconstructs a path value from THIS
				// pattern's traversal triplet, so reversing the edge would reverse
				// the path's node order — not result-identical. SuppressReorder
				// treats NamedPath as neutral (correct for the reorder peephole,
				// which never reverses an edge), so the anchor swap needs its own
				// veto here. Any NamedPath on the spine declines the site.
				if !SuppressReorder(spine) && !spineHasNamedPath(spine) {
					out = append(out, site)
				}
			}
		}
		stack = append(stack, p)
		for _, c := range p.Children() {
			walk(c)
		}
		stack = stack[:len(stack)-1]
	}
	walk(root)
	return out
}

// spineHasNamedPath reports whether any ancestor on the spine is a NamedPath, in
// which case a reversal would change the reconstructed path value and the site
// must be declined.
func spineHasNamedPath(spine []ir.LogicalPlan) bool {
	for _, p := range spine {
		if _, ok := p.(*ir.NamedPath); ok {
			return true
		}
	}
	return false
}

// computeAnchorSwaps applies the live count-store cost gate to the memoised,
// order-safe candidates and returns the set of Expand nodes whose pattern should
// be re-rooted at build time. It returns nil when nothing qualifies.
//
// The gate (design §1, §2, §5.1): OUT-ward only (written direction Incoming),
// every cost input EstExact ∧ ¬dirty (else the trustworthiness veto keeps the
// written order), and a strict cost win under [anchorSwapMargin]. All counts are
// read from the query's single snapshot via labelSrc, which the caller holds
// under View's visibility barrier.
func computeAnchorSwaps(sites []anchorSite, labelSrc labelResolverIface) map[*ir.Expand]bool {
	if len(sites) == 0 {
		return nil
	}
	cs, _ := labelSrc.(countSource)
	if cs == nil {
		// No count store on the resolver → every D estimate falls back → veto all.
		return nil
	}
	var swaps map[*ir.Expand]bool
	for _, s := range sites {
		nFrom := labelCardinalityEstimate(labelSrc, s.fromLabel)
		nTo := labelCardinalityEstimate(labelSrc, s.toLabel)

		var written, candidate float64
		switch s.exp.Direction {
		case ir.DirectionIncoming:
			// OUT-ward (shipped, §5.1). Written (anchor-from, Incoming): scan N(from)
			// + walk D(from, R, IN). Candidate (anchor-to, Outgoing): scan N(to) +
			// walk D(to, R, OUT).
			dFromIn := degreeCardinalityEstimate(cs, s.fromLabel, s.relType, count.In)
			dToOut := degreeCardinalityEstimate(cs, s.toLabel, s.relType, count.Out)
			// §1 trustworthiness veto: any non-exact / dirty input keeps written order.
			// A relabel-dirtied D cell surfaces here as EstFallback and vetoes.
			if planStaysDefault(nFrom, nTo, dFromIn, dToOut) {
				continue
			}
			// The WRITTEN side is the reverse one here, and its probe term is
			// deliberately NOT modelled: leaving it out understates the written cost,
			// so the modelled win stays a lower bound on the real win.
			written = anchorSwapNodeCost*nFrom.rows + anchorSwapEdgeCost*dFromIn.rows
			candidate = anchorSwapNodeCost*nTo.rows + anchorSwapEdgeCost*dToOut.rows

		case ir.DirectionOutgoing:
			// IN-ward (#2150). Written (anchor-from, Outgoing): scan N(from) + walk
			// D(from, R, OUT). Candidate (anchor-to, Incoming): scan N(to) + walk
			// D(to, R, IN), where every in-edge additionally probes the forward range
			// of ITS SOURCE — a from-label node — so the probe depth is keyed on the
			// from side.
			dFromOut := degreeCardinalityEstimate(cs, s.fromLabel, s.relType, count.Out)
			dToIn := degreeCardinalityEstimate(cs, s.toLabel, s.relType, count.In)
			// The veto covers dFromOut, which this direction needs TWICE — for the
			// written cost and as the probe-depth input. That is the extra
			// trustworthiness condition the reverse-introducing direction requires:
			// without an exact, non-dirty from-side out-degree the probe depth is
			// unknown and the candidate is not costable.
			if planStaysDefault(nFrom, nTo, dFromOut, dToIn) {
				continue
			}
			written = anchorSwapNodeCost*nFrom.rows + anchorSwapEdgeCost*dFromOut.rows
			candidate = anchorSwapNodeCost*nTo.rows +
				(anchorSwapEdgeCost+anchorSwapProbeBase+
					anchorSwapProbeCost*probeDepthEstimate(dFromOut.rows))*dToIn.rows

		default:
			// Undirected (Both): the mirror is also Both, so the swap would re-anchor
			// without changing the traversal, and D(label, R, BOTH) is not a modelled
			// cell. Vetoed.
			continue
		}

		// Strict win under the margin. Equal (or near-equal) costs keep the
		// written order, so the swap never merely reshuffles a tie.
		if candidate*anchorSwapMargin <= written && candidate < written {
			if swaps == nil {
				swaps = make(map[*ir.Expand]bool, len(sites))
			}
			swaps[s.exp] = true
		}
	}
	return swaps
}

// probeDepthEstimate returns the modelled binary-search depth of ONE in-edge's
// forward-position probe, from the degree-sum of the far side's out-edges of this
// type (#2150). dFarOut is D(fromLabel, relType, OUT) — a SUM over the label, which
// is the scale of the forward ranges the reverse path searches.
//
// It is an ESTIMATE, not a bound, and deliberately so. Two things stop it from
// being one: D is an aggregate and cannot see degree skew, and the mirror walks
// in-edges whose sources may carry ANY label (the from-label Selection filters
// afterwards), so the probed range is not confined to the from-label family at all.
// Pretending otherwise would be a false guarantee.
//
// WHY NO REGRESSION IS STILL GUARANTEED — the argument rests on a structural bound
// plus the margin, not on this estimate being tight. A binary search over a
// 64-bit-addressable adjacency has depth at most 64, so the probe penalty per
// in-edge is at most
//
//	anchorSwapProbeCost·64 + anchorSwapProbeBase = 12·64 + 4 = 772 < 800 = anchorSwapEdgeCost
//
// Hence the TRUE cost of an in-edge is under twice the modelled cost of an out-edge,
// for any graph. So:
//
//	true(candidate) ≤ c_s·N(to) + 2·c_e·D(to,R,IN) ≤ 2·modelled(candidate, no probe term)
//
// and the gate admits only when modelled(candidate)·2 ≤ modelled(written), while the
// written OUT side carries no probe term and is therefore modelled exactly. Chaining
// the two gives true(candidate) ≤ true(written): the swap cannot be slower than the
// written order. Modelling the probe term explicitly on top of that only makes the
// gate STRICTER than the proof requires, which is the direction to err in.
//
// This is what changed since #2089a, and it is a change in kind. An unmodelled
// Θ(out-degree) penalty is unbounded relative to c_e, so no constant margin can
// absorb it; a Θ(log out-degree) penalty is bounded by 64 levels and the margin can.
//
// ceil(log₂(d + 1)) is the number of levels a binary search over d slots performs.
// A non-positive input yields 0 (nothing to probe), and a non-finite one saturates
// at the structural maximum of 64.
func probeDepthEstimate(dFarOut float64) float64 {
	if dFarOut <= 0 {
		return 0
	}
	if math.IsInf(dFarOut, 0) || math.IsNaN(dFarOut) {
		return 64
	}
	depth := math.Ceil(math.Log2(dFarOut + 1))
	if depth > 64 {
		return 64
	}
	return depth
}

// countSource is satisfied by *lpgLabelResolver; declared in count_estimate.go.
// degreeCardinalityEstimate there takes a countSource directly, so this peephole
// passes the asserted resolver rather than re-wrapping it.

// tryBuildAnchorSwap is the build-time application of the anchor swap. When the
// Selection p is the top of a matched single-edge site whose Expand was chosen
// for a swap (in computeAnchorSwaps), it builds the mirror plan instead and
// returns (op, true, nil). Otherwise it returns (nil, false, nil) and the caller
// continues the normal Selection build.
//
// The swap map is keyed on the ORIGINAL Expand pointer, and the mirror allocates
// a FRESH Expand (a different pointer, not in the map), so recursing buildOperator
// on the mirror re-enters this function but never re-fires — the swap is applied
// exactly once. Every non-read build path leaves bopts.anchorSwap nil, so this is
// a no-op there.
func tryBuildAnchorSwap(
	p *ir.Selection,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
	procReg *procs.Registry,
	argByTag map[uint32]*exec.Argument,
	bopts *buildOpts,
) (exec.Operator, bool, error) {
	if bopts == nil || len(bopts.anchorSwap) == 0 {
		return nil, false, nil
	}
	site, ok := matchAnchorSite(p)
	if !ok || !bopts.anchorSwap[site.exp] {
		return nil, false, nil
	}
	mirror := mirrorAnchorSite(&site)
	op, err := buildOperator(mirror, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
	if err != nil {
		return nil, false, err
	}
	anchorSwapBuildCount.Add(1)
	return op, true, nil
}
