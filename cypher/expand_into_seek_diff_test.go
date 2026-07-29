package cypher

// expand_into_seek_diff_test.go — result-identity gate for the bound-destination
// seek (#2151, for #2149).
//
// Layer: short. Engines and graphs are local, so the suite is goleak-clean
// (enforced by TestMain in testmain_test.go).
//
// Every case is checked THREE ways, not two:
//
//  1. against the seek DISABLED (EngineOptions.DisableExpandIntoSeek), which catches
//     a seek that changes an answer;
//  2. against an ABSOLUTE oracle computed from the edge list the test itself wrote,
//     which catches a defect the two arms would SHARE — both run the same row
//     pipeline below the cursor, so a differential alone can go green over a real
//     bug;
//  3. against the ACCESS PATH, asserting the seek actually fired
//     (exec.ExpandIntoSeekCount) where it is meant to and did NOT where it must fall
//     back. A differential whose arms silently take the same path is green for the
//     wrong reason.
//
// The seek is ORDER-PRESERVING, not merely multiset-preserving (design
// docs/design-expand-into-symmetric-swap.md §4): the slots sharing a destination are
// contiguous and handle-ordered, so the block dstRun returns is exactly the
// subsequence enumerate-and-filter emitted, in the same ascending position order. So
// these cases compare the row SEQUENCE, not a sorted bag — a weaker comparison would
// not notice a permutation the design claims is impossible.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seekArc is one directed edge the fixture writes. Several arcs may share (from,to):
// that is a parallel-edge group, and each member must still yield its own row.
type seekArc struct{ from, to int }

// seekFixture builds a labelled multigraph over nodes 0..n-1 (all :P, key "n<i>",
// property i) with exactly the given arcs, all of type K.
func seekFixture(t *testing.T, n int, arcs []seekArc) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "i", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", k, err)
		}
	}
	for _, a := range arcs {
		from, to := fmt.Sprintf("n%d", a.from), fmt.Sprintf("n%d", a.to)
		if err := g.AddEdge(from, to, 1.0); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", from, to, err)
		}
		g.SetEdgeLabel(from, to, "K")
	}
	return g
}

// seekRows drains query and returns one deterministic string per row, IN EMISSION
// ORDER. Record is a map, so the keys are sorted before formatting: Go map iteration
// order is randomised and would make the comparison flaky rather than strict.
func seekRows(t *testing.T, eng *Engine, query string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		rec := res.Record()
		keys := make([]string, 0, len(rec))
		for k := range rec {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&sb, "%s=%v;", k, rec[k])
		}
		out = append(out, sb.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return out
}

// oracleTwoCycleCount counts the rows `MATCH (a:P)-[:K]->(b:P)-[:K]->(a)` must
// return, straight from the arc list, with no engine involved.
//
// Per openCypher relationship isomorphism, the two hops must bind DISTINCT
// relationships. For an ordered pair (u,v) with u != v the hops draw from disjoint
// groups (u->v and v->u), so every combination is admissible: mult(u,v)·mult(v,u).
// For u == v both hops draw from the SAME self-loop group, so the pair must be
// ordered-distinct: mult·(mult-1).
func oracleTwoCycleCount(arcs []seekArc) int {
	mult := map[seekArc]int{}
	for _, a := range arcs {
		mult[a]++
	}
	total := 0
	for a, m := range mult {
		if a.from == a.to {
			total += m * (m - 1)
			continue
		}
		total += m * mult[seekArc{from: a.to, to: a.from}]
	}
	return total
}

// seekEngines returns the seek-ON and seek-OFF engines over g. The anchor swap is
// disabled in BOTH arms so the only variable between them is the seek — a two-hop
// pattern is not a swap candidate anyway, but pinning it keeps the arms honest if
// that ever changes.
func seekEngines(g *lpg.Graph[string, float64]) (on, off *Engine) {
	return NewEngineWithOptions(g, EngineOptions{DisableAnchorSwap: true}),
		NewEngineWithOptions(g, EngineOptions{DisableAnchorSwap: true, DisableExpandIntoSeek: true})
}

