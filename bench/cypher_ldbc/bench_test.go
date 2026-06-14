// Package cypher_ldbc_test contains benchmarks for the GoGraph Cypher engine
// modelled after the LDBC SNB Interactive Complex (IC) query shapes.
//
// The seed graph (1000 nodes distributed across Person/City/Company labels)
// is built once in TestMain. Each benchmark measures engine throughput for
// a specific query shape; write benchmarks (ic5, ic6, ic8) each use a fresh
// graph so that accumulation of created nodes does not distort later
// iterations.
//
// Run with:
//
//	go test -bench=. -benchmem -count=3 ./bench/cypher_ldbc/...
package cypher_ldbc_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedSize is the number of nodes in the shared benchmark graph.
const seedSize = 1000

// benchGraph is the shared read-only graph seeded in TestMain.
var benchGraph *lpg.Graph[string, float64]

// TestMain seeds the benchmark graph and runs all tests/benchmarks.
func TestMain(m *testing.M) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < seedSize; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			log.Fatalf("seed AddNode: %v", err)
		}
		var lbl string
		switch i % 3 {
		case 0:
			lbl = "Person"
		case 1:
			lbl = "City"
		case 2:
			lbl = "Company"
		}
		if err := g.SetNodeLabel(key, lbl); err != nil {
			log.Fatalf("seed SetNodeLabel: %v", err)
		}
	}
	// Pre-install an index.Manager so that concurrent NewEngine calls in
	// parallel sub-tests do not race on the first-time SetIndexManager write.
	g.SetIndexManager(index.NewManager())
	benchGraph = g
	os.Exit(m.Run())
}

// isWriteQuery reports whether q contains a CREATE or MERGE keyword at the
// top level, using a case-insensitive prefix scan. This simple heuristic is
// sufficient for the well-formed IC query set.
func isWriteQuery(q string) bool {
	upper := strings.ToUpper(strings.TrimSpace(q))
	return strings.HasPrefix(upper, "CREATE") ||
		strings.HasPrefix(upper, "MERGE")
}

// benchmarkQuery runs the Cypher query loaded from queryFile for b.N
// iterations. Write queries receive a fresh graph per benchmark group to
// prevent node accumulation from skewing measurements.
func benchmarkQuery(b *testing.B, queryFile string) {
	b.Helper()

	qBytes, err := os.ReadFile(filepath.Join("queries", queryFile)) //nolint:gosec // path is caller-controlled fixture name, not user input
	if err != nil {
		b.Fatalf("read %s: %v", queryFile, err)
	}
	query := strings.TrimSpace(string(qBytes))

	write := isWriteQuery(query)

	// Write benchmarks use a dedicated fresh graph so that every b.N loop
	// starts from the same cardinality base.
	var g *lpg.Graph[string, float64]
	if write {
		g = lpg.New[string, float64](adjlist.Config{Directed: true})
	} else {
		g = benchGraph
	}
	engine := cypher.NewEngine(g)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var (
			res *cypher.Result
			err error
		)
		if write {
			res, err = engine.RunInTx(ctx, query, nil)
		} else {
			res, err = engine.Run(ctx, query, nil)
		}
		if err != nil {
			b.Fatalf("Run(%s): %v", queryFile, err)
		}
		for res.Next() {
		}
		if e := res.Err(); e != nil {
			b.Fatalf("result.Err(%s): %v", queryFile, e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("result.Close(%s): %v", queryFile, err)
		}
	}
}

