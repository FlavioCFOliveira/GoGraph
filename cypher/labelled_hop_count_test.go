package cypher

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labelledHopDifferential is [degreeDifferential] for the labelled single-hop
// count (rmp #2235), and it carries one extra obligation.
//
// Besides establishing identity against a semantically equivalent UNREWRITABLE
// form and against a hand-computed absolute value, it asserts that
// degreeRewriteCount did NOT move. That is acceptance criterion 3 discharged by
// construction rather than by inspection: if this optimisation had been built by
// widening #2232's recogniser, the degree counter would fire here and every case
// below would fail.
func labelledHopDifferential(t *testing.T, g *lpg.Graph[string, float64], rewritable, equivalent, want string) {
	t.Helper()
	// NOT parallel, and neither is any test in this file: labelledHopRewriteCount
	// and degreeRewriteCount are process-wide, so a concurrent test firing either
	// one corrupts every delta read here. Running them in parallel first made
	// nine cases fail against counts another subtest had produced.
	eng := NewEngine(g)

	beforeHop := labelledHopRewriteCount.Load()
	beforeDeg := degreeRewriteCount.Load()
	got := degreeRun(t, eng, rewritable)
	fired := labelledHopRewriteCount.Load() - beforeHop
	degFired := degreeRewriteCount.Load() - beforeDeg

	if fired == 0 {
		t.Fatalf("the labelled-hop count did NOT fire for %q, so this case proves nothing about it", rewritable)
	}
	if degFired != 0 {
		t.Fatalf("the DEGREE rewrite fired %d time(s) for %q — a labelled far node must "+
			"stay ineligible for it (rmp #2235 AC 3)", degFired, rewritable)
	}

	beforeHop = labelledHopRewriteCount.Load()
	oracle := degreeRun(t, eng, equivalent)
	if labelledHopRewriteCount.Load() != beforeHop {
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

	// The absolute oracle. Two forms agreeing proves nothing when both share the
	// comparison code beneath them — the lesson #2232's harness records after an
	// inverted operand order made every bounded-count predicate select nothing
	// while its differential stayed green.
	if want != "" && (len(got) == 0 || got[0] != want) {
		t.Fatalf("absolute value is wrong: got %v, want %q\n  query: %s", got, want, rewritable)
	}
}

// TestLabelledHopCount_Identity is acceptance criterion 2. Each case pairs the
// rewritable `(a)-[:K]->(:L)` form with an unrewritable one that must produce
// the same answer.
//
// The control is deliberately the FULL subquery form,
// `COUNT { MATCH … WHERE … RETURN b }`, and not the more obvious pattern form
// `COUNT { (a)-[:K]->(b) WHERE b:Q }`. The latter is not a valid oracle: the
// parser drops the inline WHERE of a COUNT pattern subquery
// (VisitSubqueryCount builds ast.CountSubquery without pat.Where, where the
// EXISTS sibling sets it), so that spelling silently answers the unfiltered
// count. The absolute oracle in labelledHopDifferential is what caught it —
// the two forms disagreed 1 against 2, and the hand-computed value said 1.
// Filed separately; see the sprint 327 notes.
func TestLabelledHopCount_Identity(t *testing.T) {

	cases := []struct {
		name       string
		rewritable string
		equivalent string
		want       string
	}{
		{
			// n0 has degree 0 (i%4), so no neighbour matches anything.
			name:       "zero matching neighbours",
			rewritable: "MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->(:Q) }",
			equivalent: "MATCH (a:P {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(b) WHERE b:Q RETURN b }",
			want:       "0\x1f",
		},
		{
			// n3 has degree 3: two :K edges and one :M. Only some land on :Q.
			name:       "some matching and some not",
			rewritable: "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(:Q) }",
			equivalent: "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(b) WHERE b:Q RETURN b }",
		},
		{
			name:       "untyped hop to a labelled far node",
			rewritable: "MATCH (a:P {id: 3}) RETURN COUNT { (a)-->(:Q) }",
			equivalent: "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-->(b) WHERE b:Q RETURN b }",
		},
		{
			// Every node carries :P, so this degenerates to the typed degree —
			// a useful boundary because the label filter admits everything.
			name:       "label that every neighbour carries",
			rewritable: "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(:P) }",
			equivalent: "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(b) WHERE b:P RETURN b }",
		},
		{
			// A conjunction: the far node must carry BOTH labels.
			name:       "multi-label far node is a conjunction",
			rewritable: "MATCH (a:P {id: 2}) RETURN COUNT { (a)-->(:P:Q) }",
			equivalent: "MATCH (a:P {id: 2}) RETURN COUNT { MATCH (a)-->(b) WHERE b:P AND b:Q RETURN b }",
		},
		{
			name:       "a label no node carries counts zero",
			rewritable: "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(:NoSuchLabel) }",
			equivalent: "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:K]->(b) WHERE b:NoSuchLabel RETURN b }",
			want:       "0\x1f",
		},
		{
			name:       "a relationship type no edge carries counts zero",
			rewritable: "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:NOPE]->(:Q) }",
			equivalent: "MATCH (a:P {id: 3}) RETURN COUNT { MATCH (a)-[:NOPE]->(b) WHERE b:Q RETURN b }",
			want:       "0\x1f",
		},
		{
			// EXISTS in RETURN position, not WHERE position: a WHERE-position
			// EXISTS is planned as a SemiApply operator rather than evaluated as
			// an expression, so it never reaches this path at all.
			name:       "existence per row",
			rewritable: "MATCH (a:P) RETURN a.id, EXISTS { (a)-[:K]->(:Q) } ORDER BY a.id",
			equivalent: "MATCH (a:P) RETURN a.id, EXISTS { (a)-[:K]->(b) WHERE b:Q } ORDER BY a.id",
		},
		{
			name:       "bounded comparison over the whole graph",
			rewritable: "MATCH (a:P) WHERE COUNT { (a)-[:K]->(:Q) } > 0 RETURN count(a)",
			equivalent: "MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(b) WHERE b:Q RETURN b } > 0 RETURN count(a)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labelledHopDifferential(t, degreeFixture(t, 60), tc.rewritable, tc.equivalent, tc.want)
		})
	}
}