// assertSeekIdentical runs query with the seek on and off and requires the identical
// row SEQUENCE. wantFired says whether the seek must have engaged: false is the
// assertion that an excluded or undecidable shape genuinely FELL BACK, which is the
// half of the contract a result comparison cannot see.
func assertSeekIdentical(t *testing.T, g *lpg.Graph[string, float64], query string, wantFired bool) []string {
	t.Helper()
	on, off := seekEngines(g)

	// Count BOTH cursors: a closing hop that is DirIn narrows only the reverse one, so
	// checking the forward counter alone would call a working reverse seek "not fired".
	before := exec.ExpandIntoSeekCount() + exec.ExpandIntoSeekReverseCount()
	got := seekRows(t, on, query)
	fired := exec.ExpandIntoSeekCount() + exec.ExpandIntoSeekReverseCount() - before
	want := seekRows(t, off, query)

	if wantFired && fired == 0 {
		t.Fatalf("expected the bound-destination seek to fire for %q, but it never did — "+
			"the comparison would be vacuous", query)
	}
	if !wantFired && fired != 0 {
		t.Fatalf("expected %q to fall back to full enumeration, but the seek fired %d time(s)",
			query, fired)
	}
	if len(got) != len(want) {
		t.Fatalf("row count differs for %q: seek=%d enumerate=%d", query, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("row %d differs for %q (emission order must be IDENTICAL, not merely the "+
				"multiset):\n  seek=%s\n  enum=%s", i, query, got[i], want[i])
		}
	}
	return got
}

// TestExpandIntoSeek_ClosingShapes covers the shapes the seek exists for, each
// against the disabled arm AND an absolute oracle.
func TestExpandIntoSeek_ClosingShapes(t *testing.T) {
	cases := []struct {
		name string
		n    int
		arcs []seekArc
	}{
		{
			// A ring with mutual back-edges, so 2-cycles genuinely exist.
			name: "ring_with_mutual",
			n:    8,
			arcs: func() []seekArc {
				var a []seekArc
				for i := 0; i < 8; i++ {
					a = append(a, seekArc{i, (i + 1) % 8}, seekArc{(i + 1) % 8, i})
				}
				return a
			}(),
		},
		{
			// PARALLEL EDGES between the bound pair, in both directions and with
			// different multiplicities, so a per-instance defect cannot hide behind a
			// symmetric count. Oracle: 3*2 + 2*3 = 12.
			name: "parallel_both_ways",
			n:    4,
			arcs: []seekArc{
				{0, 1}, {0, 1}, {0, 1},
				{1, 0}, {1, 0},
			},
		},
		{
			// Self-loops only. Both hops draw from the same group, so the oracle is the
			// ORDERED-DISTINCT count 3*2 = 6, not 3*3 = 9 — isomorphism forbids reusing
			// the same relationship.
			name: "self_loops",
			n:    3,
			arcs: []seekArc{{0, 0}, {0, 0}, {0, 0}},
		},
		{
			// A node with no edges at all (node 3), so the seek must handle an EMPTY
			// run as a miss rather than walking into the next node's slots.
			name: "isolated_node_present",
			n:    4,
			arcs: []seekArc{{0, 1}, {1, 0}, {1, 2}, {2, 1}},
		},
		{
			// Asymmetric: 0->1 exists but 1->0 does not, so the closing hop must find
			// nothing for that pair while still finding the 1<->2 pair.
			name: "asymmetric",
			n:    4,
			arcs: []seekArc{{0, 1}, {1, 2}, {2, 1}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := seekFixture(t, tc.n, tc.arcs)
			rows := assertSeekIdentical(t, g,
				`MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN a.i AS ai, b.i AS bi`, true)
			if want := oracleTwoCycleCount(tc.arcs); len(rows) != want {
				t.Fatalf("absolute oracle disagrees: engine returned %d rows, the arc list "+
					"implies %d — a defect BOTH arms share", len(rows), want)
			}
		})
	}
}

// TestExpandIntoSeek_Directions covers every traversal direction of a closing hop,
// including the reverse seek, which searches the CSR built by csr.BuildReverse and so
// depends on that builder's by-construction (source, handle) ordering — the invariant
// graph/csr/reverse_order_invariant_test.go pins.
func TestExpandIntoSeek_Directions(t *testing.T) {
	var arcs []seekArc
	for i := 0; i < 6; i++ {
		arcs = append(arcs, seekArc{i, (i + 1) % 6}, seekArc{(i + 1) % 6, i})
	}
	arcs = append(arcs, seekArc{0, 2}, seekArc{0, 2}, seekArc{2, 0})
	g := seekFixture(t, 6, arcs)

	for _, q := range []string{
		`MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN a.i AS ai, b.i AS bi`,      // out, out
		`MATCH (a:P)<-[:K]-(b:P)<-[:K]-(a) RETURN a.i AS ai, b.i AS bi`,      // in, in
		`MATCH (a:P)-[:K]->(b:P)<-[:K]-(a) RETURN a.i AS ai, b.i AS bi`,      // out, in
		`MATCH (a:P)<-[:K]-(b:P)-[:K]->(a) RETURN a.i AS ai, b.i AS bi`,      // in, out
		`MATCH (a:P)-[:K]-(b:P)-[:K]-(a) RETURN a.i AS ai, b.i AS bi`,        // undirected
		`MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*) AS n`, // triangle
		`MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(d:P)-[:K]->(a) RETURN count(*) AS n`,
	} {
		t.Run(q, func(t *testing.T) { assertSeekIdentical(t, g, q, true) })
	}
}

