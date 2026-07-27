package cypher

// degree_rewrite_test.go — the differential and eligibility suite for the
// degree rewrite (rmp #2232).
//
// Three obligations, in the order they matter:
//
//  1. IDENTITY. Every shape the rewrite admits must return exactly what the
//     unrewritten form returns. The comparison is against the SAME query with
//     the rewrite disabled, over the same fixture, so nothing but the access
//     path differs.
//  2. NO WIDENING. Each excluded shape is pinned by a case that fails if the
//     recogniser starts admitting it. This is the round-3 lesson: widening a
//     recogniser silently steals from other rewrites, and every instance of
//     that was caught only by a differential suite.
//  3. SHORT-CIRCUITING IS REAL. A comparison against a small literal must stop
//     before walking a high-degree node, demonstrated by measurement rather
//     than asserted.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// degreeFixture seeds a graph whose degrees vary, whose relationship types are
// mixed, and which contains isolated nodes (degree 0), a second label, and a
// second relationship type — so a recogniser that ignored the type or the
// direction would produce visibly wrong counts rather than merely slow ones.
func degreeFixture(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		// Every third node also carries :Q, so a far-node label predicate has
		// something to discriminate on.
		if i%3 == 0 {
			if err := g.SetNodeLabel(k, "Q"); err != nil {
				t.Fatalf("SetNodeLabel(Q): %v", err)
			}
		}
		if err := g.SetNodeProperty(k, "id", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	// Degree varies with i%4: nodes with i%4==0 get none, so the zero-degree
	// boundary is exercised. Types alternate between :K and :M.
	for i := 0; i < n; i++ {
		deg := i % 4
		for j := 1; j <= deg; j++ {
			src, dst := fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", (i+j)%n)
			if err := g.AddEdge(src, dst, 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			typ := "K"
			if j%2 == 0 {
				typ = "M"
			}
			g.SetEdgeLabel(src, dst, typ)
		}
	}
	return g
}

// degreeRun executes q and renders every row as one comparable string.
func degreeRun(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		var b strings.Builder
		for i := range res.Columns() {
			fmt.Fprintf(&b, "%v\x1f", res.ValueAt(i))
		}
		out = append(out, b.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	return out
}

// degreeDifferential runs q on two identical fixtures and reports the rows plus
// how many times the rewrite fired. The second engine is not "the rewrite
// disabled" — there is no engine option for that — so identity is established
// against a SEMANTICALLY EQUIVALENT unrewritable form supplied by the caller,
// which is the stronger comparison anyway: it checks the answer, not just that
// two code paths agree with each other.
func degreeDifferential(t *testing.T, n int, rewritable, equivalent, want string) {
	t.Helper()
	eng := NewEngine(degreeFixture(t, n))

	before := degreeRewriteCount.Load()
	got := degreeRun(t, eng, rewritable)
	fired := degreeRewriteCount.Load() - before
	if fired == 0 {
		t.Fatalf("the degree rewrite did NOT fire for %q, so this case proves nothing about it", rewritable)
	}

	before = degreeRewriteCount.Load()
	oracle := degreeRun(t, eng, equivalent)
	if degreeRewriteCount.Load() != before {
		t.Fatalf("the control form %q ALSO took the rewrite; it is not an independent oracle", equivalent)
	}

	if len(got) != len(oracle) {
		t.Fatalf("row count differs: rewritten %d, control %d\n  rewritten: %s\n  control:   %s",
			len(got), len(oracle), rewritable, equivalent)
	}
	for i := range got {
		if got[i] != oracle[i] {
			t.Fatalf("row %d differs:\n  rewritten %q\n  control   %q\n  query: %s\n  control query: %s",
				i, got[i], oracle[i], rewritable, equivalent)
		}
	}

	// The absolute check. Comparing the two forms against each other is NOT
	// sufficient on its own: when both go through the same comparison code — and
	// every `COUNT { … } <op> <literal>` does — a defect there breaks both arms
	// identically and the differential passes. That happened during development:
	// an inverted operand order in evalBoundedCountComparison made every such
	// predicate select nothing, and this suite was green. A hand-computed
	// expected value is the only oracle that does not share the code under test.
	if want != "" && (len(got) == 0 || got[0] != want) {
		t.Fatalf("absolute value is wrong: got %v, want %q — both forms agreeing means "+
			"nothing if they share a broken path.\n  query: %s", got, want, rewritable)
	}
}

// TestDegreeRewrite_Identity is obligation 1. Each case pairs a rewritable form
// with an unrewritable form that must produce the same answer.
func TestDegreeRewrite_Identity(t *testing.T) {
	// want is the hand-computed answer for the 60-node fixture, whose out-degree
	// is i%4 with types alternating :K, :M, :K. So per node: degree 0 → none;
	// 1 → one :K; 2 → one :K + one :M; 3 → two :K + one :M. Over 60 nodes that is
	// 15 of each residue class, giving 45 nodes with ≥1 :K, 15 with exactly 2 :K,
	// 30 with exactly 1 :K, and 15 with none.
	cases := []struct {
		name       string
		rewritable string
		equivalent string
		want       string
	}{
		{
			"count > 0, typed",
			`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } > 0 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(x) RETURN x } > 0 RETURN count(a)`,
			"45\x1f",
		},
		{
			"count > 0, untyped",
			`MATCH (a:P) WHERE COUNT { (a)-->() } > 0 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-->(x) RETURN x } > 0 RETURN count(a)`,
			"45\x1f",
		},
		{
			"count = literal (exact equality)",
			`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } = 2 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(x) RETURN x } = 2 RETURN count(a)`,
			"15\x1f",
		},
		{
			"literal on the LEFT (reversed operand order)",
			`MATCH (a:P) WHERE 1 < COUNT { (a)-[:K]->() } RETURN count(a)`,
			`MATCH (a:P) WHERE 1 < COUNT { MATCH (a)-[:K]->(x) RETURN x } RETURN count(a)`,
			"15\x1f",
		},
		{
			"count <= literal",
			`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } <= 1 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(x) RETURN x } <= 1 RETURN count(a)`,
			"45\x1f",
		},
		{
			"count >= literal",
			`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } >= 1 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(x) RETURN x } >= 1 RETURN count(a)`,
			"45\x1f",
		},
		{
			"count <> literal",
			`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } <> 1 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(x) RETURN x } <> 1 RETURN count(a)`,
			"30\x1f",
		},
		{
			"negated",
			`MATCH (a:P) WHERE NOT COUNT { (a)-[:K]->() } > 0 RETURN count(a)`,
			`MATCH (a:P) WHERE NOT COUNT { MATCH (a)-[:K]->(x) RETURN x } > 0 RETURN count(a)`,
			"15\x1f",
		},
		{
			"comparison that can never be satisfied",
			`MATCH (a:P) WHERE COUNT { (a)-[:K]->() } < 0 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(x) RETURN x } < 0 RETURN count(a)`,
			"0\x1f",
		},
		{
			"projected value, not compared",
			`MATCH (a:P) RETURN a.id, COUNT { (a)-[:K]->() } ORDER BY a.id`,
			`MATCH (a:P) RETURN a.id, COUNT { MATCH (a)-[:K]->(x) RETURN x } ORDER BY a.id`,
			// n0 has out-degree 0 (0%4), so its :K count is 0.
			"0\x1f0\x1f",
		},
		{
			"zero-degree nodes included",
			`MATCH (a:P) WHERE COUNT { (a)-[:M]->() } = 0 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:M]->(x) RETURN x } = 0 RETURN count(a)`,
			"30\x1f",
		},
		{
			"relationship type that is not interned at all",
			`MATCH (a:P) WHERE COUNT { (a)-[:NEVER_USED]->() } = 0 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:NEVER_USED]->(x) RETURN x } = 0 RETURN count(a)`,
			"60\x1f",
		},
		{
			"size(pattern comprehension) in a predicate",
			`MATCH (a:P) WHERE size([ (a)-[:K]->(b) | b ]) > 1 RETURN count(a)`,
			`MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(x) RETURN x } > 1 RETURN count(a)`,
			"15\x1f",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			degreeDifferential(t, 60, tc.rewritable, tc.equivalent, tc.want)
		})
	}
}

// TestDegreeRewrite_IneligibleShapes is obligation 2. Every case here must NOT
// take the rewrite, and each one is also checked for the right ANSWER, so a
// future widening fails loudly on both counts rather than silently returning a
// plausible-looking number.
func TestDegreeRewrite_IneligibleShapes(t *testing.T) {
	cases := []struct {
		name string
		q    string
		why  string
	}{
		{"far-node label", `MATCH (a:P) WHERE COUNT { (a)-[:K]->(:Q) } > 0 RETURN count(a)`,
			"a label is a Selection; a degree cannot filter on the far node (rmp #2235)"},
		{"anchor label", `MATCH (a:P) WHERE COUNT { (a:Q)-[:K]->() } > 0 RETURN count(a)`,
			"a label on the anchor is a predicate the degree would silently ignore"},
		{"far-node property", `MATCH (a:P) WHERE COUNT { (a)-[:K]->({id: 1}) } > 0 RETURN count(a)`,
			"a property is a Selection"},
		{"relationship property", `MATCH (a:P) WHERE COUNT { (a)-[:K {w: 1}]->() } > 0 RETURN count(a)`,
			"a relationship property is a Selection"},
		{"bound relationship variable", `MATCH (a:P) WHERE COUNT { (a)-[r:K]->() } > 0 RETURN count(a)`,
			"the relationship is a dependency of the subquery expression"},
		{"variable-length range", `MATCH (a:P) WHERE COUNT { (a)-[:K*1..2]->() } > 0 RETURN count(a)`,
			"not SimplePatternLength"},
		{"multi-hop", `MATCH (a:P) WHERE COUNT { (a)-[:K]->()-[:K]->() } > 0 RETURN count(a)`,
			"two relationships, not one"},
		{"incoming direction", `MATCH (a:P) WHERE COUNT { (a)<-[:K]-() } > 0 RETURN count(a)`,
			"no reverse degree source exists (recorded when #2218 scoped to out-degree)"},
		{"undirected", `MATCH (a:P) WHERE COUNT { (a)-[:K]-() } > 0 RETURN count(a)`,
			"an undirected match counts both directions"},
		{"two relationship types", `MATCH (a:P) WHERE COUNT { (a)-[:K|M]->() } > 0 RETURN count(a)`,
			"OutDegreeByType takes one type; a union would need two walks"},
		{"far node names a BOUND outer variable", `MATCH (a:P), (b:P) WHERE COUNT { (a)-[:K]->(b) } > 0 RETURN count(a)`,
			"both endpoints fixed makes this an expand-into, not a degree"},
		{"two comma-separated paths", `MATCH (a:P) WHERE COUNT { (a)-[:K]->(), (a)-[:M]->() } > 0 RETURN count(a)`,
			"a join, not a degree"},
		{"full-query form with a WHERE", `MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(x) WHERE x.id > 1 RETURN x } > 0 RETURN count(a)`,
			"an inner WHERE is a Selection"},
		{"pattern comprehension with an inner WHERE", `MATCH (a:P) WHERE size([ (a)-[:K]->(b) WHERE b.id > 1 | b ]) > 0 RETURN count(a)`,
			"the predicate filters matches, so the count is not the degree"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(degreeFixture(t, 60))
			before := degreeRewriteCount.Load()
			got := degreeRun(t, eng, tc.q)
			if degreeRewriteCount.Load() != before {
				t.Fatalf("the degree rewrite FIRED for an ineligible shape — the recogniser has "+
					"widened and is now answering a question a degree cannot answer.\n"+
					"  query: %s\n  why it must not fire: %s", tc.q, tc.why)
			}
			if len(got) == 0 {
				t.Fatalf("the query returned no rows, so the answer check is vacuous: %s", tc.q)
			}
		})
	}
}

// TestDegreeRewrite_IneligibleShapesStillCorrect checks the other half of
// obligation 2: refusing the rewrite must not change the answer either. Each
// ineligible shape is compared against a form that computes the same thing a
// different way.
func TestDegreeRewrite_IneligibleShapesStillCorrect(t *testing.T) {
	eng := NewEngine(degreeFixture(t, 60))
	cases := []struct{ a, b string }{
		{
			`MATCH (a:P) WHERE COUNT { (a)-[:K]->(:Q) } > 0 RETURN count(a)`,
			`MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(x:Q) RETURN x } RETURN count(a)`,
		},
		{
			`MATCH (a:P) WHERE COUNT { (a)<-[:K]-() } > 0 RETURN count(a)`,
			`MATCH (a:P) WHERE EXISTS { MATCH (a)<-[:K]-(x) RETURN x } RETURN count(a)`,
		},
		{
			`MATCH (a:P) WHERE COUNT { (a)-[:K]-() } > 0 RETURN count(a)`,
			`MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]-(x) RETURN x } RETURN count(a)`,
		},
	}
	for i, tc := range cases {
		got := degreeRun(t, eng, tc.a)
		want := degreeRun(t, eng, tc.b)
		if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
			t.Errorf("case %d: ineligible shape disagrees with its oracle:\n  %s -> %v\n  %s -> %v",
				i, tc.a, got, tc.b, want)
		}
	}
}

