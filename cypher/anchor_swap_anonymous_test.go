package cypher

// anchor_swap_anonymous_test.go — regression gates for rmp #2603: the anchor-swap
// peephole returned the WRONG ANSWER, silently, whenever it fired on a pattern
// whose SOURCE node is anonymous.
//
// Layer: short. Engines and graphs are local, so the suite is goleak-clean
// (enforced by TestMain in testmain_test.go).
//
// WHY THE DEFECT EXISTED. The reversal moves the from-label from the ACCESS PATH
// onto a Selection predicate: the written plan enforces it structurally in
// `NodeByLabelScan{fromVar, fromLabel}`, which needs no variable name, while the
// mirror re-checks it as `Selection{LabelPredicate(fromVar, [fromLabel])}` above the
// re-rooted Expand, which can only reach the node THROUGH ITS NAME. An anonymous
// pattern HEAD has no name: ir.matchNodeScan leaves its NodeVar "" (only a non-head
// node is given a synthetic `__anon_N`), and the physical builder never registers ""
// for an Expand destination either — it substitutes `__anon_to_N`. So the mirror's
// receiver resolved to no column, evaluated to NULL, and the Filter dropped every
// row.
//
// WHY THE EXISTING GATES WERE BLIND. Every pattern in anchor_swap_diff_test.go and
// anchor_swap_symmetric_test.go named both endpoints, so the differential oracle only
// ever compared the spelling that was already correct; and the openCypher TCK
// scenario that uses the anonymous-both-sides spelling
// (clauses/match/Match2.feature [2]) cannot reach the peephole at all, for two
// independent reasons — its relationship is UNTYPED (matchAnchorSite requires exactly
// one type) and its graph is BALANCED (2 :A, 2 :B, so anchorSwapMargin is
// unreachable). TestAnchorSwapAnonymous_TCKMatch2Shape below pins both.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// anchorScalar runs q through the READ path (Engine.Run) and returns the first
// column of the single row it must produce, rendered.
//
// Both halves matter, and both were needed to see this defect at all:
//
//   - The READ path is the only build path that populates buildOpts.anchorSwap
//     (see the comment at cypher/api.go's tryBuildAnchorSwap), so a reproduction
//     routed through RunInTx/RunInTxAny disarms the peephole and reports all-green.
//   - `RETURN count(*)` always yields exactly ONE row, so an instrument that counts
//     rows reports success whether the aggregate is 0 or 1. The VALUE is the oracle.
func anchorScalar(t *testing.T, e *Engine, q string) string {
	t.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	var got string
	rows := 0
	for res.Next() {
		rows++
		v := res.ValueAt(0)
		if v == nil {
			t.Fatalf("%q produced a row with no column 0", q)
		}
		got = v.String()
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	if rows != 1 {
		t.Fatalf("%q returned %d rows, want exactly 1 — the scalar oracle needs one row", q, rows)
	}
	return got
}

// TestAnchorSwapAnonymous_SourceAnonymousKeepsTheRows is the #2603 regression gate.
//
// For each fixture and each anonymous spelling it asserts the SCALAR the query
// computes, and — as the non-vacuity control — that the SAME fixture with the source
// NAMED does fire the swap. Without that control the case could pass merely because
// the fixture was too balanced for the cost gate to admit anything, which is exactly
// how the openCypher TCK scenario for this spelling stays green over a broken build.
//
// On the pre-fix build every anonymous-source row asserts twice over: the scalar is
// 0 instead of 1, and the swap fires where it must now decline.
func TestAnchorSwapAnonymous_SourceAnonymousKeepsTheRows(t *testing.T) {
	for _, fx := range []struct {
		name  string
		seed  func(*testing.T) *lpg.Graph[string, float64]
		arrow string
	}{
		// Written DirIn: anchor Hub and walk its ~1601 incoming R-edges, or re-root
		// onto Leaf and walk 1. The OUT-ward direction (§5.1).
		{"outward_writtenIN", seedHubGraph, "<-[:R]-"},
		// Written DirOut: the IN-ward (reverse-introducing) direction (#2150).
		{"inward_writtenOUT", seedReverseHubGraph, "-[:R]->"},
	} {
		t.Run(fx.name, func(t *testing.T) {
			g := fx.seed(t)
			e := NewEngine(g)

			for _, tc := range []struct {
				name string
				// anon has an anonymous SOURCE; named is the same query with the
				// source named, and must still fire the swap.
				anon, named string
				want        string
			}{
				{
					// The shape the defect was reported on. count(*) keeps the row
					// count at 1 either way, so only the value can tell.
					name:  "count_star",
					anon:  "MATCH (:Hub)" + fx.arrow + "(b:Leaf) RETURN count(*)",
					named: "MATCH (a:Hub)" + fx.arrow + "(b:Leaf) RETURN count(*)",
					want:  "1",
				},
				{
					// BOTH endpoints anonymous in the written text. Note this is not a
					// second failure mode: the IR translator names every NON-HEAD node
					// (ir.matchPathPattern assigns a synthetic `__anon_N`), so only the
					// HEAD can ever carry the empty name.
					name:  "count_star_both_anonymous",
					anon:  "MATCH (:Hub)" + fx.arrow + "(:Leaf) RETURN count(*)",
					named: "MATCH (a:Hub)" + fx.arrow + "(:Leaf) RETURN count(*)",
					want:  "1",
				},
				{
					// A named relationship counted through the aggregate: the swap
					// preserves the relationship binding, so count(r) must agree.
					name:  "count_rel",
					anon:  "MATCH (:Hub)" + relNamed(fx.arrow) + "(b:Leaf) RETURN count(r)",
					named: "MATCH (a:Hub)" + relNamed(fx.arrow) + "(b:Leaf) RETURN count(r)",
					want:  "1",
				},
				{
					// The destination's property still reaches the projection, so a
					// dropped row shows up as a missing row rather than a zero.
					name:  "destination_property",
					anon:  "MATCH (:Hub)" + fx.arrow + "(b:Leaf) RETURN b.i",
					named: "MATCH (a:Hub)" + fx.arrow + "(b:Leaf) RETURN b.i",
					want:  "1",
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := anchorSwapBuildCount.Load()
					got := anchorScalar(t, e, tc.anon)
					firedAnon := anchorSwapBuildCount.Load() - before
					if got != tc.want {
						t.Fatalf("%q returned %s, want %s — the anonymous-source pattern lost its "+
							"rows (rmp #2603: the mirror re-checks the from-label through a "+
							"variable name the anonymous head does not have)", tc.anon, got, tc.want)
					}
					if firedAnon != 0 {
						t.Fatalf("the anchor swap fired %d time(s) for %q; an anonymous source must "+
							"decline the site, because the mirror cannot re-check its label",
							firedAnon, tc.anon)
					}

					// Non-vacuity: the fixture must actually reach the peephole, so
					// that the case above is the GUARD declining and not the cost gate
					// finding nothing to do.
					before = anchorSwapBuildCount.Load()
					gotNamed := anchorScalar(t, e, tc.named)
					firedNamed := anchorSwapBuildCount.Load() - before
					if gotNamed != tc.want {
						t.Fatalf("control %q returned %s, want %s", tc.named, gotNamed, tc.want)
					}
					if firedNamed == 0 {
						t.Fatalf("control %q did NOT fire the anchor swap, so this fixture cannot "+
							"show the guard doing anything: the anonymous case above would pass "+
							"even on a build with no guard at all", tc.named)
					}
				})
			}
		})
	}
}

