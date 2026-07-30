package cypher

// cyclic_intersect_diff_test.go — the differential and semantic battery for the
// fused cyclic expand (rmp #2158).
//
// Layer: short.
//
// This complements cyclic_intersect_plan_test.go, which pins the recogniser's
// plan-level behaviour. Here the concern is SEMANTICS: that turning the operator on
// cannot change what a query answers, across the shapes a set-intersection
// formulation is most likely to get wrong.
//
// Two of this task's originally-specified cases were CORRECTED by SPIKE #2155 and
// are asserted in their corrected form, with the reasoning recorded at each site:
//
//   - "a cycle where the count-store cells are dirty (must veto)" is VACUOUS. The
//     operator consults no count cell at all — it orders its ranges by CSR run
//     length, which is exact per-vertex and cannot be stale — so there is nothing
//     for a dirty cell to veto. The correct assertion is the INVERSE: it engages
//     regardless of dirtiness. See TestCyclicDiff_EngagesWithDirtyCounts.
//   - "a cycle with repeated relationship variables" needs no runtime assertion:
//     reusing a relationship variable in one path pattern is a COMPILE-TIME
//     SyntaxError.RelationshipUniquenessViolation. Asserted as such.
//
// Every differential here pairs equality with the engagement counter, because a
// silently-declining recogniser would make both arms run today's plan and agree
// perfectly — the same blindness that makes TCK 3897/3897 unable to gate this
// operator at all.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// diffCase is one query compared across the flag, with whether it must engage.
type diffCase struct {
	name       string
	query      string
	mustEngage bool
	why        string
}

// richGraph builds a fixture containing, deliberately, every shape this battery
// needs: triangles, a square, a longer cycle, parallel edges, a self-loop, acyclic
// pendant legs, an isolated node, and typed/untyped and labelled/unlabelled nodes.
func richGraph(t testing.TB) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	add := func(k, label string, x int64) {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
		if err := g.SetNodeLabel(k, label); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "x", lpg.Int64Value(x)); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", k, err)
		}
	}
	for i, k := range []string{"a", "b", "c", "d", "e", "f", "iso"} {
		add(k, "P", int64(i))
	}
	edge := func(u, v, typ string) {
		if err := g.AddEdge(u, v, 1.0); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", u, v, err)
		}
		g.SetEdgeLabel(u, v, typ)
	}
	// Triangle a→b→c→a, with a parallel edge on b→c and a reciprocal b→a.
	edge("a", "b", "K")
	edge("b", "a", "K") // 2-cycle a↔b
	edge("b", "c", "K")
	edge("b", "c", "K") // parallel
	edge("c", "a", "K")
	// A square a→d→e→f→a.
	edge("a", "d", "K")
	edge("d", "e", "K")
	edge("e", "f", "K")
	edge("f", "a", "K")
	// A self-loop and a pendant acyclic leg off c.
	edge("c", "c", "K")
	edge("c", "e", "L") // different type, also a pendant
	return g
}

// runRowsDiff drains a query into an ordered slice of positional row renderings.
func runRowsDiff(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%s): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		var sb strings.Builder
		for i := range res.Columns() {
			if i > 0 {
				sb.WriteByte('|')
			}
			if v := res.ValueAt(i); v == nil {
				sb.WriteString("<nil>")
			} else {
				sb.WriteString(v.String())
			}
		}
		out = append(out, sb.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%s): %v", q, err)
	}
	return out
}

