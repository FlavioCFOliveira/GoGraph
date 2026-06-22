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

// BenchmarkCountAllNodes is the multithread-audit M4 / #1673 query verbatim:
// MATCH (n) RETURN count(n) over 2000 nodes, no edges, no label. It isolates
// the per-row Project scratch-header allocation: the projection that feeds the
// aggregator pulled its child row into a fresh `var inputRow Row` whose address
// escaped through the Operator.Next interface boundary, costing one heap header
// per scanned row (≈1 alloc/node, on top of the AllNodesScan id box). Reusing a
// single Project.inputRow field removes that per-row header. Layer: short.
//
// Run with:
//
//	go test -run=^$ -bench=BenchmarkCountAllNodes -benchmem -count=8 ./cypher/
func BenchmarkCountAllNodes(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	seedNodesOnly(b, eng, 2000)
	const q = "MATCH (n) RETURN count(n)"
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

// seedNodesOnly inserts n bare nodes (single property) via autocommit writes,
// giving BenchmarkCountAllNodes an edge-free graph to scan.
func seedNodesOnly(b *testing.B, eng *cypher.Engine, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		res, err := eng.RunInTx(context.Background(), "CREATE ({v:1})", nil)
		if err != nil {
			b.Fatalf("seed: %v", err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatalf("seed drain: %v", err)
		}
		_ = res.Close()
	}
}
