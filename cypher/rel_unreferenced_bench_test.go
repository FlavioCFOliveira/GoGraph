package cypher_test

// rel_unreferenced_bench_test.go — regression benchmark for sprint 221 / #1630
// (F4a). A relationship bound by a pattern but never referenced by the query
// must NOT have its full RelationshipValue (and the per-row EdgeProperties /
// EdgeLabels reads it entails) materialised. Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// BenchmarkRelBoundUnreferenced binds a property-bearing relationship r in the
// pattern but never reads it (the WHERE and RETURN touch only the node). Before
// #1630 the executor built the full RelationshipValue per matched row,
// fetching every edge's `w` property; after, r is demand-gated away.
func BenchmarkRelBoundUnreferenced(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	seedRelGraph(b, eng, 400, 4)
	const q = "MATCH (a)-[r]->(b) WHERE a.i >= 0 RETURN a.i"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatal(err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatal(err)
		}
		_ = res.Close()
	}
}
