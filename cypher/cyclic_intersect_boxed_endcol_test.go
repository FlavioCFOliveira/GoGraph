package cypher

// cyclic_intersect_boxed_endcol_test.go — regression gates for the two wrong-result
// defects in the fused cyclic expand (rmp #2267: the planner audit's F3 and F4).
//
// Layer: short.
//
// # Why this file cannot rely on the usual instruments
//
// Both defects returned WRONG RESULTS with no error, and every gate this project
// normally leans on is structurally blind to them:
//
//   - The TCK is blind. SPIKE #2155 established that the openCypher TCK contains
//     NO directed cycle over three or more distinct node variables, so 3897/3897
//     stays green whether this operator is right, wrong, or never runs.
//   - A flag-on/flag-off differential is blind whenever the recogniser DECLINES,
//     because both arms then run the same plan and agree perfectly. That is exactly
//     the endCol case: the recogniser must now decline it, so on and off agree — and
//     they agreed BEFORE the fix too, at the wrong answer, because the operator
//     engaged and silently emitted nothing.
//   - The engagement counter alone is blind to the boxed case, because there the
//     operator DID engage, four times, and still produced nothing.
//
// So every case below pins THREE things at once: the engagement verdict (did the
// fused operator run, or was the shape vetoed?), the differential against the
// two-Expand plan, and an ABSOLUTE row count computed by hand from the fixture.
// Any two of the three can agree while the third is wrong; that is how both of
// these defects survived until now.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ── #2267 defect 1: a BOXED node cell ───────────────────────────────────────────

// TestCyclicIntersect_BoxedNodeColumn covers the representation defect: a node
// column that has flowed through a projection arrives as a full expr.NodeValue
// rather than the raw expr.IntegerValue the in-pipeline encoding uses.
//
// FAILS ON THE OLD CODE: nodeAt asserted directly to expr.IntegerValue, so every
// row whose node cell was boxed was classified as a malformed column and SKIPPED.
// Skipping every row is indistinguishable from a graph with no cycles, so each
// query below returned 0 while the two-Expand plan returned 18 — and the operator
// engaged on every one of them, so no counter would have flagged it.
func TestCyclicIntersect_BoxedNodeColumn(t *testing.T) {
	// One triangle with 3 parallel edges on 1→2 and 2 on 2→0: three rotations,
	// each enumerated once per edge-choice combination, so 3 × 3 × 2 × 1 = 18.
	g := cyclicGraph(t, 4, [][2]int{
		{0, 1},
		{1, 2}, {1, 2}, {1, 2},
		{2, 0}, {2, 0},
	})
	const wantRows = 18

	cases := []struct {
		name    string
		query   string
		boxedBy string
	}{{
		// `WITH a` projects the anchor, so the column the cycle CLOSES on (endCol)
		// arrives boxed. This is the minimal reproduction.
		name:    "projected_close_variable",
		query:   `MATCH (a) WITH a MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`,
		boxedBy: "endCol — the closing variable came through a projection",
	}, {
		// Projecting both endpoints boxes the MIDDLE hop's source column too, so
		// this covers midCol on the same path.
		name:    "projected_close_and_mid_variables",
		query:   `MATCH (a)-[:K]->(b) WITH a, b MATCH (b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`,
		boxedBy: "midCol and endCol — both endpoints came through a projection",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var off, on []string
			offEngaged := withEngageProbe(t, func() { off = runRows(t, NewEngine(g), tc.query) })
			onEngaged := withEngageProbe(t, func() {
				on = runRows(t, NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}), tc.query)
			})

			// The engagement proof comes FIRST. Without it a green differential
			// below would prove only that both arms ran the same plan.
			if offEngaged != 0 {
				t.Fatalf("the operator engaged %d times with the flag OFF; want 0", offEngaged)
			}
			if onEngaged == 0 {
				t.Fatalf("the operator did NOT engage on a shape that boxes %s, so this "+
					"case no longer exercises the boxed-cell path at all", tc.boxedBy)
			}

			assertCountRow(t, "flag off", off, wantRows)
			assertCountRow(t, "flag on", on, wantRows)
		})
	}
}

// ── #2267 defect 2: a mis-resolved endCol ───────────────────────────────────────

