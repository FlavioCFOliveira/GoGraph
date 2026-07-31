package cypher_test

// patterncomp_projection_test.go — the two defects a projected pattern
// comprehension carried (rmp #2264 and the inline-WHERE drop it exposed).
//
// Layer: short.
//
// # Why these could not be seen before
//
// A pattern comprehension reaches the runtime by two completely different
// routes depending on where it is written. In a WHERE clause it survives to
// evaluation as an *ast.PatternComprehension and is served by the expression
// evaluator. In a RETURN or WITH item the IR translator hoists it into a
// RollUpApply and substitutes a variable. Everything below is a divergence
// between those two routes, so any test that exercises only one of them is
// structurally unable to see it — and the delivered tests exercised the WHERE
// route.
//
//  1. CORRECTNESS. The hoist built its filter with ir.NewSelection, which takes
//     only the predicate's STRING form and leaves PredicateExpr nil. The
//     executor's fallback for a nil PredicateExpr is a pass-through stub, so the
//     predicate was never evaluated: `[ (a)-[:K]->(x) WHERE x:Far | 1 ]` returned
//     one element per out-edge instead of one per MATCHING out-edge. No error, no
//     warning, just a wrong list — and the identical comprehension in a WHERE
//     clause was right the whole time.
//
//  2. PERFORMANCE. Because the hoist consumed every projected comprehension, the
//     degree rewrite (cypher.patternEvaluator.CountPatternComp) was unreachable
//     from a projection, and `size([ … ])` built the whole list to measure it.
//     At an out-degree of 100 000 that cost 52.7 ms against 2.2 ms for the
//     COUNT { } spelling of the same question — 24× for choosing a different
//     spelling of one query.
//
// The tests are paired deliberately: TestPatternComp_InlineWhere_IsApplied fails
// on the pre-fix code with a wrong ANSWER, and
// TestPatternComp_Projection_UsesDegreeRewrite fails on it with a wrong COST.

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// pcHubGraph builds a hub with four typed out-edges whose far nodes carry a
// distinguishing label and an ordered weight, so a predicate over either a
// label or a property can be checked.
func pcHubGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s): %v", n, err)
		}
	}
	if err := g.SetNodeLabel("a", "Hub"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("c", "Far"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	for n, w := range map[string]int64{"b": 10, "c": 20, "d": 30} {
		if err := g.SetNodeProperty(n, "w", lpg.Int64Value(w)); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", n, err)
		}
	}
	for _, far := range []string{"b", "c", "d"} {
		if err := g.AddEdgeLabeled("a", far, 1, "K"); err != nil {
			t.Fatalf("AddEdgeLabeled(a,%s): %v", far, err)
		}
	}
	return g
}

// pcRunScalar executes q and returns the single value of column col from the last
// row. Every query below produces exactly one row.
func pcRunScalar(t *testing.T, eng *cypher.Engine, q, col string) any {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%s): %v", q, err)
	}
	var v any
	for res.Next() {
		v = res.Record()[col]
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%s): %v", q, err)
	}
	_ = res.Close()
	return v
}

// pcSortedList renders a list value with its elements sorted, because openCypher
// does not specify the order in which a pattern comprehension enumerates its
// matches. Asserting the literal order would be asserting an implementation
// detail — a first draft of this test did exactly that and reported the engine
// wrong when it was right.
func pcSortedList(v any) string {
	s := strings.TrimSpace(fmt.Sprintf("%v", v))
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return s
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return "[]"
	}
	parts := strings.Split(inner, ", ")
	sort.Strings(parts)
	return "[" + strings.Join(parts, ", ") + "]"
}

// pcAllocs returns the number of heap objects fn allocates. Unlike wall
// clock this is a property of the code path taken, so machine load cannot move
// it — which is what makes it usable as a gate rather than an observation.
func pcAllocs(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.Mallocs - before.Mallocs
}