// TestLabelledHopCount_PatternPredicateIdentity covers the other entry point:
// `WHERE (a)-[:K]->(:P)` as a bare pattern predicate, which the audit measured at
// 26x the bare-scan baseline per outer row.
func TestLabelledHopCount_PatternPredicateIdentity(t *testing.T) {

	labelledHopDifferential(t, degreeFixture(t, 60),
		"MATCH (a:P) WHERE (a)-[:K]->(:Q) RETURN count(a)",
		"MATCH (a:P) WHERE EXISTS { (a)-[:K]->(b) WHERE b:Q } RETURN count(a)",
		"")
}

// TestLabelledHopCount_LabelAddedBySet is acceptance criterion 2's third case.
// The label the walk tests must be the one the graph currently holds, not a
// snapshot taken when the pattern was recognised — the shape caches resolved
// label IDS, and an id is stable, but MEMBERSHIP is not.
func TestLabelledHopCount_LabelAddedBySet(t *testing.T) {

	g := mutableHopFixture(t)
	eng := NewEngine(g)

	const q = "MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->(:Marked) }"

	// b1 carries :Marked from the start; b2 does not.
	if got := degreeRun(t, eng, q); got[0] != "1\x1f" {
		t.Fatalf("before SET: got %v, want 1", got)
	}

	mustRun(t, eng, "MATCH (n {id: 2}) SET n:Marked")

	if got := degreeRun(t, eng, q); got[0] != "2\x1f" {
		t.Fatalf("after SET n:Marked: got %v, want 2 — the count did not observe a "+
			"label added after the pattern was first recognised", got)
	}
}