// TestCyclicIntersect_SelfLoopClosingHopIsVetoed covers the column-resolution
// defect. A cycle whose closing hop is a self-loop closes on a variable the MIDDLE
// hop binds, so that node is not in the fused operator's input row at all — it is
// what the fused-away hop was going to produce.
//
// FAILS ON THE OLD CODE: schema[p.IntoVar] resolved to the middle hop's own
// destination slot, past the end of the input row, so every row read as a malformed
// column and was skipped. The query returned 0 where it must return 5.
//
// The fix VETOES the shape rather than guessing a column for it, because declining
// is always safe and merely falls back to the two-Expand plan — which is why the
// assertion below is that the operator does NOT engage.
func TestCyclicIntersect_SelfLoopClosingHopIsVetoed(t *testing.T) {
	// Edges: 0→1, two parallel self-loops on 1, 0→2, one self-loop on 2.
	//
	// The hand-computed oracle for MATCH (a)-[r1:K]->(b)-[r2:K]->(b), where r1 and
	// r2 must be distinct edges (openCypher relationship isomorphism, §3.2.2):
	//
	//	a=0, b=1 via 0→1     : r2 ∈ {both self-loops on 1}           → 2
	//	a=1, b=1 via loop e1 : r2 ∈ {the OTHER self-loop on 1}       → 1
	//	a=1, b=1 via loop e2 : r2 ∈ {the OTHER self-loop on 1}       → 1
	//	a=0, b=2 via 0→2     : r2 ∈ {the single self-loop on 2}      → 1
	//	a=2, b=2 via loop    : r2 must differ from r1, none left     → 0
	//	                                                        total  5
	g := cyclicGraph(t, 3, [][2]int{
		{0, 1}, {1, 1}, {1, 1}, {0, 2}, {2, 2},
	})
	const q = `MATCH (a)-[r1:K]->(b)-[r2:K]->(b) RETURN count(*) AS n`
	const wantRows = 5

	var off, on []string
	_ = withEngageProbe(t, func() { off = runRows(t, NewEngine(g), q) })
	engaged := withEngageProbe(t, func() {
		on = runRows(t, NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}), q)
	})

	if engaged != 0 {
		t.Fatalf("the operator engaged %d times on a self-loop-closing cycle. Its closing "+
			"variable is bound by the very hop being fused away, so there is no correct "+
			"input column for it and the shape must be vetoed", engaged)
	}
	assertCountRow(t, "flag off", off, wantRows)
	assertCountRow(t, "flag on", on, wantRows)
}

// ── The absolute oracle, over a fixture with every awkward feature at once ───────

// TestCyclicIntersect_AbsoluteOracleWithTombstone pins a hand-computed triangle
// count over a fixture that carries parallel edges, a self-loop AND a tombstoned
// node, so the operator is exercised against a graph whose live node space is
// narrower than its id space.
//
// The tombstone is not decoration. ExpandIntersect intersects RAW CSR runs with no
// visibility filter inside the merge, so a deleted node reaching the candidate set
// would show up here and nowhere else in this package.
func TestCyclicIntersect_AbsoluteOracleWithTombstone(t *testing.T) {
	// Edges, all of type K:
	//	0→1 (×1), 1→2 (×2), 2→0 (×2)   — triangle A, with parallel edges
	//	2→2 (×1)                        — a self-loop
	//	3→4, 4→5, 5→3                   — triangle B, disjoint from A
	//
	// MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) enumerates each directed 3-cycle once
	// per ROTATION, times the product of its three legs' multiplicities:
	//
	//	triangle A : 3 rotations × (1 × 2 × 2) = 12
	//	triangle B : 3 rotations × (1 × 1 × 1) =  3
	//	self-loop  : a 3-cycle needs three PAIRWISE DISTINCT edges and there is
	//	             only one 2→2, so it contributes 0
	//	                                            live total = 15
	//
	// Tombstoning node 5 removes triangle B entirely (both 4→5 and 5→3 lose an
	// endpoint), leaving 12.
	const (
		wantLive       = 15
		wantTombstoned = 12
	)
	edges := [][2]int{
		{0, 1},
		{1, 2}, {1, 2},
		{2, 0}, {2, 0},
		{2, 2},
		{3, 4}, {4, 5}, {5, 3},
	}
	const q = `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`

	t.Run("all_nodes_live", func(t *testing.T) {
		g := cyclicGraph(t, 6, edges)
		assertFusedAndAgrees(t, g, q, wantLive)
	})

	t.Run("one_node_tombstoned", func(t *testing.T) {
		g := cyclicGraph(t, 6, edges)
		g.RemoveNode("n5")
		assertFusedAndAgrees(t, g, q, wantTombstoned)
		// Anti-degeneracy: if the tombstone changed nothing, the case would prove
		// nothing about visibility.
		if wantTombstoned == wantLive {
			t.Fatal("the tombstoned oracle equals the live one, so the tombstone is inert")
		}
	})
}