// relNamed turns an anonymous-relationship arrow into the same arrow with the
// relationship bound to `r`.
func relNamed(arrow string) string {
	switch arrow {
	case "<-[:R]-":
		return "<-[r:R]-"
	default:
		return "-[r:R]->"
	}
}

// TestAnchorSwapAnonymous_TCKMatch2Shape covers the openCypher scenario whose
// spelling this defect broke, and pins the two independent reasons the TCK suite
// could not see it.
//
// clauses/match/Match2.feature scenario [2] "Matching a relationship pattern using a
// label predicate on both sides" creates
// `(:A)-[:T1]->(:B), (:B)-[:T2]->(:A), (:B)-[:T3]->(:B), (:A)-[:T4]->(:A)` and expects
// `MATCH (:A)-[r]->(:B) RETURN r` to return exactly `[:T1]`. That query is UNTYPED, so
// matchAnchorSite declines it (D is a per-type statistic); and the graph is BALANCED,
// so even the typed spelling cannot clear anchorSwapMargin. Give the same fixture a
// relationship type AND a label skew and the peephole engages — which is where the
// wrong answer appeared.
//
// This lives here as a Go test rather than as a new .feature scenario deliberately:
// the TCK regression gate in cypher/tck/runner_test.go pins an EXACT scenario count
// (tckExecutionBaseline), so adding a scenario would move the baseline.
func TestAnchorSwapAnonymous_TCKMatch2Shape(t *testing.T) {
	const tckFixture = "CREATE (:A)-[:T1]->(:B), (:B)-[:T2]->(:A), (:B)-[:T3]->(:B), (:A)-[:T4]->(:A)"

	for _, tc := range []struct {
		name string
		// extra skews the label cardinalities so the cost gate can admit.
		extra string
		q     string
		// wantFire records whether the peephole reaches this spelling at all.
		wantFire bool
	}{
		{
			// Verbatim TCK: untyped relationship → matchAnchorSite declines outright.
			name: "verbatim_untyped_balanced",
			q:    "MATCH (:A)-[r]->(:B) RETURN count(r)",
		},
		{
			// Typed but balanced (2 :A, 2 :B): the cost gate finds no 2x win.
			name: "typed_balanced",
			q:    "MATCH (:A)-[r:T1]->(:B) RETURN count(r)",
		},
		{
			// Typed AND skewed: the peephole engages. Pre-#2603 this returned 0.
			name:  "typed_skewed_anonymous_source",
			extra: "UNWIND range(1,40) AS i CREATE (:A {i:i})",
			q:     "MATCH (:A)-[r:T1]->(:B) RETURN count(r)",
		},
		{
			// The same skew with the source NAMED: this is the spelling that always
			// worked, and it must still take the swap.
			name:     "typed_skewed_named_source",
			extra:    "UNWIND range(1,40) AS i CREATE (:A {i:i})",
			q:        "MATCH (a:A)-[r:T1]->(:B) RETURN count(r)",
			wantFire: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stmts := []string{tckFixture}
			if tc.extra != "" {
				stmts = append(stmts, tc.extra)
			}
			g := buildAnchorGraph(t, stmts...)
			e := NewEngine(g)

			before := anchorSwapBuildCount.Load()
			got := anchorScalar(t, e, tc.q)
			fired := anchorSwapBuildCount.Load() - before

			// The scenario's own expectation: exactly one relationship, [:T1].
			if got != "1" {
				t.Fatalf("%q returned %s, want 1 — openCypher Match2 [2] requires the "+
					"anonymous-both-sides spelling to return the matching relationship", tc.q, got)
			}
			if gotFire := fired != 0; gotFire != tc.wantFire {
				t.Fatalf("%q fired the anchor swap %d time(s), wantFire=%v — the reachability of the "+
					"peephole for this spelling is what decides whether the TCK suite could have "+
					"caught the defect", tc.q, fired, tc.wantFire)
			}
		})
	}
}

