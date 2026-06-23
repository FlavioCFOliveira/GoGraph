package cypher_test

// lazynode_reuse_test.go — #1697 (black box).
//
// Pins the soundness invariants of the pooled per-row *expr.LazyNodeValue reuse
// on the non-escaping WHERE/scalar-projection path:
//
//   - Aliasing: when one row binds two node variables that are BOTH read only
//     through scalar accessors (e.g. WHERE a.v <> b.v RETURN a.v, b.v), each
//     variable must keep its own reusable lazy struct so the two property reads
//     never observe one mutated id. A single shared struct would make a.v read
//     b's property.
//   - Resolver freshness: two back-to-back queries against two DIFFERENT graphs
//     reuse the same pooled RowContext (and its lazy arena) from the sync.Pool.
//     The second query must read its OWN graph, never the first graph's data —
//     proving Reset rebinds the resolver, not just the id.
//   - High row count under the pooled path, to exercise arena reuse across many
//     rows within one query.
//
// Layer: short.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedReuse builds a graph of n nodes each carrying property v = base+index.
func seedReuse(t *testing.T, n int, base int64) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(id, "v", lpg.Int64Value(base+int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

// TestLazyNodeReuse_MultiVarPredicateNoAliasing exercises a single-row predicate
// that reads two distinct bound node variables through scalar accessors only. If
// the reuse aliased one struct across both variables, a.v would misread b's
// value and the filter / projection would be wrong.
func TestLazyNodeReuse_MultiVarPredicateNoAliasing(t *testing.T) {
	t.Parallel()
	// 3 nodes with v in {0,1,2}; the only edge is n0->n1 (v 0 -> 1).
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(id, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if err := g.AddEdge("n0", "n1", 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	eng := cypher.NewEngine(g)

	// a.v and b.v are both scalar-only reads; the predicate needs BOTH lazy
	// nodes live in the same row evaluation. Expect exactly one row: a.v=0, b.v=1.
	res, err := eng.Run(context.Background(),
		"MATCH (a)-->(b) WHERE a.v <> b.v RETURN a.v AS av, b.v AS bv", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var rows int
	for res.Next() {
		rec := res.Record()
		av, aok := rec["av"].(expr.IntegerValue)
		bv, bok := rec["bv"].(expr.IntegerValue)
		if !aok || !bok {
			t.Fatalf("row %d: want IntegerValue av/bv, got av=%T bv=%T", rows, rec["av"], rec["bv"])
		}
		if int64(av) != 0 || int64(bv) != 1 {
			t.Errorf("row %d: aliasing detected — want av=0 bv=1, got av=%d bv=%d", rows, av, bv)
		}
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result err: %v", err)
	}
	if rows != 1 {
		t.Fatalf("want exactly 1 row, got %d", rows)
	}
}

// TestLazyNodeReuse_ResolverFreshnessAcrossGraphs runs two queries against two
// different graphs back-to-back. The second draws the first's recycled pooled
// RowContext (and lazy arena) from the sync.Pool, so it would read the first
// graph's resolver if Reset rebound only the id. The values must come from the
// second graph.
func TestLazyNodeReuse_ResolverFreshnessAcrossGraphs(t *testing.T) {
	// Not parallel: relies on this goroutine's P reusing the pooled object the
	// first query released, which is most reliable without interleaving.
	g1 := seedReuse(t, 64, 0)    // v in [0, 63]
	g2 := seedReuse(t, 64, 1000) // v in [1000, 1063]

	run := func(g *lpg.Graph[string, float64], wantMin, wantMax int64) {
		eng := cypher.NewEngine(g)
		res, err := eng.Run(context.Background(),
			"MATCH (n) WHERE n.v >= 0 RETURN n.v AS v", nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer res.Close()
		var rows int
		for res.Next() {
			v, ok := res.Record()["v"].(expr.IntegerValue)
			if !ok {
				t.Fatalf("want IntegerValue, got %T", res.Record()["v"])
			}
			if int64(v) < wantMin || int64(v) > wantMax {
				t.Fatalf("stale resolver — value %d outside graph range [%d,%d]", v, wantMin, wantMax)
			}
			rows++
		}
		if err := res.Err(); err != nil {
			t.Fatalf("result err: %v", err)
		}
		if rows != 64 {
			t.Fatalf("want 64 rows, got %d", rows)
		}
	}

	run(g1, 0, 63)
	run(g2, 1000, 1063) // must read g2's data, not g1's recycled resolver
}

// TestLazyNodeReuse_HighRowCount drives many rows through the pooled arena in one
// query to exercise arena growth and per-row Reset; the count must be exact.
func TestLazyNodeReuse_HighRowCount(t *testing.T) {
	t.Parallel()
	const n = 5000
	g := seedReuse(t, n, 0)
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(),
		"MATCH (n) WHERE n.v >= 0 RETURN count(n) AS c", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()
	if !res.Next() {
		t.Fatalf("want one row, got none: %v", res.Err())
	}
	c, ok := res.Record()["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("want IntegerValue, got %T", res.Record()["c"])
	}
	if int64(c) != n {
		t.Errorf("want count %d, got %d", n, c)
	}
}
