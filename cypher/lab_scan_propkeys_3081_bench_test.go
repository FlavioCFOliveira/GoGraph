package cypher_test

// lab_scan_propkeys_3081_bench_test.go — rmp #3081 evidence: the two example-25
// /stats and /schema statements that drive AllNodesScan.Init (under Expand) and
// lpg.Graph.PropertyKeysInUse, on a 20k-node / 80k-edge property graph.
//
// Run with:
//
//	GOGRAPH_FSYNC_LATENCY=off go test -run=^$ -bench=Lab3081 -benchmem ./cypher/
//
// Layer: short (benchmarks run only under -bench).

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newLab3081Engine(b *testing.B) *cypher.Engine {
	b.Helper()
	const nodes, fanout = 20000, 4
	g := lpg.New[string, float64](adjlist.Config{})
	name := func(i int) string { return fmt.Sprintf("t%d", i) }
	for i := 0; i < nodes; i++ {
		if err := g.AddNode(name(i)); err != nil {
			b.Fatal(err)
		}
		if err := g.SetNodeLabel(name(i), "Task"); err != nil {
			b.Fatal(err)
		}
		if err := g.SetNodeProperty(name(i), "title", lpg.StringValue(name(i))); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < nodes; i++ {
		for f := 1; f <= fanout; f++ {
			j := (i + f*7) % nodes
			if err := g.AddEdge(name(i), name(j), 1); err != nil {
				b.Fatal(err)
			}
			if err := g.SetEdgeLabel(name(i), name(j), "DEPENDS_ON"); err != nil {
				b.Fatal(err)
			}
			if err := g.SetEdgeProperty(name(i), name(j), "kind", lpg.StringValue("hard")); err != nil {
				b.Fatal(err)
			}
			if err := g.SetEdgeProperty(name(i), name(j), "weight", lpg.Int64Value(int64(f))); err != nil {
				b.Fatal(err)
			}
		}
	}
	return cypher.NewEngine(g)
}

func benchLab3081Query(b *testing.B, q string, wantRows int) {
	eng := newLab3081Engine(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if err := res.Err(); err != nil {
			b.Fatal(err)
		}
		_ = res.Close()
		if n != wantRows {
			b.Fatalf("%q: %d rows, want %d", q, n, wantRows)
		}
	}
}

// BenchmarkLab3081_CountRelType is example 25's /stats per-type count: the plan
// is AllNodesScan → Expand → count, so it drives AllNodesScan.Init.
func BenchmarkLab3081_CountRelType(b *testing.B) {
	benchLab3081Query(b, "MATCH ()-[r:DEPENDS_ON]->() RETURN count(r) AS n", 1)
}

// BenchmarkLab3081_PropertyKeys is example 25's /schema property-key listing:
// CALL db.propertyKeys() → lpg.Graph.PropertyKeysInUse.
func BenchmarkLab3081_PropertyKeys(b *testing.B) {
	benchLab3081Query(b, "CALL db.propertyKeys()", 3)
}