// TestAnchorSwapAnonymous_MirrorCannotReCheckAnEmptyName pins the REASON the guard in
// matchAnchorSite exists, on the one function that cannot express it any other way.
//
// mirrorAnchorSite re-checks the from-label as a LabelPredicate over fromVar. Handed a
// site whose fromVar is the empty string — the shape ir.matchNodeScan produces for an
// anonymous pattern head — it emits a receiver that binds nothing, and the physical
// builder compounds it: an Expand whose ToVar is "" registers its destination under
// the synthesised key `__anon_to_N`, never under "". So the predicate resolves to no
// column, evaluates to NULL, and drops every row.
//
// If a future change makes the mirror bind that endpoint to a readable name (the
// alternative fix, which would recover the optimisation for these patterns), this test
// fails — and the guard should then be revisited rather than left in place by inertia.
func TestAnchorSwapAnonymous_MirrorCannotReCheckAnEmptyName(t *testing.T) {
	// The site the translator would produce for `MATCH (:A)-[:R]->(b:B)`, built by
	// hand because matchAnchorSite now (correctly) refuses to return it.
	scan := ir.NewNodeByLabelScan("", "A")
	exp := ir.NewExpand("", "r", []string{"R"}, ir.DirectionOutgoing, "b", scan)
	lp := &ast.LabelPredicate{Receiver: &ast.Variable{Name: "b"}, Labels: []string{"B"}}
	sel := ir.NewSelectionExpr(lp.String(), lp, exp)
	site := anchorSite{
		topSel: sel, exp: exp, scan: scan,
		fromVar: "", fromLabel: "A", toVar: "b", toLabel: "B", relType: "R",
	}

	// Precondition: this really is the shape the translator emits, so the hand-built
	// site is not a strawman.
	real, ok := firstAnchorSite(t, translateForTest(t, "MATCH (:A)-[r:R]->(b:B) RETURN b"))
	if ok {
		t.Fatalf("matchAnchorSite accepted an anonymous-source site (%s); the #2603 guard is gone",
			siteFingerprint(&real))
	}

	mirror := mirrorAnchorSite(&site)
	top, ok := mirror.(*ir.Selection)
	if !ok {
		t.Fatalf("mirror root is %T, want *ir.Selection", mirror)
	}
	pred, ok := top.PredicateExpr.(*ast.LabelPredicate)
	if !ok {
		t.Fatalf("mirror predicate is %T, want *ast.LabelPredicate", top.PredicateExpr)
	}
	recv, ok := pred.Receiver.(*ast.Variable)
	if !ok {
		t.Fatalf("mirror predicate receiver is %T, want *ast.Variable", pred.Receiver)
	}
	if recv.Name != "" {
		t.Fatalf("the mirror now re-checks the from-label through the readable name %q instead of "+
			"the empty name: the anonymous-source guard in matchAnchorSite may no longer be "+
			"necessary, and #2603's cost (the swap declines for every anonymous-head pattern) "+
			"can be recovered — re-derive the guard rather than leaving it in place", recv.Name)
	}
	mirrorExp, ok := top.Child.(*ir.Expand)
	if !ok {
		t.Fatalf("mirror child is %T, want *ir.Expand", top.Child)
	}
	if mirrorExp.ToVar != "" {
		t.Fatalf("mirror Expand.ToVar = %q, want \"\" — the endpoint the predicate above must "+
			"read is the one the empty name refers to", mirrorExp.ToVar)
	}
}