// TestLabelledHopCount_TombstonedFarNode is acceptance criterion 2's fourth
// case. A deleted far node must not be counted, and the liveness rule must be
// the one every other degree walker uses — which is why the tombstone gate lives
// in lpg.Graph.OutDegreeMatchingBoundedByID and not here.
func TestLabelledHopCount_TombstonedFarNode(t *testing.T) {

	g := mutableHopFixture(t)
	eng := NewEngine(g)

	const q = "MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->(:P) }"

	if got := degreeRun(t, eng, q); got[0] != "2\x1f" {
		t.Fatalf("before DELETE: got %v, want 2", got)
	}

	mustRun(t, eng, "MATCH (n {id: 2}) DETACH DELETE n")

	if got := degreeRun(t, eng, q); got[0] != "1\x1f" {
		t.Fatalf("after DETACH DELETE: got %v, want 1 — a tombstoned far node was counted", got)
	}
}

// TestLabelledHopCount_ParallelEdges is acceptance criterion 2's fifth case. In
// a multigraph two edges between the same pair are two matches, so the walk must
// count SLOTS rather than distinct neighbours.
func TestLabelledHopCount_ParallelEdges(t *testing.T) {

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := NewEngine(g)
	mustRun(t, eng, "CREATE (:P {id: 0})")
	mustRun(t, eng, "CREATE (:P:Q {id: 1})")
	// Three parallel :K edges to the same :Q node, plus one :M that must not count.
	for i := 0; i < 3; i++ {
		mustRun(t, eng, "MATCH (a {id: 0}), (b {id: 1}) CREATE (a)-[:K]->(b)")
	}
	mustRun(t, eng, "MATCH (a {id: 0}), (b {id: 1}) CREATE (a)-[:M]->(b)")

	labelledHopDifferential(t, g,
		"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->(:Q) }",
		"MATCH (a:P {id: 0}) RETURN COUNT { MATCH (a)-[:K]->(b) WHERE b:Q RETURN b }",
		"3\x1f")
}

// TestLabelledHopCount_IneligibleShapes pins the recogniser's boundary. Every
// case must NOT take the labelled-hop path: each carries something an adjacency
// walk cannot evaluate, so answering it this way would be a wrong answer rather
// than a slow one.
func TestLabelledHopCount_IneligibleShapes(t *testing.T) {

	cases := []struct{ name, query string }{
		// NOTE: `COUNT { (a)-[:K]->(b:Q) WHERE b.id > 1 }` is deliberately absent.
		// It SHOULD be ineligible, but the recogniser never sees the WHERE — the
		// parser discards it before the AST reaches here (see the note on
		// TestLabelledHopCount_Identity). Asserting either outcome would pin
		// behaviour that belongs to the parser defect, not to this recogniser.
		// The EXISTS spelling, whose WHERE the parser does preserve, is covered.
		{"inline WHERE on EXISTS is a Selection", "MATCH (a:P {id: 3}) RETURN EXISTS { (a)-[:K]->(b:Q) WHERE b.id > 1 }"},
		{"far-node property is a Selection", "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(:Q {id: 4}) }"},
		{"label on the anchor", "MATCH (a:P {id: 3}) RETURN COUNT { (a:P)-[:K]->(:Q) }"},
		{"incoming direction", "MATCH (a:P {id: 3}) RETURN COUNT { (a)<-[:K]-(:Q) }"},
		{"undirected", "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]-(:Q) }"},
		{"two relationship types", "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K|M]->(:Q) }"},
		{"named relationship variable", "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[r:K]->(:Q) }"},
		{"variable-length range", "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K*1..2]->(:Q) }"},
		{"two hops", "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->(:Q)-[:K]->(:Q) }"},
		{"both endpoints bound is an expand-into", "MATCH (a:P {id: 3}), (b:Q) RETURN COUNT { (a)-[:K]->(b:Q) }"},
		{"unlabelled far node belongs to the degree rewrite", "MATCH (a:P {id: 3}) RETURN COUNT { (a)-[:K]->() }"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(degreeFixture(t, 60))
			before := labelledHopRewriteCount.Load()
			_ = degreeRun(t, eng, tc.query)
			if fired := labelledHopRewriteCount.Load() - before; fired != 0 {
				t.Errorf("the labelled-hop count fired %d time(s) for an ineligible shape:\n  %s",
					fired, tc.query)
			}
		})
	}
}

