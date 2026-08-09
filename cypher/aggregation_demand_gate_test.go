package cypher_test

// aggregation_demand_gate_test.go — the aggregation pre-projection must not
// materialise a relationship the query never names.
//
// Layer: short.

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestAggregationPreProjection_DoesNotMaterialiseAnUnnamedRelationship pins the
// #1630 demand gate on the aggregation pre-projection path.
//
// # Why an allocation bound and not a result comparison
//
// The gate is RESULT-IDENTICAL: it only omits variables the expression never
// names, so a differential test cannot see it at all and would go green against a
// build that had lost it. What it changes is the WORK: without the gate,
// newAggregationEval built a full value for every variable in the row once per
// row, and for an edge variable that means its type, its endpoints AND its whole
// property map. A CPU profile of examples/26_social_scale_bench attributed 17.16 %
// of all samples to buildRelationshipValueFromRow, reached only from
// populateRowCtx, of which 46.8 % was buildEdgeProps alone.
//
// So the observable is allocation, measured per matched row.
//
// # The bound
//
// Measured on this fixture (2000 nodes, 8000 edges, two properties per edge,
// go1.26.5 darwin/arm64):
//
//	ungated  21.45 allocs/row, 1374.5 B/row
//	gated    10.95 allocs/row,  774.5 B/row   (-48.9 % allocs, -43.7 % bytes)
//
// The bound is 16 allocations per row: 46 % of headroom above the gated figure and
// 25 % below the ungated one, so it separates the two arms without being tight
// enough to fail on ordinary allocation drift. Injection-validated — reverting
// newAggregationEval to the eager buildRowCtx call fails this test at 21.45.
//
// The query's answer is asserted too, so the test cannot pass by degrading it.
func TestAggregationPreProjection_DoesNotMaterialiseAnUnnamedRelationship(t *testing.T) {
	const nodes = 2000
	const outDegree = 4

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	for i := 0; i < nodes; i++ {
		k := fmt.Sprintf("u%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "U"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", k, err)
		}
	}
	// Two properties per edge, so the ungated path has a real map to build.
	for i := 0; i < nodes; i++ {
		for d := 1; d <= outDegree; d++ {
			src, dst := fmt.Sprintf("u%d", i), fmt.Sprintf("u%d", (i+d)%nodes)
			if err := g.AddEdgeLabeledWithProperty(src, dst, 1, "R", "w", lpg.Int64Value(int64(d))); err != nil {
				t.Fatalf("AddEdgeLabeledWithProperty %s->%s: %v", src, dst, err)
			}
			if err := g.SetEdgePropertyAt(src, dst, 0, "tag", lpg.StringValue("edge-property-payload")); err != nil {
				t.Fatalf("SetEdgePropertyAt %s->%s: %v", src, dst, err)
			}
		}
	}

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// A grouped aggregation over a relationship pattern that names NO relationship
	// variable — the shape the gate exists for.
	const query = "MATCH (u:U)-[:R]->(:U) WITH u, count(*) AS deg " +
		"RETURN min(deg) AS mn, max(deg) AS mx, count(u) AS users"

	runOnce := func(t *testing.T, check bool) {
		t.Helper()
		res, err := eng.Run(ctx, query, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		rows := 0
		for res.Next() {
			rows++
			if !check {
				continue
			}
			rec := res.Record()
			// Every node has exactly outDegree out-edges, so min == max == 4 and
			// every node contributes one group.
			for _, want := range []struct {
				col string
				val int64
			}{{"mn", outDegree}, {"mx", outDegree}, {"users", nodes}} {
				got, ok := rec[want.col]
				if !ok {
					t.Fatalf("column %q missing from %v", want.col, rec)
				}
				iv, isInt := got.(interface{ Int64() (int64, bool) })
				if isInt {
					if n, ok2 := iv.Int64(); !ok2 || n != want.val {
						t.Errorf("%s = %v, want %d", want.col, got, want.val)
					}
					continue
				}
				if fmt.Sprint(got) != fmt.Sprint(want.val) {
					t.Errorf("%s = %v (%T), want %d", want.col, got, got, want.val)
				}
			}
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if rows != 1 {
			t.Fatalf("got %d result rows, want 1", rows)
		}
	}

	// Correctness first, and it also warms every lazily-built structure so the
	// measured window below sees only the per-row work.
	runOnce(t, true)

	const reps = 5
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < reps; i++ {
		runOnce(t, false)
	}
	runtime.ReadMemStats(&after)

	matchedRows := uint64(nodes) * outDegree
	allocsPerRow := float64(after.Mallocs-before.Mallocs) / float64(reps) / float64(matchedRows)
	bytesPerRow := float64(after.TotalAlloc-before.TotalAlloc) / float64(reps) / float64(matchedRows)
	t.Logf("aggregation pre-projection: %.2f allocs/row, %.1f B/row over %d matched rows",
		allocsPerRow, bytesPerRow, matchedRows)

	const maxAllocsPerRow = 16.0
	if allocsPerRow > maxAllocsPerRow {
		t.Errorf("aggregation pre-projection allocated %.2f objects per matched row, want <= %.0f: "+
			"the #1630 demand gate is not engaged, so a relationship the query never names is "+
			"being materialised (type, endpoints and property map) once per row",
			allocsPerRow, maxAllocsPerRow)
	}
}
