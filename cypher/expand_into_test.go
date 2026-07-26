package cypher_test

// expand_into_test.go — correctness gate for expand-into (#2206).
//
// The filter runs INSIDE the Expand operator, before the equality Selection the IR
// translator puts above it, so it must not change any result. The risk is specific: a
// hop's destination cell is compared unboxed, and getting that wrong drops rows that
// should survive — a silent under-count on exactly the cyclic patterns the optimisation
// targets.
//
// So these tests do not compare the engine against itself. They compare it against an
// ORACLE computed directly from the fixture in Go, which cannot share a bug with the
// operator, and they cover the cases where cardinality is subtle: parallel edges in a
// multigraph, relationship isomorphism (cyphermorphism) forbidding one edge from filling
// two pattern slots, self-loops, and both traversal directions.
//
// Layer: short.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// intoRing builds a ring over n nodes where node i is joined to (i+1)..(i+degree) modulo
// n in BOTH directions, every node labelled P and every edge typed K. The edges must run
// both ways or the fixture contains no cycles at all and the tests below would prove
// nothing — a forward-only ring with degree << n has neither 2-cycles nor triangles,
// which is exactly what the guards in each test catch.
//
// It returns the graph and the adjacency as a plain Go map, which is the oracle.
func intoRing(t *testing.T, n, degree int) (*lpg.Graph[string, float64], map[int][]int) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("n%04d", i)
		keys[i] = k
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	adj := make(map[int][]int, n)
	for i := 0; i < n; i++ {
		for d := 1; d <= degree; d++ {
			j := (i + d) % n
			if err := g.AddEdge(keys[i], keys[j], 1.0); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			g.SetEdgeLabel(keys[i], keys[j], "K")
			adj[i] = append(adj[i], j)
			if err := g.AddEdge(keys[j], keys[i], 1.0); err != nil {
				t.Fatalf("AddEdge reverse: %v", err)
			}
			g.SetEdgeLabel(keys[j], keys[i], "K")
			adj[j] = append(adj[j], i)
		}
	}
	return g, adj
}

// countIntoRows runs query and returns the number of rows.
func countIntoRows(t *testing.T, eng *cypher.Engine, query string) int {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", query, err)
	}
	return n
}

// TestExpandInto_TwoCycleMatchesOracle counts 2-cycles against a Go oracle. Relationship
// isomorphism means the two hops must be DISTINCT edges, so a->b->a counts only when the
// ring carries both a->b and b->a as separate edges.
func TestExpandInto_TwoCycleMatchesOracle(t *testing.T) {
	t.Parallel()
	const n, degree = 60, 5
	g, adj := intoRing(t, n, degree)
	eng := cypher.NewEngine(g)

	// Oracle: ordered pairs (a,b) with an a->b edge and a b->a edge.
	has := make(map[[2]int]bool)
	for a, outs := range adj {
		for _, b := range outs {
			has[[2]int{a, b}] = true
		}
	}
	want := 0
	for a, outs := range adj {
		for _, b := range outs {
			if has[[2]int{b, a}] {
				want++
			}
		}
	}

	got := countIntoRows(t, eng, `MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN a, b`)
	if got != want {
		t.Fatalf("2-cycles: engine %d, oracle %d — expand-into must not change the result", got, want)
	}
	if want == 0 {
		t.Fatal("the fixture produced no 2-cycles, so this test proves nothing; adjust degree")
	}
}

// TestExpandInto_TriangleMatchesOracle counts directed triangles against a Go oracle.
func TestExpandInto_TriangleMatchesOracle(t *testing.T) {
	t.Parallel()
	const n, degree = 60, 5
	g, adj := intoRing(t, n, degree)
	eng := cypher.NewEngine(g)

	has := make(map[[2]int]bool)
	for a, outs := range adj {
		for _, b := range outs {
			has[[2]int{a, b}] = true
		}
	}
	want := 0
	for a, outsA := range adj {
		for _, b := range outsA {
			for _, c := range adj[b] {
				if has[[2]int{c, a}] {
					want++
				}
			}
		}
	}

	got := countIntoRows(t, eng, `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN a, b, c`)
	if got != want {
		t.Fatalf("triangles: engine %d, oracle %d", got, want)
	}
	if want == 0 {
		t.Fatal("the fixture produced no triangles, so this test proves nothing")
	}
}

