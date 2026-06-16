package cypher_test

// exectx_readonly_bench_test.go — empirical evidence for task #1573. It
// contrasts the throughput of a multi-statement READ transaction driven the
// two ways the Bolt server can open it:
//
//   - BenchmarkReadTx_WriterLock   — via Engine.BeginTx (the mode="w"/legacy
//     path): every transaction acquires the store single-writer serialisation
//     and the exclusive visibility barrier, so concurrent read transactions
//     fully serialise.
//   - BenchmarkReadTx_LockFree      — via Engine.BeginReadTx (the new mode="r"
//     path): no writer lock, no exclusive barrier; each statement runs under a
//     per-statement Graph.View RLock, so concurrent read transactions run in
//     parallel.
//
// Both execute the IDENTICAL read workload (two MATCH...RETURN statements per
// transaction) under b.RunParallel, so the delta isolates the lock-contention
// removal. Run with:
//
//	go test -run=^$ -bench=BenchmarkReadTx -benchmem ./cypher/
import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newBenchGraph builds a fresh store-less directed graph for the benchmarks,
// mirroring storelessEngineWithGraph without a *testing.T.
func newBenchGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{Directed: true})
}

// benchSeedNodes seeds n nodes via autocommit writes so the read workload has
// a stable graph to scan.
func benchSeedNodes(b *testing.B, eng *cypher.Engine, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		res, err := eng.RunInTx(context.Background(), "CREATE (:Bench {v:1})", nil)
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

// drainBench drains and closes a Result inside a benchmark, failing on error.
func drainBench(b *testing.B, res *cypher.Result, err error) {
	b.Helper()
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

const benchReadQuery = "MATCH (n:Bench) RETURN count(n) AS c"

// BenchmarkReadTx_WriterLock measures concurrent read transactions opened via
// the writer-serialised path (Engine.BeginTx) — the legacy behaviour for an
// explicit Bolt transaction regardless of mode.
func BenchmarkReadTx_WriterLock(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	benchSeedNodes(b, eng, 200)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				b.Fatalf("BeginTx: %v", err)
			}
			r1, e1 := tx.Exec(benchReadQuery, map[string]expr.Value(nil))
			drainBench(b, r1, e1)
			r2, e2 := tx.Exec(benchReadQuery, map[string]expr.Value(nil))
			drainBench(b, r2, e2)
			if err := tx.Commit(); err != nil {
				b.Fatalf("Commit: %v", err)
			}
		}
	})
}

// BenchmarkReadTx_LockFree measures the SAME read workload opened via the new
// read-only path (Engine.BeginReadTx, mode="r"): no writer lock, no exclusive
// barrier, so concurrent read transactions run in parallel.
func BenchmarkReadTx_LockFree(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	benchSeedNodes(b, eng, 200)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tx, err := eng.BeginReadTx(context.Background())
			if err != nil {
				b.Fatalf("BeginReadTx: %v", err)
			}
			r1, e1 := tx.Exec(benchReadQuery, map[string]expr.Value(nil))
			drainBench(b, r1, e1)
			r2, e2 := tx.Exec(benchReadQuery, map[string]expr.Value(nil))
			drainBench(b, r2, e2)
			if err := tx.Rollback(); err != nil {
				b.Fatalf("Rollback: %v", err)
			}
		}
	})
}
