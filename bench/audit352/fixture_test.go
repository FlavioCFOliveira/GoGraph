// Package audit352_test is a purpose-built exercise harness for the
// 2026-08-27 bottleneck audit (rmp #352). It is an INSTRUMENT, not part of
// the module: nothing in GoGraph imports it.
//
// Design notes — each guards against a measurement trap this project has
// been bitten by before:
//
//   - The fixture is built ONCE in TestMain and never mutated. No benchmark
//     arm may change the setup cost, so setup can never scale with the
//     independent variable.
//   - Every node carries BOTH a small-integer property (`age`, 18..82, inside
//     the Go runtime's staticuint64s window where interface boxing of an
//     integer is allocation-free) and a large-integer property (`salary`,
//     >=100000, outside it). Comparing the two isolates the cost of interface
//     boxing itself without changing anything else about the query.
//   - `bucket` is i%100, so a `bucket < K` predicate selects exactly K% of
//     rows with a comparison whose cost does not vary with K. That makes rows
//     SHIPPED the only variable while rows PRODUCED (the full label scan)
//     stays fixed at nodeCount.
//   - assertSamePlan fails the run if two arms of a sweep do not compile to
//     the identical physical plan. A sweep that silently changed plan would
//     measure two different programs.
package audit352_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	// nodeCount matches bench/cypher_scale so the two harnesses are
	// comparable; comfortably above the 100k threshold at which the
	// per-scanned-entity cost is visible.
	nodeCount = 120_000
	// outDegree gives ~960k directed :KNOWS edges.
	outDegree = 8
	// stride is prime and coprime with nodeCount's factors, so neighbours
	// are spread across the id space deterministically without an RNG.
	stride = 104_729
)

// benchGraph is the shared, read-only fixture.
var benchGraph *lpg.Graph[string, float64]

func TestMain(m *testing.M) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < nodeCount; i++ {
		key := fmt.Sprintf("n%d", i)
		must(g.AddNode(key))
		must(g.SetNodeLabel(key, "Person"))
		must(g.SetNodeProperty(key, "firstName", lpg.StringValue(fmt.Sprintf("Person%d", i))))
		// Small integer: 18..82. Inside staticuint64s — boxing is free.
		must(g.SetNodeProperty(key, "age", lpg.Int64Value(int64(18+i%65))))
		// Large integer: 100000..164999. Outside staticuint64s — boxing allocates.
		must(g.SetNodeProperty(key, "salary", lpg.Int64Value(int64(100_000+i%65_000))))
		// Selectivity dial: exactly i%100, so `bucket < K` selects K% of rows.
		must(g.SetNodeProperty(key, "bucket", lpg.Int64Value(int64(i%100))))
	}
	for i := 0; i < nodeCount; i++ {
		src := fmt.Sprintf("n%d", i)
		for k := 1; k <= outDegree; k++ {
			dst := fmt.Sprintf("n%d", (i+k*stride)%nodeCount)
			must(g.AddEdge(src, dst, 0))
			g.SetEdgeLabel(src, dst, "KNOWS")
		}
	}
	benchGraph = g
	os.Exit(m.Run())
}

func must(err error) {
	if err != nil {
		log.Fatalf("fixture: %v", err)
	}
}

// runQuery drives one query to full completion on a shared warm Engine.
// It is the single execution primitive every benchmark in this package uses,
// so no arm can differ from another by how it consumes the result.
//
// It brackets the timed loop with an Explain on the SAME engine and fails if
// the physical plan changed underneath the measurement. That is not
// theoretical here: the engine refreshes planner statistics off the write
// path, and a shape measured in this package was observed planning as
// columnarExpand on a fresh engine and as a row-based Expand on a warmed one.
// A benchmark whose plan changes mid-run is measuring two programs and
// averaging them.
func runQuery(b *testing.B, engine *cypher.Engine, query string) {
	b.Helper()
	ctx := context.Background()
	planBefore, err := engine.Explain(query, nil)
	if err != nil {
		b.Fatalf("Explain(%q): %v", query, err)
	}
	defer func() {
		planAfter, err := engine.Explain(query, nil)
		if err != nil {
			b.Fatalf("Explain after(%q): %v", query, err)
		}
		if planAfter != planBefore {
			b.Fatalf("PLAN DRIFTED during benchmark of %q\n--- before ---\n%s\n--- after ---\n%s",
				query, planBefore, planAfter)
		}
	}()
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
			b.Fatalf("Err(%q): %v", query, e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Close(%q): %v", query, err)
		}
	}
}

// countRows executes query once and returns the number of rows it actually
// shipped. Every sweep asserts against this, so an arm that produced a
// different row count than the sweep assumes fails loudly instead of
// silently measuring a different workload.
func countRows(tb testing.TB, engine *cypher.Engine, query string) int {
	tb.Helper()
	res, err := engine.Run(context.Background(), query, nil)
	if err != nil {
		tb.Fatalf("countRows Run(%q): %v", query, err)
	}
	n := 0
	for res.Next() {
		n++
	}
	if e := res.Err(); e != nil {
		tb.Fatalf("countRows Err(%q): %v", query, e)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("countRows Close(%q): %v", query, err)
	}
	return n
}

// assertSamePlan fails unless every query compiles to the identical physical
// plan. A selectivity sweep whose arms plan differently is not a sweep.
func assertSamePlan(tb testing.TB, engine *cypher.Engine, queries []string) {
	tb.Helper()
	var first string
	for i, q := range queries {
		p, err := engine.Explain(q, nil)
		if err != nil {
			tb.Fatalf("Explain(%q): %v", q, err)
		}
		if i == 0 {
			first = p
			continue
		}
		if strings.TrimSpace(p) != strings.TrimSpace(first) {
			tb.Fatalf("plan differs between sweep arms.\n--- arm 0 (%s) ---\n%s\n--- arm %d (%s) ---\n%s",
				queries[0], first, i, q, p)
		}
	}
}
