package server

// Whole-node return benchmark across the full lpg → expr → PackStream seam
// (#1502). The Bolt encode path is not part of the curated bench-history set,
// so this benchmark isolates the chain that the seam fix targets: a real
// `MATCH (n) RETURN n` query materialises NodeValues from storage through
// cypher (upgradeNodeIDToValue → nodePropsToExprMap), the Bolt PULL path reads
// each row positionally via Result.ValueAt, and exprValueToPackstream encodes
// every value into the wire map.
//
// BenchmarkNodeReturnToPackstream measures the end-to-end allocation profile of
// returning whole nodes to the wire; the property map and label set are the
// dominant allocators, so the #1502 fix (one copy out of storage instead of a
// throwaway intermediate map) shows up here as fewer allocs/op and B/op while
// the curated cypher_ldbc set stays flat.
//
// Layer: short (no build tag).

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildWholeNodeGraph builds a graph of n nodes, each carrying propsPerNode
// string properties and two labels, so a `RETURN n` query exercises the full
// property-map + label materialisation per node.
func buildWholeNodeGraph(tb testing.TB, n, propsPerNode int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeLabel(key, "User"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		for p := 0; p < propsPerNode; p++ {
			pk := fmt.Sprintf("prop%d", p)
			if err := g.SetNodeProperty(key, pk, lpg.StringValue(fmt.Sprintf("value-%d-%d", i, p))); err != nil {
				tb.Fatalf("SetNodeProperty: %v", err)
			}
		}
	}
	return g
}

// BenchmarkNodeReturnToPackstream drives whole-node return through the complete
// lpg → expr → PackStream chain, reading rows positionally exactly as the Bolt
// PULL path does and encoding each value with exprValueToPackstream.
func BenchmarkNodeReturnToPackstream(b *testing.B) {
	const (
		nodes        = 200
		propsPerNode = 8
		boltMajor    = 5
	)
	g := buildWholeNodeGraph(b, nodes, propsPerNode)
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		ncols := len(res.Columns())
		for res.Next() {
			for c := 0; c < ncols; c++ {
				ev := res.ValueAt(c)
				if ev == nil {
					continue
				}
				_ = exprValueToPackstream(ev, boltMajor)
			}
		}
		if err := res.Err(); err != nil {
			b.Fatalf("Result.Err: %v", err)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Result.Close: %v", err)
		}
	}
}