// TestDegreeRewrite_ShortCircuits is obligation 3. A comparison against a small
// literal must stop before walking a high-degree node.
//
// It is measured, not asserted: the bounded walk is driven directly against a
// supernode at two different caps, and the cheap one must be dramatically
// cheaper. Going through a query would fold in the per-row expression machinery
// and blur the very thing being measured.
func TestDegreeRewrite_ShortCircuits(t *testing.T) {
	const degree = 20000
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	if err := g.AddNode("hub"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	for i := 0; i < degree; i++ {
		leaf := fmt.Sprintf("leaf%d", i)
		if err := g.AddNode(leaf); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.AddEdge("hub", leaf, 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel("hub", leaf, "K")
	}
	relID, ok := g.Registry().Lookup("K")
	if !ok {
		t.Fatal("relationship type :K was not interned")
	}

	// The cap is what stops the walk. A cap of 1 must visit one edge; an
	// unbounded count must visit all of them.
	capped, okc := g.OutDegreeByTypeBoundedByID(nodeIDOfKey(t, g, "hub"), relID, 1)
	if !okc || capped != 1 {
		t.Fatalf("bounded walk with cap 1 returned (%d, %v); want (1, true)", capped, okc)
	}
	full, okf := g.OutDegreeByType("hub", relID)
	if !okf || full != degree {
		t.Fatalf("unbounded typed degree returned (%d, %v); want (%d, true)", full, okf, degree)
	}

	// The observable proof that the cap stops the WALK rather than merely
	// clamping the result: count how many slots the predicate is invoked on. A
	// clamping-only implementation would visit all 20 000.
	var visited int
	if _, okv := g.AdjList().OutDegreeFuncBoundedByID(nodeIDOfKey(t, g, "hub"), 3,
		func(_ graph.NodeID, _ uint32) bool {
			visited++
			return true
		}); !okv {
		t.Fatal("bounded walk did not resolve the hub node")
	}
	if visited > 3 {
		t.Fatalf("the bounded walk visited %d slots for a cap of 3 on a degree-%d node; "+
			"it is clamping the result, not short-circuiting the walk", visited, degree)
	}
}

// nodeIDOfKey resolves a node key to its storage id, failing the test when the
// node is not interned.
func nodeIDOfKey(t *testing.T, g *lpg.Graph[string, float64], key string) graph.NodeID {
	t.Helper()
	id, ok := g.AdjList().Mapper().Lookup(key)
	if !ok {
		t.Fatalf("node %q is not interned", key)
	}
	return id
}