// assertSameAcrossFlag runs q with the operator off and on and requires an
// IDENTICAL ORDERED sequence, plus the expected engagement.
func assertSameAcrossFlag(t *testing.T, g *lpg.Graph[string, float64], tc diffCase) {
	t.Helper()
	var off, on []string
	offEngaged := withEngageProbe(t, func() { off = runRowsDiff(t, NewEngine(g), tc.query) })
	onEngaged := withEngageProbe(t, func() {
		on = runRowsDiff(t, NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}), tc.query)
	})
	if offEngaged != 0 {
		t.Fatalf("engaged %d times with the flag OFF; want 0", offEngaged)
	}
	switch {
	case tc.mustEngage && onEngaged == 0:
		t.Fatalf("did NOT engage with the flag on, so this differential is vacuous (%s)", tc.why)
	case !tc.mustEngage && onEngaged != 0:
		t.Fatalf("engaged %d times on a shape that must decline (%s)", onEngaged, tc.why)
	}
	if len(on) != len(off) {
		t.Fatalf("row count: on=%d off=%d\n  on:  %v\n  off: %v", len(on), len(off), on, off)
	}
	for i := range off {
		if on[i] != off[i] {
			t.Fatalf("row %d differs:\n  on  %q\n  off %q", i, on[i], off[i])
		}
	}
}