// TestCypherLDBC_AllQueriesRun is a smoke test that verifies all 14 IC query
// files parse and execute without error. This test is always included in
// the CI run (no -bench flag required).
func TestCypherLDBC_AllQueriesRun(t *testing.T) {
	queries := []struct {
		file  string
		write bool
	}{
		{"ic1.cypher", false},
		{"ic2.cypher", false},
		{"ic3.cypher", false},
		{"ic4.cypher", false},
		{"ic5.cypher", true},
		{"ic6.cypher", true},
		{"ic7.cypher", false},
		{"ic8.cypher", true},
		{"ic9.cypher", false},
		{"ic10.cypher", false},
		{"ic11.cypher", false},
		{"ic12.cypher", true},
		{"ic13.cypher", true},
		{"ic14.cypher", false},
	}

	ctx := context.Background()

	for _, q := range queries {
		q := q
		t.Run(q.file, func(t *testing.T) {
			t.Parallel()

			qBytes, err := os.ReadFile(filepath.Join("queries", q.file)) //nolint:gosec // path is a test fixture name, not user input
			if err != nil {
				t.Fatalf("read %s: %v", q.file, err)
			}
			query := strings.TrimSpace(string(qBytes))

			// Each write sub-test gets its own fresh graph to avoid shared
			// mutation across parallel tests.
			var g *lpg.Graph[string, float64]
			if q.write {
				g = lpg.New[string, float64](adjlist.Config{Directed: true})
			} else {
				g = benchGraph
			}
			engine := cypher.NewEngine(g)

			var res *cypher.Result
			if q.write {
				res, err = engine.RunInTx(ctx, query, nil)
			} else {
				res, err = engine.Run(ctx, query, nil)
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for res.Next() {
			}
			if e := res.Err(); e != nil {
				t.Fatalf("result.Err: %v", e)
			}
			if err := res.Close(); err != nil {
				t.Fatalf("result.Close: %v", err)
			}
		})
	}
}

func BenchmarkIC1(b *testing.B)  { benchmarkQuery(b, "ic1.cypher") }
func BenchmarkIC2(b *testing.B)  { benchmarkQuery(b, "ic2.cypher") }
func BenchmarkIC3(b *testing.B)  { benchmarkQuery(b, "ic3.cypher") }
func BenchmarkIC4(b *testing.B)  { benchmarkQuery(b, "ic4.cypher") }
func BenchmarkIC5(b *testing.B)  { benchmarkQuery(b, "ic5.cypher") }
func BenchmarkIC6(b *testing.B)  { benchmarkQuery(b, "ic6.cypher") }
func BenchmarkIC7(b *testing.B)  { benchmarkQuery(b, "ic7.cypher") }
func BenchmarkIC8(b *testing.B)  { benchmarkQuery(b, "ic8.cypher") }
func BenchmarkIC9(b *testing.B)  { benchmarkQuery(b, "ic9.cypher") }
func BenchmarkIC10(b *testing.B) { benchmarkQuery(b, "ic10.cypher") }
func BenchmarkIC11(b *testing.B) { benchmarkQuery(b, "ic11.cypher") }
func BenchmarkIC12(b *testing.B) { benchmarkQuery(b, "ic12.cypher") }
func BenchmarkIC13(b *testing.B) { benchmarkQuery(b, "ic13.cypher") }
func BenchmarkIC14(b *testing.B) { benchmarkQuery(b, "ic14.cypher") }

// BenchmarkWithProjection exercises the WITH-clause general projection path
// (#1501). Each projected row is evaluated through the engine's
// buildRowCtx → evalRow → expr.EvalWith bridge; the WITH item `n.name AS name`
// is a property-access expression (not a bare-variable fast path), so every
// matched row builds a per-row RowContext and dispatches through EvalWith.
// The query matches all seeded nodes, so the per-row cost dominates and any
// regression or improvement on that path is visible in allocs/op.
//
// This benchmark is the focused WITH-path coverage the curated benchmark set
// otherwise lacks: the LDBC IC queries are MATCH/WHERE/RETURN and never carry
// a WITH clause.
func BenchmarkWithProjection(b *testing.B) {
	benchmarkInlineRead(b, "MATCH (n) WITH n.name AS name RETURN name")
}

// benchmarkInlineRead runs a read-only inline query against the shared
// benchGraph for b.N iterations, draining and closing the result each time.
func benchmarkInlineRead(b *testing.B, query string) {
	b.Helper()
	engine := cypher.NewEngine(benchGraph)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := engine.Run(ctx, query, nil)
		if err != nil {
			b.Fatalf("Run(%q): %v", query, err)
		}
		for res.Next() {
		}
		if e := res.Err(); e != nil {
			b.Fatalf("result.Err(%q): %v", query, e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("result.Close(%q): %v", query, err)
		}
	}
}

// BenchmarkIC1_Parallel measures IC1 throughput across GOMAXPROCS goroutines.
// IC1 is the broadest read query (all nodes); it exercises the full scan path
// under concurrent load.
func BenchmarkIC1_Parallel(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := engine.Run(ctx, "MATCH (n) RETURN n", nil)
			if err != nil {
				b.Fatal(err)
			}
			for res.Next() {
			}
			if e := res.Err(); e != nil {
				b.Fatal(e)
			}
			_ = res.Close()
		}
	})
}