// TestLabelledHopCount_ShortCircuits is acceptance criterion 4. A comparison
// against a small literal must stop walking once the answer is settled, rather
// than counting a supernode's whole adjacency.
//
// The observable is the number of label-membership probes the walk performs,
// counted by the predicate itself — a direct measurement of how far the walk
// got, not a timing.
func TestLabelledHopCount_ShortCircuits(t *testing.T) {

	const degree = 5000
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	if err := g.AddNode("hub"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("hub", "P"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	for i := 0; i < degree; i++ {
		k := fmt.Sprintf("t%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Q"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.AddEdge("hub", k, 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel("hub", k, "K")
	}

	sh := &labelledHopShape{anchorVar: "a", typed: true, typeName: "K", farLabels: []string{"Q"}}
	hubID, ok := g.AdjList().Mapper().Lookup("hub")
	if !ok {
		t.Fatal("the hub node is not interned")
	}
	row := expr.RowContext{"a": expr.NodeValue{ID: uint64(hubID)}}

	// Capped at 3: the walk must stop at the third match, not at edge 5000.
	probes := countLabelProbes(t, g, sh, row, 3)
	if probes > 3 {
		t.Errorf("a cap of 3 probed %d neighbours on a degree-%d anchor; the walk did not short-circuit",
			probes, degree)
	}

	// Uncapped, the same walk must reach the end — otherwise the cap above proves
	// nothing, since a walk that always stops early would pass it too.
	full := countLabelProbes(t, g, sh, row, -1)
	if full != degree {
		t.Errorf("an uncapped walk probed %d neighbours, want %d — the capped case is "+
			"only evidence if the uncapped one really walks the whole adjacency", full, degree)
	}
}

// countLabelProbes runs the shape's walk with an instrumented graph index,
// returning how many far nodes the label predicate was asked about.
func countLabelProbes(t *testing.T, g *lpg.Graph[string, float64], sh *labelledHopShape, row expr.RowContext, limit int64) int {
	t.Helper()
	probes := 0
	// Reproduce count's walk with a counting predicate. Going through
	// OutDegreeMatchingBoundedByID directly is what makes this a measurement of
	// the WALK rather than of the shape's bookkeeping.
	if !sh.resolveRelType(g.ReadAt(nil)) || !sh.resolveFarLabels(g.ReadAt(nil)) {
		t.Fatal("fixture did not intern the type or the label")
	}
	id, ok := nodeIDFromValue(row["a"], g.AdjList().Mapper())
	if !ok {
		t.Fatal("anchor did not resolve to a node id")
	}
	ceiling := maxDegreeLimit
	if limit >= 0 {
		ceiling = int(limit)
	}
	idx := g.NodeIndex()
	if _, found := g.OutDegreeMatchingBoundedByID(id, sh.relType, sh.typed, ceiling, func(dst graph.NodeID) bool {
		probes++
		for _, lid := range sh.farIDs {
			if !idx.Has(lid, dst) {
				return false
			}
		}
		return true
	}); !found {
		t.Fatal("the anchor was not interned")
	}
	return probes
}

// mutableHopFixture builds a tiny graph the mutation cases can change: node 0 is
// the anchor with :K edges to nodes 1 and 2, of which only node 1 starts out
// carrying :Marked.
func mutableHopFixture(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := NewEngine(g)
	mustRun(t, eng, "CREATE (:P {id: 0})")
	mustRun(t, eng, "CREATE (:P:Marked {id: 1})")
	mustRun(t, eng, "CREATE (:P {id: 2})")
	mustRun(t, eng, "MATCH (a {id: 0}), (b {id: 1}) CREATE (a)-[:K]->(b)")
	mustRun(t, eng, "MATCH (a {id: 0}), (b {id: 2}) CREATE (a)-[:K]->(b)")
	return g
}
