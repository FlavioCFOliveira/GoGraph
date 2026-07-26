package cypher

// columnar_shape_coverage_test.go — the standing coverage probe for the columnar
// tier (#2186).
//
// The round-3 audit measured which ordinary query shapes actually reach the columnar
// (unboxed) execution path and found **5 of 15** — every conjunction, negation,
// membership test, computed projection, LIMIT, ORDER BY and DISTINCT fell back to the
// fully boxed row path, and so did every traversal whose far endpoint carried a label
// (see docs/audit-2026-07-26-streams/s05-runtime.md F1 and F1b). Ten of the fifteen
// never touched the columnar path at all.
//
// That count was a one-off measurement taken by an audit stream. This file makes it a
// permanent, executable fact: it probes the same shapes through the metrics counter
// the columnar filter increments per batch, reports the engaged/total tally, and fails
// if coverage REGRESSES below the recorded floor. A shape moving from ROW to COL is a
// win and only needs the floor raised; a shape moving from COL to ROW fails the build.
//
// The tally is logged on every run, so `go test -run TestColumnarShapeCoverage -v
// ./cypher/` is the answer to "how many shapes are vectorized today".
//
// Correctness is NOT this file's job — every newly admitted shape is proved
// result-identical to the row path in columnar_shape_identity_test.go. This file only
// answers "did the fast path engage".
//
// Layer: short.

import (
	"context"
	"sort"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// coverageGraph builds a small labelled graph with two int properties and a string
// property, plus one relationship type, so every probed shape has something to bind:
// nodes n0..n(coverageNodes-1) all carry label P, property v (the node index),
// property w (index % 7) and property name ("p<i>"), and each node has an out-edge
// of type K to the next.
func coverageGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	const coverageNodes = 2000
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	keys := make([]string, coverageNodes)
	for i := 0; i < coverageNodes; i++ {
		k := coverageKey(i)
		keys[i] = k
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty v: %v", err)
		}
		if err := g.SetNodeProperty(k, "w", lpg.Int64Value(int64(i%7))); err != nil {
			t.Fatalf("SetNodeProperty w: %v", err)
		}
		if err := g.SetNodeProperty(k, "name", lpg.StringValue("p"+coverageKey(i)[1:])); err != nil {
			t.Fatalf("SetNodeProperty name: %v", err)
		}
	}
	for i := 0; i+1 < coverageNodes; i++ {
		if err := g.AddEdge(keys[i], keys[i+1], 1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel(keys[i], keys[i+1], "K")
	}
	return g
}

// coverageKey renders a stable, collision-free node key. (padKey in
// columnar_filter_test.go pads to three digits and would collide above 1000 nodes.)
func coverageKey(i int) string {
	s := "0000" + strconv.Itoa(i)
	return "n" + s[len(s)-5:]
}

// coverageShape is one probed query shape.
type coverageShape struct {
	name  string
	query string
}

// scanShapes are the fifteen single-scan filter-and-project shapes the audit probed,
// in its order. Every one returns a non-empty result over coverageGraph.
var scanShapes = []coverageShape{
	{"bare comparison", "MATCH (n:P) WHERE n.v > 10 RETURN n.v"},
	{"two projected properties", "MATCH (n:P) WHERE n.v > 10 RETURN n.v, n.w"},
	{"parameterised comparison", "MATCH (n:P) WHERE n.v > $p RETURN n.v"},
	{"aliased projection", "MATCH (n:P) WHERE n.v > 10 RETURN n.v AS a"},
	{"WITH passthrough", "MATCH (n:P) WHERE n.v > 10 WITH n.v AS x RETURN x"},
	{"conjunction, same property", "MATCH (n:P) WHERE n.v > 10 AND n.v < 100 RETURN n.v"},
	{"conjunction, two properties", "MATCH (n:P) WHERE n.v > 10 AND n.w = 3 RETURN n.v"},
	{"disjunction", "MATCH (n:P) WHERE n.v > 10 OR n.w = 3 RETURN n.v"},
	{"negation", "MATCH (n:P) WHERE NOT n.v > 10 RETURN n.v"},
	{"IN over a literal list", "MATCH (n:P) WHERE n.v IN [1,2,3] RETURN n.v"},
	{"STARTS WITH", "MATCH (n:P) WHERE n.name STARTS WITH 'p0000' RETURN n.v"},
	{"computed projection", "MATCH (n:P) WHERE n.v > 10 RETURN n.v + 1"},
	{"LIMIT", "MATCH (n:P) WHERE n.v > 10 RETURN n.v LIMIT 5"},
	{"ORDER BY", "MATCH (n:P) WHERE n.v > 10 RETURN n.v ORDER BY n.v"},
	{"DISTINCT", "MATCH (n:P) WHERE n.v > 10 RETURN DISTINCT n.v"},
}

