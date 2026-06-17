package cypher_test

// rel_projection_bench_test.go — empirical evidence for task #1574.
//
// It isolates the cost of reconstructing expr.RelationshipValue per result row
// in the projection path (buildRelationshipValueFromRow → edgeHandleAtFwdPos /
// edgeInstanceIdxFor). Before #1574 each of those helpers rebuilt the entire
// forward CSR (O(V+E)) per row, making `MATCH (a)-[r]->(b) RETURN r` cost
// O(R·(V+E)) per query and dominating heap allocations in the DST profile.
//
// The benchmark seeds a multigraph (so both the stable-handle path and the
// parallel-edge instance-idx path are exercised) once, outside the timer, then
// repeatedly runs a relationship-returning query and fully drains it.
//
// Run with:
//
//	go test -run=^$ -bench=BenchmarkRelProjection -benchmem -count=6 ./cypher/
import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// seedRelGraph builds a graph of nNodes nodes connected in a ring with `fanout`
// forward edges per node, plus a parallel duplicate edge on every fanout step,
// all created via CREATE so each edge carries a stable handle. The result is a
// relationship-dense multigraph whose `MATCH (a)-[r]->(b) RETURN r` returns
// roughly nNodes*fanout*2 rows.
func seedRelGraph(b *testing.B, eng *cypher.Engine, nNodes, fanout int) {
	b.Helper()
	mk := func(q string) {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatalf("seed drain %q: %v", q, err)
		}
		_ = res.Close()
	}
	for i := 0; i < nNodes; i++ {
		mk(fmt.Sprintf("CREATE (:N {i:%d})", i))
	}
	for i := 0; i < nNodes; i++ {
		for f := 1; f <= fanout; f++ {
			j := (i + f) % nNodes
			// Two parallel CREATEs between the same endpoints exercise the
			// per-CREATE instance-idx path; distinct labels exercise the
			// per-instance label override.
			mk(fmt.Sprintf("MATCH (a:N {i:%d}),(b:N {i:%d}) CREATE (a)-[:KNOWS {w:%d}]->(b)", i, j, f))
			mk(fmt.Sprintf("MATCH (a:N {i:%d}),(b:N {i:%d}) CREATE (a)-[:LIKES {w:%d}]->(b)", i, j, f))
		}
	}
}

func benchmarkRelProjection(b *testing.B, nNodes, fanout int) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	seedRelGraph(b, eng, nNodes, fanout)
	// count(r) is the DST checker's per-tick sampled-edge probe. To evaluate
	// the aggregate the executor builds the row context for EVERY matched edge,
	// which reconstructs the full RelationshipValue per row
	// (buildRelationshipValueFromRow → edgeHandleAtFwdPos / edgeInstanceIdxFor).
	// That per-row path is exactly what #1574 targets; this query reproduces
	// the profile's dominant allocator faithfully.
	const q = "MATCH (a)-[r]->(b) RETURN count(r)"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatalf("Exec: %v", err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatalf("drain: %v", err)
		}
		_ = res.Close()
	}
}

// BenchmarkRelProjection_Small — modest graph; per-query rebuild cost is small
// but still measurable.
func BenchmarkRelProjection_Small(b *testing.B) { benchmarkRelProjection(b, 100, 3) }

// BenchmarkRelProjection_Large — relationship-dense graph where the per-row
// full-CSR rebuild dominates (the case the DST profile flagged).
func BenchmarkRelProjection_Large(b *testing.B) { benchmarkRelProjection(b, 400, 5) }

// BenchmarkScalarFilterProjection exercises the non-escaping per-row evaluation
// sites that #1575 pools: a scalar WHERE predicate (Filter closure) and a
// scalar projection (RETURN n.i). Both build a RowContext per matched row that
// is consumed and discarded, so the pooled map should cut per-row allocations.
func BenchmarkScalarFilterProjection(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	for i := 0; i < 2000; i++ {
		res, err := eng.RunInTx(context.Background(), fmt.Sprintf("CREATE (:N {i:%d, j:%d})", i, i*2), nil)
		if err != nil {
			b.Fatalf("seed: %v", err)
		}
		for res.Next() { //nolint:revive // drain
		}
		_ = res.Close()
	}
	const q = "MATCH (n:N) WHERE n.i >= 0 AND n.j >= n.i RETURN n.i, n.j"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatalf("Exec: %v", err)
		}
		for res.Next() { //nolint:revive // drain
		}
		if err := res.Err(); err != nil {
			b.Fatalf("drain: %v", err)
		}
		_ = res.Close()
	}
}