// BenchmarkIC2_Parallel measures IC2 throughput (Person label scan) under
// concurrent load. IC2 exercises label-filtered node iteration.
func BenchmarkIC2_Parallel(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := engine.Run(ctx, "MATCH (n:Person) RETURN n", nil)
			if err != nil {
				b.Fatal(err)
			}
			for res.Next() {
			}
			if e := res.Err(); e != nil {
				b.Fatal(e)
			}
			_ = res.Close()
		}
	})
}

// BenchmarkIC9_Parallel measures IC9 throughput (Person nodes with IS NOT NULL
// property filter) under concurrent load.
func BenchmarkIC9_Parallel(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := engine.Run(ctx, "MATCH (n:Person) WHERE n.age IS NOT NULL RETURN n", nil)
			if err != nil {
				b.Fatal(err)
			}
			for res.Next() {
			}
			if e := res.Err(); e != nil {
				b.Fatal(e)
			}
			_ = res.Close()
		}
	})
}

// BenchmarkIC3_Parallel — concurrent Person full-scan (read-only, GOMAXPROCS goroutines).
func BenchmarkIC3_Parallel(b *testing.B) { benchmarkQueryParallel(b, "ic3.cypher") }

// BenchmarkIC4_Parallel — concurrent Person IS NOT NULL filter (read-only).
func BenchmarkIC4_Parallel(b *testing.B) { benchmarkQueryParallel(b, "ic4.cypher") }

// BenchmarkIC7_Parallel — concurrent City scan (read-only).
func BenchmarkIC7_Parallel(b *testing.B) { benchmarkQueryParallel(b, "ic7.cypher") }

// BenchmarkIC10_Parallel — concurrent property projection (MATCH … RETURN n.name).
func BenchmarkIC10_Parallel(b *testing.B) { benchmarkQueryParallel(b, "ic10.cypher") }

// BenchmarkIC11_Parallel — concurrent WHERE filter on boolean property.
func BenchmarkIC11_Parallel(b *testing.B) { benchmarkQueryParallel(b, "ic11.cypher") }

// BenchmarkIC14_Parallel — concurrent Company scan (read-only).
func BenchmarkIC14_Parallel(b *testing.B) { benchmarkQueryParallel(b, "ic14.cypher") }

// benchmarkQueryParallel runs query from file concurrently under GOMAXPROCS
// goroutines using b.RunParallel. Only read-only queries are supported; write
// queries must use a dedicated fresh graph and cannot share state safely.
func benchmarkQueryParallel(b *testing.B, file string) {
	b.Helper()
	qBytes, err := os.ReadFile(filepath.Join("queries", file)) //nolint:gosec // path is a fixed test fixture
	if err != nil {
		b.Fatalf("read %s: %v", file, err)
	}
	query := strings.TrimSpace(string(qBytes))
	engine := cypher.NewEngine(benchGraph)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := engine.Run(ctx, query, nil)
			if err != nil {
				b.Fatal(err)
			}
			for res.Next() {
			}
			if e := res.Err(); e != nil {
				b.Fatal(e)
			}
			_ = res.Close()
		}
	})
}