// TestPatternComp_InlineWhere_IsApplied pins the correctness defect: a WHERE
// written inside a projected pattern comprehension must filter its matches.
//
// On the pre-fix code every one of the filtered cases returns the UNFILTERED
// result — three elements where one is correct — because the hoisted Selection
// carried no parsed predicate and the executor passed every row through.
func TestPatternComp_InlineWhere_IsApplied(t *testing.T) {
	eng := cypher.NewEngine(pcHubGraph(t))

	t.Run("label predicate in a list projection", func(t *testing.T) {
		got := pcSortedList(pcRunScalar(t, eng, `MATCH (a:Hub) RETURN [ (a)-[:K]->(x) WHERE x:Far | x.w ] AS l`, "l"))
		if got != "[20]" {
			t.Fatalf("inline WHERE was not applied: got %s, want [20] (only node c carries :Far)", got)
		}
	})

	t.Run("label predicate under size()", func(t *testing.T) {
		got := pcRunScalar(t, eng, `MATCH (a:Hub) RETURN size([ (a)-[:K]->(x) WHERE x:Far | 1 ]) AS n`, "n")
		if fmt.Sprint(got) != "1" {
			t.Fatalf("inline WHERE was not applied under size(): got %v, want 1", got)
		}
	})

	t.Run("property predicate in a list projection", func(t *testing.T) {
		got := pcSortedList(pcRunScalar(t, eng, `MATCH (a:Hub) RETURN [ (a)-[:K]->(x) WHERE x.w > 15 | x.w ] AS l`, "l"))
		if got != "[20, 30]" {
			t.Fatalf("inline WHERE was not applied: got %s, want [20, 30]", got)
		}
	})

	t.Run("property predicate across a WITH barrier", func(t *testing.T) {
		got := pcSortedList(pcRunScalar(t, eng,
			`MATCH (a:Hub) WITH a, [ (a)-[:K]->(x) WHERE x.w >= 20 | x.w ] AS l RETURN l`, "l"))
		if got != "[20, 30]" {
			t.Fatalf("inline WHERE was not applied after WITH: got %s, want [20, 30]", got)
		}
	})

	// The control. This spelling was correct before the fix and must stay
	// correct: it proves the two routes now agree rather than that both broke.
	t.Run("the WHERE-clause route still agrees", func(t *testing.T) {
		got := pcRunScalar(t, eng,
			`MATCH (a:Hub) WHERE size([ (a)-[:K]->(x) WHERE x:Far | 1 ]) = 1 RETURN count(*) AS n`, "n")
		if fmt.Sprint(got) != "1" {
			t.Fatalf("the WHERE-clause route disagrees: got %v, want 1", got)
		}
	})

	// An unfiltered comprehension must be untouched by the change.
	t.Run("no predicate is unaffected", func(t *testing.T) {
		got := pcSortedList(pcRunScalar(t, eng, `MATCH (a:Hub) RETURN [ (a)-[:K]->(x) | x.w ] AS l`, "l"))
		if got != "[10, 20, 30]" {
			t.Fatalf("an unfiltered comprehension changed: got %s, want [10, 20, 30]", got)
		}
	})
}