// TestExpandIntoSeek_ReverseCursorIsNarrowed asserts the REVERSE seek engages, which
// no result comparison can establish.
//
// Dropping the reverse narrowing leaves the operator walking the whole in-edge range:
// slower, but the SAME rows in the SAME order. A mutation that did exactly that
// survived every differential case in this file, which is why the reverse narrowing
// has its own counter and its own assertion.
func TestExpandIntoSeek_ReverseCursorIsNarrowed(t *testing.T) {
	var arcs []seekArc
	for i := 0; i < 6; i++ {
		arcs = append(arcs, seekArc{i, (i + 1) % 6}, seekArc{(i + 1) % 6, i})
	}
	g := seekFixture(t, 6, arcs)
	on, _ := seekEngines(g)

	for _, tc := range []struct {
		name    string
		q       string
		wantRev bool
	}{
		// The closing hop is DirIn, so the REVERSE cursor is the one narrowed.
		{"closing_hop_incoming", `MATCH (a:P)<-[:K]-(b:P)<-[:K]-(a) RETURN count(*) AS n`, true},
		{"closing_hop_out_then_in", `MATCH (a:P)-[:K]->(b:P)<-[:K]-(a) RETURN count(*) AS n`, true},
		// Undirected: both cursors are narrowed.
		{"closing_hop_undirected", `MATCH (a:P)-[:K]-(b:P)-[:K]-(a) RETURN count(*) AS n`, true},
		// Purely outgoing: the reverse cursor is never loaded, so there is nothing to
		// narrow and a count here would mean the operator did needless work.
		{"closing_hop_outgoing", `MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN count(*) AS n`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := exec.ExpandIntoSeekReverseCount()
			_ = seekRows(t, on, tc.q)
			got := exec.ExpandIntoSeekReverseCount() - before
			if tc.wantRev && got == 0 {
				t.Fatalf("expected the REVERSE cursor to be narrowed for %q, but it never was", tc.q)
			}
			if !tc.wantRev && got != 0 {
				t.Fatalf("did not expect a reverse narrowing for %q, but got %d", tc.q, got)
			}
		})
	}
}

// TestExpandIntoSeek_RelationshipVariablesAndHandles asserts per-instance identity
// where it is observable: with the relationships PROJECTED, a defect that collapsed a
// parallel-edge group onto one slot, or picked the wrong slot, changes the rows
// rather than only their count.
func TestExpandIntoSeek_RelationshipVariablesAndHandles(t *testing.T) {
	g := seekFixture(t, 4, []seekArc{
		{0, 1}, {0, 1}, {0, 1},
		{1, 0}, {1, 0},
		{2, 3}, {3, 2},
	})
	for _, q := range []string{
		`MATCH (a:P)-[r:K]->(b:P)-[s:K]->(a) RETURN id(r) AS ir, id(s) AS is_ ORDER BY ir, is_`,
		`MATCH (a:P)-[r:K]->(b:P)-[s:K]->(a) RETURN a.i AS ai, b.i AS bi, id(r) AS ir, id(s) AS is_
		   ORDER BY ai, bi, ir, is_`,
		`MATCH (a:P)-[r:K]->(b:P)-[s:K]->(a) RETURN count(DISTINCT id(r)) AS distinctR`,
	} {
		t.Run(q, func(t *testing.T) { assertSeekIdentical(t, g, q, true) })
	}
}

