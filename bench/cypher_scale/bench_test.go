// Package cypher_scale_test benchmarks the Cypher engine's scan, filter,
// project, aggregate, and 1-hop expand paths at a scale (>=100k nodes) large
// enough to expose the per-scanned-entity allocation cost that the
// 2026-07-02 production-readiness audit found (finding P1: expr.Value
// interface boxing on the scan/project path; finding P3: no benchmark
// exercised the Cypher engine above ~1k nodes, so that cost had no
// regression gate at a realistic scale).
//
// The seed graph (120 000 :Person nodes with firstName/age properties, each
// with 8 outgoing :KNOWS edges) is built once in TestMain.
//
// Run with:
//
//	go test -bench=. -benchmem -count=5 ./bench/cypher_scale/...
package cypher_scale_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedSize is the node count of the shared benchmark graph — comfortably
// above the 100k threshold the audit's P1/P2 findings were measured at.
const seedSize = 120_000

// knowsOutDegree is the number of outgoing :KNOWS edges per node, giving a
// realistic 1-hop expand fan-out (~960k directed edges total).
const knowsOutDegree = 8

// benchGraph is the shared read-only graph seeded in TestMain.
var benchGraph *lpg.Graph[string, float64]

// TestMain seeds the benchmark graph once and runs all tests/benchmarks.
func TestMain(m *testing.M) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < seedSize; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			log.Fatalf("seed AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			log.Fatalf("seed SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "firstName", lpg.StringValue(fmt.Sprintf("Person%d", i))); err != nil {
			log.Fatalf("seed SetNodeProperty(firstName): %v", err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(18+i%65))); err != nil {
			log.Fatalf("seed SetNodeProperty(age): %v", err)
		}
	}
	// A large odd stride spreads each node's neighbours across the id space
	// (rather than a dense contiguous run) without needing a real RNG —
	// deterministic seeding keeps benchmark runs byte-reproducible.
	const stride = 104729 // prime, coprime with seedSize's factors
	for i := 0; i < seedSize; i++ {
		src := fmt.Sprintf("n%d", i)
		for k := 1; k <= knowsOutDegree; k++ {
			dst := fmt.Sprintf("n%d", (i+k*stride)%seedSize)
			if err := g.AddEdge(src, dst, 0); err != nil {
				log.Fatalf("seed AddEdge: %v", err)
			}
			g.SetEdgeLabel(src, dst, "KNOWS")
		}
	}
	benchGraph = g
	os.Exit(m.Run())
}

// runQuery executes query against the shared benchmark graph for b.N
// iterations, draining every row (so projection/materialisation cost is
// measured, not skipped).
func runQuery(b *testing.B, query string) {
	b.Helper()
	engine := cypher.NewEngine(benchGraph)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := engine.Run(ctx, query, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		for res.Next() {
		}
		if e := res.Err(); e != nil {
			b.Fatalf("result.Err: %v", e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("result.Close: %v", err)
		}
	}
}

// BenchmarkCountAllPersons is a pure scan+aggregate: the scan leaf boxes one
// expr.Value per scanned node (finding P1) even though the result is a
// single integer.
func BenchmarkCountAllPersons(b *testing.B) {
	runQuery(b, `MATCH (p:Person) RETURN count(p) AS c`)
}

// BenchmarkFilterProject is a scan+filter+project: one boxed scalar per
// scanned node for the WHERE predicate, plus two boxed scalars per matching
// row for the projected columns.
func BenchmarkFilterProject(b *testing.B) {
	runQuery(b, `MATCH (p:Person) WHERE p.age > 47 RETURN p.firstName, p.age`)
}

// BenchmarkExpand1Hop is a 1-hop relationship-type-filtered expand: the
// heaviest read shape (finding P2 — re-materialised edge-label slices per
// candidate edge for the :KNOWS type filter, on top of per-row boxing).
func BenchmarkExpand1Hop(b *testing.B) {
	runQuery(b, `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.firstName, b.age`)
}

// TestCypherScale_QueriesRun is a short-layer smoke test verifying the three
// benchmark query shapes execute correctly against the seed graph — always
// run (no -bench flag required), so a broken query shape fails CI rather
// than only being caught by a benchmark nobody runs by default.
func TestCypherScale_QueriesRun(t *testing.T) {
	engine := cypher.NewEngine(benchGraph)
	ctx := context.Background()

	t.Run("count", func(t *testing.T) {
		res, err := engine.Run(ctx, `MATCH (p:Person) RETURN count(p) AS c`, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer func() { _ = res.Close() }()
		if !res.Next() {
			t.Fatal("expected one row")
		}
		got, ok := res.Record()["c"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("c: want IntegerValue, got %T", res.Record()["c"])
		}
		if int64(got) != seedSize {
			t.Errorf("count(p) = %d, want %d", int64(got), seedSize)
		}
	})

	t.Run("filter_project", func(t *testing.T) {
		res, err := engine.Run(ctx, `MATCH (p:Person) WHERE p.age > 47 RETURN p.firstName, p.age`, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer func() { _ = res.Close() }()
		rows := 0
		for res.Next() {
			rec := res.Record()
			if _, ok := rec["p.firstName"].(expr.StringValue); !ok {
				t.Fatalf("p.firstName: want StringValue, got %T", rec["p.firstName"])
			}
			age, ok := rec["p.age"].(expr.IntegerValue)
			if !ok {
				t.Fatalf("p.age: want IntegerValue, got %T", rec["p.age"])
			}
			if int64(age) <= 47 {
				t.Errorf("row with age %d should have been filtered out", int64(age))
			}
			rows++
		}
		if err := res.Err(); err != nil {
			t.Fatalf("result.Err: %v", err)
		}
		if rows == 0 {
			t.Error("filter_project matched zero rows, want > 0")
		}
	})

	t.Run("expand_1hop", func(t *testing.T) {
		res, err := engine.Run(ctx, `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.firstName, b.age`, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer func() { _ = res.Close() }()
		rows := 0
		for res.Next() {
			rows++
		}
		if err := res.Err(); err != nil {
			t.Fatalf("result.Err: %v", err)
		}
		want := seedSize * knowsOutDegree
		if rows != want {
			t.Errorf("expand_1hop rows = %d, want %d", rows, want)
		}
	})
}