// TestPatternComp_Projection_MatchesSubqueryOracle is the differential the task
// asked for: for every shape in the matrix, the projected `size([ … ])`
// spelling and the `COUNT { … }` spelling of the same question must agree.
//
// The matrix deliberately includes shapes the degree rewrite must REFUSE — a
// labelled far node, an incoming or undirected relationship, an inline WHERE —
// because the danger in making the rewrite reachable is that it claims a shape
// it cannot answer and returns a plausible wrong number.
func TestPatternComp_Projection_MatchesSubqueryOracle(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b", "c", "d", "e", "z"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := g.SetNodeLabel("a", "Hub"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("c", "Far"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	// Parallel edges, a self-loop, an untyped edge, an incoming edge and a
	// tombstoned far node — every shape that has previously produced a wrong
	// degree count in this engine.
	for _, e := range [][2]string{{"a", "b"}, {"a", "b"}, {"a", "a"}, {"a", "c"}, {"e", "a"}, {"a", "z"}} {
		if err := g.AddEdgeLabeled(e[0], e[1], 1, "K"); err != nil {
			t.Fatalf("AddEdgeLabeled: %v", err)
		}
	}
	if err := g.AddEdge("a", "d", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.RemoveNode("z")

	cases := []struct{ name, projection, oracle string }{
		{"typed, rewrite eligible", `size([ (a)-[:K]->(x) | 1 ])`, `COUNT { (a)-[:K]->() }`},
		{"untyped, rewrite eligible", `size([ (a)-->(x) | 1 ])`, `COUNT { (a)-->() }`},
		{"projection is irrelevant to the count", `size([ (a)-[:K]->(x) | x.name ])`, `COUNT { (a)-[:K]->() }`},
		{"far-node label — must refuse", `size([ (a)-[:K]->(x:Far) | 1 ])`, `COUNT { (a)-[:K]->(:Far) }`},
		{"incoming — must refuse", `size([ (a)<-[:K]-(x) | 1 ])`, `COUNT { (a)<-[:K]-() }`},
		{"undirected — must refuse", `size([ (a)-[:K]-(x) | 1 ])`, `COUNT { (a)-[:K]-() }`},
		{"inline WHERE — must refuse", `size([ (a)-[:K]->(x) WHERE x:Far | 1 ])`, `COUNT { (a)-[:K]->(x) WHERE x:Far }`},
	}
	eng := cypher.NewEngine(g)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotProj := pcRunScalar(t, eng, "MATCH (a:Hub) RETURN "+c.projection+" AS n", "n")
			gotOracle := pcRunScalar(t, eng, "MATCH (a:Hub) RETURN "+c.oracle+" AS n", "n")
			if fmt.Sprint(gotProj) != fmt.Sprint(gotOracle) {
				t.Fatalf("the two spellings disagree: %s = %v but %s = %v",
					c.projection, gotProj, c.oracle, gotOracle)
			}
		})
	}
}

// TestPatternComp_Projection_UsesDegreeRewrite pins the performance defect.
//
// The claim is asserted in ALLOCATIONS, not wall clock: building a list of
// 200 000 elements to measure its length is an allocation of that order, while
// reading the degree is a constant. That difference is a property of the plan
// that ran and cannot be moved by a busy machine — the same reasoning as
// rmp #2268 in bench/cyclicjoin. Before the fix this test measures the
// list-building path and fails by two orders of magnitude.
func TestPatternComp_Projection_UsesDegreeRewrite(t *testing.T) {
	const deg = 200000
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("a", "Hub"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	for i := 0; i < deg; i++ {
		k := "b" + fmt.Sprint(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.AddEdgeLabeled("a", k, 1, "K"); err != nil {
			t.Fatalf("AddEdgeLabeled: %v", err)
		}
	}
	eng := cypher.NewEngine(g)

	// Correctness first: a fast wrong answer is not a fix.
	if got := pcRunScalar(t, eng, `MATCH (a:Hub) RETURN size([ (a)-[:K]->(x) | 1 ]) AS n`, "n"); fmt.Sprint(got) != fmt.Sprint(deg) {
		t.Fatalf("size([...]) = %v, want %d", got, deg)
	}

	allocs := pcAllocs(func() {
		_ = pcRunScalar(t, eng, `MATCH (a:Hub) RETURN size([ (a)-[:K]->(x) | 1 ]) AS n`, "n")
	})
	// A degree read allocates a small constant. The list-building path allocates
	// on the order of the degree. The threshold sits two orders of magnitude
	// below the degree, far outside anything the runtime's own bookkeeping could
	// account for, so it separates the two paths without pinning a constant.
	const ceiling = deg / 100
	t.Logf("size([ (a)-[:K]->(x) | 1 ]) at out-degree %d allocated %d objects (ceiling %d)", deg, allocs, ceiling)
	if allocs > ceiling {
		t.Fatalf("the projected comprehension allocated %d objects at out-degree %d: it is building "+
			"the list to measure it rather than reading the degree (ceiling %d)", allocs, deg, ceiling)
	}
}