// TestExpandIntoSeek_ExcludedShapesFallBack asserts the seek does NOT engage where
// the design excludes it. These are the cases where a seek would be unsound or
// meaningless, and the assertion that matters is the access path, not the rows:
// falling back keeps the operator's output a SUPERSET and lets the equality Selection
// above decide.
func TestExpandIntoSeek_ExcludedShapesFallBack(t *testing.T) {
	var arcs []seekArc
	for i := 0; i < 6; i++ {
		arcs = append(arcs, seekArc{i, (i + 1) % 6}, seekArc{(i + 1) % 6, i})
	}
	g := seekFixture(t, 6, arcs)

	for _, tc := range []struct {
		name string
		q    string
	}{
		{
			// A variable-length expand is a different operator whose neighbour loop is a
			// BFS ENUMERATION: it must visit every neighbour, so a membership probe is
			// the wrong primitive (settled in #2142).
			name: "varlen",
			q:    `MATCH (a:P)-[:K*1..3]->(b:P) RETURN count(*) AS n`,
		},
		{
			name: "varlen_closing",
			q:    `MATCH (a:P)-[:K*2..2]->(a) RETURN count(*) AS n`,
		},
		{
			// shortestPath / allShortestPaths are distinct operators and never carry
			// IntoVar.
			name: "shortestPath",
			q: `MATCH (a:P {i:0}), (b:P {i:3}), p = shortestPath((a)-[:K*1..6]->(b))
			      RETURN length(p) AS l`,
		},
		{
			name: "allShortestPaths",
			q: `MATCH (a:P {i:0}), (b:P {i:3}), p = allShortestPaths((a)-[:K*1..6]->(b))
			      RETURN count(p) AS n`,
		},
		{
			// No bound destination at all: an ordinary open hop.
			name: "open_hop",
			q:    `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P) RETURN count(*) AS n`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertSeekIdentical(t, g, tc.q, false) })
	}
}

// TestExpandIntoSeek_OptionalMatchNullPaddedRowFallsBack covers the exclusion that is
// enforced at RUN TIME rather than in the planner, and is the reason the run-time
// check is the right place for it.
//
// An OPTIONAL MATCH that fails to match leaves the interior variable NULL. A later
// hop bound to that variable therefore has no key to seek to, so the seek must
// decline for exactly those rows while still engaging for rows where the variable did
// bind. A planner-side veto could not make that distinction — it is a property of the
// row, not of the shape.
func TestExpandIntoSeek_OptionalMatchNullPaddedRowFallsBack(t *testing.T) {
	// Nodes 0..2 form a mutual chain; nodes 3 and 4 are isolated, so the OPTIONAL
	// MATCH yields NULL-padded rows for them.
	g := seekFixture(t, 5, []seekArc{{0, 1}, {1, 0}, {1, 2}, {2, 1}})
	for _, q := range []string{
		`MATCH (a:P) OPTIONAL MATCH (a)-[:K]->(b:P)-[:K]->(a) RETURN a.i AS ai, b.i AS bi
		   ORDER BY ai, bi`,
		`MATCH (a:P) OPTIONAL MATCH (a)-[:K]->(b:P)-[:K]->(a) RETURN count(b) AS matched`,
		`OPTIONAL MATCH (a:P {i:99})-[:K]->(b:P)-[:K]->(a) RETURN a.i AS ai, b.i AS bi`,
	} {
		t.Run(q, func(t *testing.T) {
			on, off := seekEngines(g)
			got, want := seekRows(t, on, q), seekRows(t, off, q)
			if len(got) != len(want) {
				t.Fatalf("row count differs for %q: seek=%d enumerate=%d", q, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("row %d differs for %q:\n  seek=%s\n  enum=%s", i, q, got[i], want[i])
				}
			}
		})
	}
}

// TestExpandIntoSeek_ExplainRendersDistinctly asserts the plan states the access path.
// EXPLAIN proving the plan and ExpandIntoSeekCount proving the path are complementary:
// the first would still pass if the operator silently stopped seeking, the second
// would still pass if the plan became unreadable.
func TestExpandIntoSeek_ExplainRendersDistinctly(t *testing.T) {
	g := seekFixture(t, 5, []seekArc{{0, 1}, {1, 0}, {1, 2}, {2, 1}})
	on, off := seekEngines(g)

	closing := `MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN count(*) AS n`
	openHop := `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P) RETURN count(*) AS n`

	planOn, err := on.Explain(closing, nil)
	if err != nil {
		t.Fatalf("Explain(closing): %v", err)
	}
	if !strings.Contains(planOn, "ExpandInto seek") {
		t.Fatalf("closing plan does not state the seek access path:\n%s", planOn)
	}
	planOff, err := off.Explain(closing, nil)
	if err != nil {
		t.Fatalf("Explain(closing, seek off): %v", err)
	}
	if !strings.Contains(planOff, "ExpandInto filter") {
		t.Fatalf("closing plan with the seek disabled does not state the filter path:\n%s", planOff)
	}
	planOpen, err := on.Explain(openHop, nil)
	if err != nil {
		t.Fatalf("Explain(open): %v", err)
	}
	if strings.Contains(planOpen, "ExpandInto") {
		t.Fatalf("an open hop must not render as ExpandInto:\n%s", planOpen)
	}
}

