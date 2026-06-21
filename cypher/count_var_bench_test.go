package cypher_test

// count_var_bench_test.go — empirical evidence for #1654 (audit M2).
//
// count(<bare node/relationship variable>) only null-checks its argument
// (funcs.CountAgg.Step → !IsNull), yet before #1654 the aggregate-input
// pre-projection upgraded every matched row's variable to a full
// expr.NodeValue / expr.RelationshipValue — for a relationship that entails a
// per-row EdgeProperties / EdgeLabels read — purely to discard it. The fix
// passes the raw row cell (IntegerValue(NodeID) / relationship reference, or
// Null) straight to the aggregator. These benchmarks isolate that per-row
// materialisation over a relationship-dense multigraph. Layer: short.
//
// Run with:
//
//	go test -run=^$ -bench=BenchmarkCount -benchmem -count=6 ./cypher/

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// BenchmarkCountRelVar measures count(<relationship variable>) over a
// property-bearing multigraph: the dominant case from example 22's
// count(KNOWS). Before #1654 each of the ~nNodes*fanout*2 matched rows
// rebuilt a full RelationshipValue (with its `w` property) just to null-check
// it; after, the raw cell flows to the aggregator.
func BenchmarkCountRelVar(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	seedRelGraph(b, eng, 400, 4) // ~3.2k relationships
	const q = "MATCH (a)-[r]->(b) RETURN count(r)"
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

// BenchmarkCountNodeVar measures count(<node variable>) — the same fix on the
// node side (raw NodeID cell instead of an upgraded NodeValue per row).
func BenchmarkCountNodeVar(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	seedRelGraph(b, eng, 400, 4)
	const q = "MATCH (a)-[r]->(b) RETURN count(a)"
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