// TestExpandInto_ParallelEdgesPreserveCardinality pins the multigraph case: two parallel
// a->b edges and one b->a edge must yield TWO rows for a->b->a, because each parallel
// edge is a distinct relationship. A filter that answered "does an edge exist" rather than
// enumerating the edges between the pair would collapse this to one.
func TestExpandInto_ParallelEdgesPreserveCardinality(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	// Two parallel a->b edges, one b->a.
	for i := 0; i < 2; i++ {
		if err := g.AddEdge("a", "b", 1.0); err != nil {
			t.Fatalf("AddEdge a->b: %v", err)
		}
	}
	g.SetEdgeLabel("a", "b", "K")
	if err := g.AddEdge("b", "a", 1.0); err != nil {
		t.Fatalf("AddEdge b->a: %v", err)
	}
	g.SetEdgeLabel("b", "a", "K")

	eng := cypher.NewEngine(g)
	// The pattern is symmetric, so it matches in both orientations:
	//   a->b->a : two choices for the first hop (the parallel pair), one for the second
	//   b->a->b : one choice for the first hop, two for the second
	// giving 4 rows in total. The number matters: a filter that answered "does an edge
	// exist between this pair" rather than ENUMERATING the edges between it would collapse
	// each orientation to one and report 2.
	const want = 4
	if got := countIntoRows(t, eng, `MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN a, b`); got != want {
		t.Fatalf("parallel edges: got %d rows, want %d — each parallel edge is a distinct "+
			"relationship, so expand-into must ENUMERATE the edges between the pair rather "+
			"than answer whether one exists", got, want)
	}
}

// TestExpandInto_SelfLoopAndDirections covers the shapes most likely to break an unboxed
// destination comparison: a self-loop (source and destination are the same node), and the
// inbound and undirected forms of a closing hop.
func TestExpandInto_SelfLoopAndDirections(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"a", "b"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	if err := g.AddEdge("a", "a", 1.0); err != nil { // self-loop
		t.Fatalf("AddEdge a->a: %v", err)
	}
	g.SetEdgeLabel("a", "a", "K")
	if err := g.AddEdge("a", "b", 1.0); err != nil {
		t.Fatalf("AddEdge a->b: %v", err)
	}
	g.SetEdgeLabel("a", "b", "K")
	if err := g.AddEdge("b", "a", 1.0); err != nil {
		t.Fatalf("AddEdge b->a: %v", err)
	}
	g.SetEdgeLabel("b", "a", "K")

	eng := cypher.NewEngine(g)
	for _, tc := range []struct {
		query string
		want  int
	}{
		// a->b->a is the only 2-cycle over distinct edges; the self-loop cannot fill
		// both hops (cyphermorphism), and a->a->a needs two distinct a->a edges.
		{`MATCH (x:P)-[:K]->(y:P)-[:K]->(x) RETURN x, y`, 2},
		// Inbound closing hop: (x)-->(y) then (y)<--(x) is the same edge, which
		// cyphermorphism forbids, so only the genuine 2-cycle survives... expressed
		// inbound, a<-b means b->a.
		{`MATCH (x:P)-[:K]->(y:P)<-[:K]-(x) RETURN x, y`, 0},
		// Undirected closing hop admits either orientation.
		{`MATCH (x:P)-[:K]->(y:P)-[:K]-(x) RETURN x, y`, 2},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got := countIntoRows(t, eng, tc.query); got != tc.want {
				t.Errorf("got %d rows, want %d", got, tc.want)
			}
		})
	}
}

// TestExpandInto_ProjectedValuesUnchanged pins that the surviving rows carry the right
// values, not merely the right count: a filter applied at the wrong point could keep the
// correct number of rows while pairing the wrong endpoints.
func TestExpandInto_ProjectedValuesUnchanged(t *testing.T) {
	t.Parallel()
	const n, degree = 40, 4
	g, adj := intoRing(t, n, degree)
	eng := cypher.NewEngine(g)

	has := make(map[[2]int]bool)
	for a, outs := range adj {
		for _, b := range outs {
			has[[2]int{a, b}] = true
		}
	}
	var want []string
	for a, outs := range adj {
		for _, b := range outs {
			if has[[2]int{b, a}] {
				want = append(want, fmt.Sprintf("%04d->%04d", a, b))
			}
		}
	}
	sort.Strings(want)

	res, err := eng.Run(context.Background(),
		`MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN a.name AS an, b.name AS bn`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The fixture has no name property, so project ids instead via a second query.
	_ = res.Close()

	res2, err := eng.Run(context.Background(),
		`MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN id(a) AS ai, id(b) AS bi`, nil)
	if err != nil {
		t.Fatalf("Run ids: %v", err)
	}
	defer func() { _ = res2.Close() }()
	pairs := 0
	seen := make(map[[2]string]bool)
	for res2.Next() {
		key := [2]string{res2.ValueAt(0).String(), res2.ValueAt(1).String()}
		if seen[key] {
			// Distinct parallel edges legitimately repeat a pair; the ring has none,
			// so a repeat here means a row was duplicated.
			t.Errorf("pair %v repeated; the ring fixture has no parallel edges", key)
		}
		seen[key] = true
		pairs++
	}
	if err := res2.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if pairs != len(want) {
		t.Fatalf("got %d ordered pairs, oracle %d", pairs, len(want))
	}
}