// ── Every existing veto stays in force ──────────────────────────────────────────

// TestCyclicIntersect_VetoesStayInForce re-checks the vetoes the recogniser
// carried before #2267, plus the one it gained. #2267 narrowed what the operator
// admits; nothing about it may WIDEN the admitted set, and a fix that accidentally
// relaxed a veto would show up here rather than as a wrong answer months later.
func TestCyclicIntersect_VetoesStayInForce(t *testing.T) {
	g := cyclicGraph(t, 6, [][2]int{
		{0, 1}, {1, 2}, {2, 0}, {1, 3}, {3, 0}, {0, 1}, {2, 2},
	})

	cases := []struct{ name, query, why string }{{
		// The undirected leg must be one of the two FUSED legs to be vetoed. An
		// undirected leg lower in the chain is merely the fusion's input and does
		// not disqualify it — see TestCyclicIntersect_UndirectedLegBelowStillFuses.
		name:  "dir_both_middle_leg",
		query: `MATCH (a)-[:K]->(b)-[:K]-(c)-[:K]->(a) RETURN count(*) AS n`,
		why:   "an undirected neighbourhood is out ∪ in, which is not one contiguous ordered run",
	}, {
		name:  "dir_both_closing_leg",
		query: `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]-(a) RETURN count(*) AS n`,
		why:   "the closing leg must be DirectionOutgoing",
	}, {
		name:  "variable_length_leg",
		query: `MATCH (a)-[:K*1..2]->(b)-[:K]->(a) RETURN count(*) AS n`,
		why:   "a variable-length leg is not a fixed-arity hop",
	}, {
		name:  "non_expand_child",
		query: `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*) AS n`,
		why:   "label predicates interpose a Selection, so the child is not exactly an *exec.Expand",
	}, {
		name:  "mismatched_middle_hop",
		query: `MATCH (a)-[:K]->(b), (c)-[:K]->(a) RETURN count(*) AS n`,
		why:   "the hop below does not feed this hop's source (mid.ToVar != p.FromVar)",
	}, {
		name:  "acyclic",
		query: `MATCH (a)-[:K]->(b)-[:K]->(c) RETURN count(*) AS n`,
		why:   "no hop has IntoVar set, so the recogniser cannot fire at all",
	}, {
		name:  "self_loop_closing_hop",
		query: `MATCH (a)-[:K]->(b)-[:K]->(b) RETURN count(*) AS n`,
		why:   "#2267: the closing variable is bound by the hop being fused away",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var off, on []string
			_ = withEngageProbe(t, func() { off = runRows(t, NewEngine(g), tc.query) })
			engaged := withEngageProbe(t, func() {
				on = runRows(t, NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}), tc.query)
			})
			if engaged != 0 {
				t.Fatalf("the operator engaged %d times on a shape it must decline (%s)", engaged, tc.why)
			}
			if len(on) != len(off) {
				t.Fatalf("row count changed with the flag on: on=%d off=%d", len(on), len(off))
			}
			for i := range off {
				if on[i] != off[i] {
					t.Fatalf("row %d changed with the flag on:\n  on  %q\n  off %q", i, on[i], off[i])
				}
			}
		})
	}
}

// ── two facts the vetoes do NOT cover, pinned positively ────────────────────────

// TestCyclicIntersect_UndirectedLegBelowStillFuses records that the DirBoth veto
// is about the two FUSED legs and nothing else.
//
// This was written expecting a veto and the code proved the expectation wrong, so
// it is recorded rather than corrected: in `(a)-[:K]-(b)-[:K]->(c)-[:K]->(a)` the
// undirected leg is the fusion's INPUT, not one of its legs, and the two outgoing
// hops above it fuse and answer correctly. Vetoing on it would forfeit a valid
// fusion for no reason.
func TestCyclicIntersect_UndirectedLegBelowStillFuses(t *testing.T) {
	g := cyclicGraph(t, 6, [][2]int{
		{0, 1}, {1, 2}, {2, 0}, {1, 3}, {3, 0}, {0, 1}, {2, 2},
	})
	// Rotations of 0→1→2→0 (with 0→1 doubled) and of 0→1→3→0 (likewise):
	// 3 × 2 + 3 × 2 = 12.
	assertFusedAndAgrees(t, g,
		`MATCH (a)-[:K]-(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`, 12)
}