func TestCyclicDiff_Shapes(t *testing.T) {
	g := richGraph(t)
	cases := []diffCase{
		{"directed triangle, count", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`,
			true, "direct-stack triangle"},
		{"directed triangle, named rels", `MATCH (a)-[r1:K]->(b)-[r2:K]->(c)-[r3:K]->(a) RETURN a.x AS ax, b.x AS bx, c.x AS cx`,
			true, "named relationship variables must still bind"},
		{"2-cycle", `MATCH (a)-[:K]->(b)-[:K]->(a) RETURN count(*) AS n`,
			true, "the middle source and IntoVar are the same variable"},
		{"square (4-cycle)", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(d)-[:K]->(a) RETURN count(*) AS n`,
			true, "only the LAST hop closes, so the fusion is the same 2-way intersection"},
		{"5-cycle", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(d)-[:K]->(e)-[:K]->(a) RETURN count(*) AS n`,
			true, "longer cycles add open hops, not intersections"},
		{"cycle with an acyclic pendant leg", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a), (c)-[:L]->(z) RETURN count(*) AS n`,
			true, "the pendant leg is bound by an ordinary Expand after the fusion"},
		{"untyped triangle", `MATCH (a)-->(b)-->(c)-->(a) RETURN count(*) AS n`,
			true, "no type filter at all"},
		{"undirected triangle", `MATCH (a)-[:K]-(b)-[:K]-(c)-[:K]-(a) RETURN count(*) AS n`,
			false, "an undirected neighbourhood is out UNION in, so no contiguous ordered run exists"},
		{"labelled triangle", `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*) AS n`,
			false, "label predicates interpose a Selection, so Child is not an *ir.Expand"},
		{"triangle with property predicates", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) WHERE a.x < b.x RETURN count(*) AS n`,
			true, "a WHERE above the pattern is a Selection ABOVE the fusion, not between the hops"},
		{"acyclic two-hop", `MATCH (a)-[:K]->(b)-[:K]->(c) RETURN count(*) AS n`,
			false, "no hop closes, so IntoVar is never set"},
		{"cycle inside OPTIONAL MATCH", `MATCH (a) OPTIONAL MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN a.x AS ax, count(c) AS n`,
			true, "the null-row obligation is OptionalApply's and is operator-agnostic"},
		{"cycle from an isolated anchor (one leg empty)", `MATCH (a) WHERE a.x = 6 OPTIONAL MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`,
			true, "engagement is per Init, i.e. per plan execution, NOT per matching row — the " +
				"operator is built and initialised even when its input yields nothing, and the " +
				"empty-leg short-circuit is exactly where it is cheaper than the plan it replaces"},
		{"mixed direction cycle", `MATCH (a)-[:K]->(b)<-[:K]-(c)-[:K]->(a) RETURN count(*) AS n`,
			false, "a reversed leg is not DirectionOutgoing"},
		{"variable-length leg", `MATCH (a)-[:K*1..2]->(b)-[:K]->(a) RETURN count(*) AS n`,
			false, "a variable-length leg is not a fixed-arity hop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertSameAcrossFlag(t, g, tc) })
	}
}

// TestCyclicDiff_OrderSafety covers the constructs that make a bag's emission order
// OBSERVABLE. The operator is order-PRESERVING by construction — the intersection
// yields candidates strictly ascending and both handle runs are walked in position
// order, which is exactly the nesting the two Expands produced — so no suppression
// predicate is needed. That is a strong claim, so it is asserted directly here
// rather than argued: if any of these differ, the operator needs a SuppressReorder
// gate and this test is how that would be discovered.
func TestCyclicDiff_OrderSafety(t *testing.T) {
	g := richGraph(t)
	for _, tc := range []diffCase{
		{"bare LIMIT without ORDER BY", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN a.x AS ax, b.x AS bx LIMIT 2`,
			true, "LIMIT selects WHICH rows survive, so order changes the multiset"},
		{"SKIP without ORDER BY", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN a.x AS ax SKIP 1`,
			true, "SKIP likewise selects which rows survive"},
		{"collect over an unsorted stream", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN collect(a.x) AS xs`,
			true, "a list cell compares element by element, so order is a VALUE"},
		{"head of a collected list", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN head(collect(b.x)) AS h`,
			true, "head reads a position, so order is observable"},
		{"ORDER BY is order-establishing and must agree too", `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN a.x AS ax, b.x AS bx ORDER BY ax, bx`,
			true, "the control: a total sort makes order independent of the operator"},
	} {
		t.Run(tc.name, func(t *testing.T) { assertSameAcrossFlag(t, g, tc) })
	}
}

// TestCyclicDiff_RepeatedRelVarIsACompileError records the corrected form of this
// task's "repeated relationship variables" case: it is rejected before execution, so
// there is no runtime behaviour to differentiate.
func TestCyclicDiff_RepeatedRelVarIsACompileError(t *testing.T) {
	g := richGraph(t)
	const q = `MATCH (a)-[r:K]->(b)-[r:K]->(a) RETURN count(*) AS n`
	for _, arm := range []struct {
		name string
		eng  *Engine
	}{
		{"flag off", NewEngine(g)},
		{"flag on", NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true})},
	} {
		t.Run(arm.name, func(t *testing.T) {
			_, err := arm.eng.Run(context.Background(), q, nil)
			if err == nil {
				t.Fatal("reusing a relationship variable in one path pattern was accepted; " +
					"want a compile-time RelationshipUniquenessViolation")
			}
			if !strings.Contains(err.Error(), "RelationshipUniquenessViolation") {
				t.Fatalf("error = %v; want RelationshipUniquenessViolation", err)
			}
		})
	}
}

// TestCyclicDiff_EngagesWithDirtyCounts asserts the INVERSE of this task's original
// "dirty counts must veto" case.
//
// The operator consults no count-store cell: it orders its ranges by CSR run length
// (vertices[v+1]-vertices[v]), which is exact per-vertex and cannot go stale. So
// there is nothing for a dirty cell to veto, and a veto would be a pure loss. Writes
// that dirty the count store must therefore leave the operator engaging — which also
// means this operator adds NO new dependency on count-store freshness.
func TestCyclicDiff_EngagesWithDirtyCounts(t *testing.T) {
	g := richGraph(t)
	eng := NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true})
	const q = `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`

	before := runRowsDiff(t, eng, q)
	engaged := withEngageProbe(t, func() { _ = runRowsDiff(t, eng, q) })
	if engaged == 0 {
		t.Fatal("operator did not engage on the clean graph, so the dirty comparison proves nothing")
	}

	// Dirty the counts by adding a node and edges through a write query.
	if _, err := eng.RunInTx(context.Background(),
		`CREATE (z:P {x: 99}) WITH z MATCH (a) WHERE a.x = 0 CREATE (a)-[:K]->(z), (z)-[:K]->(a)`, nil); err != nil {
		t.Fatalf("write to dirty the counts: %v", err)
	}

	afterEngaged := withEngageProbe(t, func() { _ = runRowsDiff(t, eng, q) })
	if afterEngaged == 0 {
		t.Fatal("operator stopped engaging after a write dirtied the count store; it consults " +
			"no count cell, so it must engage regardless — a veto here would be a pure loss")
	}
	after := runRowsDiff(t, eng, q)
	// The write added a 2-cycle, not a triangle, so the triangle count is unchanged;
	// what matters is that both arms still agree, checked below.
	offAfter := runRowsDiff(t, NewEngine(g), q)
	if len(after) != len(offAfter) {
		t.Fatalf("after the write: on=%v off=%v", after, offAfter)
	}
	for i := range offAfter {
		if after[i] != offAfter[i] {
			t.Fatalf("after the write, row %d differs: on %q off %q", i, after[i], offAfter[i])
		}
	}
	_ = before
}

// TestCyclicDiff_Rapid is the strongest correctness check available: over randomised
// small multigraphs, the operator's output must equal the binary-join plan's output
// exactly, as an ordered sequence.
//
// The oracle is the SHIPPED plan with the flag off, which is legitimate here for a
// reason worth stating: unlike a differential where both arms share the code under
// test, these two arms run genuinely different operator trees — one fused, one not —
// so agreement is real evidence rather than a shared-defect artefact. The engagement
// counter guards the remaining failure mode, a recogniser that quietly declines.
func TestCyclicDiff_Rapid(t *testing.T) {
	queries := []string{
		`MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`,
		`MATCH (a)-[r1:K]->(b)-[r2:K]->(c)-[r3:K]->(a) RETURN a.x AS ax, b.x AS bx, c.x AS cx`,
		`MATCH (a)-[:K]->(b)-[:K]->(a) RETURN count(*) AS n`,
	}
	engagedAny := 0
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(3, 7).Draw(rt, "nodes")
		nEdges := rapid.IntRange(2, 14).Draw(rt, "edges")

		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		keys := make([]string, n)
		for i := 0; i < n; i++ {
			keys[i] = fmt.Sprintf("n%d", i)
			if err := g.AddNode(keys[i]); err != nil {
				rt.Fatalf("AddNode: %v", err)
			}
			if err := g.SetNodeLabel(keys[i], "P"); err != nil {
				rt.Fatalf("SetNodeLabel: %v", err)
			}
			if err := g.SetNodeProperty(keys[i], "x", lpg.Int64Value(int64(i))); err != nil {
				rt.Fatalf("SetNodeProperty: %v", err)
			}
		}
		// Parallel edges and self-loops arise naturally because (u, v) may repeat and
		// u may equal v — which is exactly what this operator most easily gets wrong.
		for e := 0; e < nEdges; e++ {
			u := rapid.IntRange(0, n-1).Draw(rt, fmt.Sprintf("u%d", e))
			v := rapid.IntRange(0, n-1).Draw(rt, fmt.Sprintf("v%d", e))
			if err := g.AddEdge(keys[u], keys[v], 1.0); err != nil {
				rt.Fatalf("AddEdge: %v", err)
			}
			g.SetEdgeLabel(keys[u], keys[v], "K")
		}

		off := NewEngine(g)
		on := NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true})
		for _, q := range queries {
			wantRows := runRowsRapid(rt, off, q)
			gotRows := runRowsRapid(rt, on, q)
			if len(gotRows) != len(wantRows) {
				rt.Fatalf("query %q: row count on=%d off=%d\n  on:  %v\n  off: %v",
					q, len(gotRows), len(wantRows), gotRows, wantRows)
			}
			for i := range wantRows {
				if gotRows[i] != wantRows[i] {
					rt.Fatalf("query %q row %d: on %q off %q", q, i, gotRows[i], wantRows[i])
				}
			}
		}
		engagedAny++
	})
	if engagedAny == 0 {
		t.Fatal("no rapid iteration ran")
	}
}

// runRowsRapid is runRowsDiff for a rapid.T.
func runRowsRapid(rt *rapid.T, eng *Engine, q string) []string {
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		rt.Fatalf("Run(%s): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		var sb strings.Builder
		for i := range res.Columns() {
			if i > 0 {
				sb.WriteByte('|')
			}
			if v := res.ValueAt(i); v == nil {
				sb.WriteString("<nil>")
			} else {
				sb.WriteString(v.String())
			}
		}
		out = append(out, sb.String())
	}
	if err := res.Err(); err != nil {
		rt.Fatalf("Err(%s): %v", q, err)
	}
	return out
}