// traversalShapes are the single-hop shapes from the audit's traversal table, whose
// fourth entry — the labelled far endpoint — was the measured 4.4x / 1640x cliff,
// plus the anchor-filter shapes F1b identified as a permanent blind spot: a WHERE on
// the STARTING node, which is the commonest shape in graph querying.
var traversalShapes = []coverageShape{
	{"post-traversal filter", "MATCH (a:P)-[:K]->(m) WHERE m.v > 10 RETURN m.v"},
	{"post-traversal filter, untyped edge", "MATCH (a:P)-->(m) WHERE m.v > 10 RETURN m.v"},
	{"post-traversal filter, unlabelled anchor", "MATCH (a)-[:K]->(m) WHERE m.v > 10 RETURN m.v"},
	{"post-traversal filter, labelled far endpoint", "MATCH (a:P)-[:K]->(m:P) WHERE m.v > 10 RETURN m.v"},
	{"anchor filter", "MATCH (a:P)-[:K]->(m) WHERE a.v > 10 RETURN m.v"},
	{"anchor filter, labelled far endpoint", "MATCH (a:P)-[:K]->(m:P) WHERE a.v > 10 RETURN m.v"},
	{"anchor and post-traversal filter", "MATCH (a:P)-[:K]->(m) WHERE a.v > 10 AND m.v < 1000 RETURN m.v"},
}

// The coverage floors are the number of probed shapes that MUST reach the columnar
// path. They are a ratchet: raise one whenever a shape is newly admitted, never lower
// it.
//
// History:
//   - 5/15 scan + 4/7 traversal — round-3 audit baseline (v0.10.0). The audit reported
//     3/7 for the traversal group and named the anchor filter a "permanent blind
//     spot"; re-measuring here refuted that. The translator puts an anchor predicate
//     in the SAME Selection slot as a post-traversal one, so the existing recogniser
//     already vectorized it. The measured cliff was entirely the STACKED Selection a
//     pattern label produces, plus the conjunction combiner.
//   - 9/15 scan + 7/7 traversal — #2186: conjunction, label test, IN over a scalar
//     literal list, stacked-Selection fusion over a traversal, chunk-transparent LIMIT.
//
// The six scan shapes still on the row path are, with the reason each declines:
// disjunction and negation (no OR/NOT combiner — a 3VL OR cannot decide a drop from
// one undecided operand, so it needs a different rule than AND); STARTS WITH (no
// prefix ChunkPredicate; backlog); computed projection (the projection, not the
// predicate — every item must be a bare property access); ORDER BY and DISTINCT
// (pipeline breakers that are not ChunkProducers, the same structural reason LIMIT had
// before this task).
const (
	scanCoverageFloor      = 9
	traversalCoverageFloor = 7
)

// probeEngaged reports whether query reaches the columnar filter, measured by the
// per-batch counter the columnar filter increments.
func probeEngaged(t *testing.T, g *lpg.Graph[string, float64], be *countingBackend, query string) bool {
	t.Helper()
	be.filterBatches.Store(0)
	eng := NewEngine(g)
	res, err := eng.Run(context.Background(), query, map[string]expr.Value{"p": expr.IntegerValue(10)})
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", query, err)
	}
	if rows == 0 {
		t.Fatalf("probe %q returned no rows: the probe cannot distinguish paths on an "+
			"empty result, so the query or the fixture is wrong", query)
	}
	return be.filterBatches.Load() > 0
}

// TestColumnarShapeCoverage reports and ratchets columnar engagement across the
// audit's probed shapes.
func TestColumnarShapeCoverage(t *testing.T) {
	g := coverageGraph(t)
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)

	for _, group := range []struct {
		label  string
		shapes []coverageShape
		floor  int
	}{
		{"single scan", scanShapes, scanCoverageFloor},
		{"single hop", traversalShapes, traversalCoverageFloor},
	} {
		var engaged, notEngaged []string
		for _, s := range group.shapes {
			if probeEngaged(t, g, be, s.query) {
				engaged = append(engaged, s.name)
			} else {
				notEngaged = append(notEngaged, s.name)
			}
		}
		sort.Strings(engaged)
		sort.Strings(notEngaged)
		t.Logf("%s: %d/%d shapes on the columnar path", group.label, len(engaged), len(group.shapes))
		for _, n := range engaged {
			t.Logf("  COL  %s", n)
		}
		for _, n := range notEngaged {
			t.Logf("  row  %s", n)
		}
		if len(engaged) < group.floor {
			t.Fatalf("%s columnar coverage REGRESSED to %d/%d, below the recorded floor of %d; "+
				"shapes now on the row path: %v", group.label, len(engaged), len(group.shapes),
				group.floor, notEngaged)
		}
	}
}