// TestCyclicIntersect_NamedPathReconstructsThroughTheFusion pins that a named path
// over a fused cycle still reconstructs.
//
// This one matters more than it looks. The recogniser vetoes a hop carrying a
// PathVar, but the translator sets PathVar only for a path that also has a
// variable-length leg — so a plain `MATCH p = (a)-->(b)-->(c)-->(a)` FUSES, and its
// path is rebuilt from the triplet sequence the two discarded Expand nodes recorded
// at plan time. That works only because the fused operator emits the same six
// columns at the same indices those triplets name. If that alignment ever slips,
// the count(*) differential stays green and only this test moves.
func TestCyclicIntersect_NamedPathReconstructsThroughTheFusion(t *testing.T) {
	g := cyclicGraph(t, 4, [][2]int{{0, 1}, {1, 2}, {2, 0}})
	for i := 0; i < 4; i++ {
		if err := g.SetNodeProperty("n"+itoaCyc(i), "x", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	// Every projection below is path-derived, so a mis-aligned column would change
	// the ANSWER rather than merely the row count.
	for _, q := range []string{
		`MATCH p = (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN [x IN nodes(p) | x.x] AS ns ORDER BY ns`,
		`MATCH p = (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN length(p) AS l, size(nodes(p)) AS n ORDER BY l`,
	} {
		t.Run(q, func(t *testing.T) {
			var off, on []string
			_ = withEngageProbe(t, func() { off = runRows(t, NewEngine(g), q) })
			engaged := withEngageProbe(t, func() {
				on = runRows(t, NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}), q)
			})
			if engaged == 0 {
				t.Fatal("the operator did not engage, so this says nothing about path " +
					"reconstruction through a fusion")
			}
			if len(off) == 0 {
				t.Fatal("the query returned no rows, so the comparison proves nothing")
			}
			if len(on) != len(off) {
				t.Fatalf("row count: on=%d off=%d\n  on:  %v\n  off: %v", len(on), len(off), on, off)
			}
			for i := range off {
				if on[i] != off[i] {
					t.Fatalf("row %d differs — the fused operator's columns no longer line up "+
						"with the triplets the path is rebuilt from:\n  on  %q\n  off %q", i, on[i], off[i])
				}
			}
		})
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────────

// assertCountRow checks that rows is exactly one row carrying want.
func assertCountRow(t *testing.T, arm string, rows []string, want int) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%s: got %d rows (%v); a global count(*) always returns exactly one", arm, len(rows), rows)
	}
	got := itoaCyc(want)
	if rows[0] != got {
		t.Fatalf("%s: count(*) = %s; the hand-computed oracle says %s", arm, rows[0], got)
	}
}

// assertFusedAndAgrees runs q with the flag off and on over g and requires that the
// operator engaged with it on, that the two arms return the identical row SEQUENCE,
// and that both equal the hand-computed oracle.
func assertFusedAndAgrees(t *testing.T, g *lpg.Graph[string, float64], q string, want int) {
	t.Helper()
	var off, on []string
	offEngaged := withEngageProbe(t, func() { off = runRows(t, NewEngine(g), q) })
	onEngaged := withEngageProbe(t, func() {
		on = runRows(t, NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}), q)
	})
	if offEngaged != 0 {
		t.Fatalf("the operator engaged %d times with the flag OFF; want 0", offEngaged)
	}
	if onEngaged == 0 {
		t.Fatal("the operator did NOT engage, so the differential below is vacuous — " +
			"both arms simply ran today's plan")
	}
	if len(on) != len(off) {
		t.Fatalf("row count: on=%d off=%d", len(on), len(off))
	}
	for i := range off {
		if on[i] != off[i] {
			t.Fatalf("row %d differs:\n  on  %q\n  off %q", i, on[i], off[i])
		}
	}
	assertCountRow(t, "flag off", off, want)
	assertCountRow(t, "flag on", on, want)
}