// TestExpandIntoSeek_Rapid_AllFlagCombinations is the property test the acceptance
// criteria require: over random small multigraphs, every closing shape must return
// the identical row sequence under all four combinations of the two flags this sprint
// introduces or widens, and must agree with the absolute oracle.
//
// Random graphs are what reach the shapes a hand-written case list does not think of
// — a destination whose run is empty, a self-loop that is also a parallel edge, a
// node that is the only member of its run.
func TestExpandIntoSeek_Rapid_AllFlagCombinations(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(2, 7).Draw(rt, "nodes")
		arcCount := rapid.IntRange(0, 18).Draw(rt, "arcCount")
		arcs := make([]seekArc, 0, arcCount)
		for i := 0; i < arcCount; i++ {
			arcs = append(arcs, seekArc{
				from: rapid.IntRange(0, n-1).Draw(rt, fmt.Sprintf("from%d", i)),
				to:   rapid.IntRange(0, n-1).Draw(rt, fmt.Sprintf("to%d", i)),
			})
		}
		g := seekFixture(unwrapT(rt, t), n, arcs)

		type arm struct {
			name string
			opts EngineOptions
		}
		arms := []arm{
			{"seek+swap", EngineOptions{}},
			{"seek-only", EngineOptions{DisableAnchorSwap: true}},
			{"swap-only", EngineOptions{DisableExpandIntoSeek: true}},
			{"neither", EngineOptions{DisableExpandIntoSeek: true, DisableAnchorSwap: true}},
		}
		queries := []string{
			`MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN a.i AS ai, b.i AS bi ORDER BY ai, bi`,
			`MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*) AS n`,
			`MATCH (a:P)<-[:K]-(b:P)-[:K]->(a) RETURN count(*) AS n`,
			`MATCH (a:P)-[:K]->(a) RETURN count(*) AS n`,
		}
		for _, q := range queries {
			var reference []string
			for _, a := range arms {
				eng := NewEngineWithOptions(g, a.opts)
				rows := seekRowsRapid(rt, eng, q)
				if reference == nil {
					reference = rows
					continue
				}
				if len(rows) != len(reference) {
					rt.Fatalf("arm %s row count differs for %q: %d vs %d (arcs=%v)",
						a.name, q, len(rows), len(reference), arcs)
				}
				for i := range rows {
					if rows[i] != reference[i] {
						rt.Fatalf("arm %s row %d differs for %q: %s vs %s (arcs=%v)",
							a.name, i, q, rows[i], reference[i], arcs)
					}
				}
			}
		}

		// Absolute oracle on the closing count, so a defect every arm shares is caught.
		eng := NewEngineWithOptions(g, EngineOptions{})
		rows := seekRowsRapid(rt, eng, `MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN a.i AS ai, b.i AS bi`)
		if want := oracleTwoCycleCount(arcs); len(rows) != want {
			rt.Fatalf("absolute oracle disagrees: engine %d rows, arc list implies %d (arcs=%v)",
				len(rows), want, arcs)
		}
	})
}

// seekRowsRapid is seekRows against a *rapid.T, whose failures must be reported
// through rapid so it can shrink the counterexample.
func seekRowsRapid(rt *rapid.T, eng *Engine, query string) []string {
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		rt.Fatalf("Run(%q): %v", query, err)
	}
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		rec := res.Record()
		keys := make([]string, 0, len(rec))
		for k := range rec {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&sb, "%s=%v;", k, rec[k])
		}
		out = append(out, sb.String())
	}
	if err := res.Err(); err != nil {
		rt.Fatalf("Err(%q): %v", query, err)
	}
	return out
}

// unwrapT lets the fixture builder, which takes *testing.T, be used from inside a
// rapid property. A fixture failure is a defect in the TEST, not a shrinkable
// counterexample, so failing the outer T is the correct behaviour.
func unwrapT(_ *rapid.T, t *testing.T) *testing.T { return t }
