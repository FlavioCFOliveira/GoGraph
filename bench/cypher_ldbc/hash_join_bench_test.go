package cypher_ldbc_test

// hash_join_bench_test.go — benchmark demonstrating the disconnected equi-join
// hash join (#1506) win: O(|A|·|B|) nested loop → O(|A|+|B|) hash join.
//
// The query `MATCH (a:HJA),(b:HJB) WHERE a.k = b.k RETURN a.k` joins two
// disconnected labelled pattern parts on an equality predicate. With the hash
// join DISABLED the engine evaluates the full Cartesian product and filters it
// (|A|·|B| candidate rows); with it ENABLED it builds a hash table on one side
// and probes with the other (|A|+|B| work). The two benchmarks below measure the
// same query on the same graph with the optimisation on and off, so the delta is
// the pure algorithmic win. The result multiset is identical (proven by the
// differential test in cypher/hash_join_diff_test.go).
//
// Run:
//
//	go test -run='^$' -bench=BenchmarkHashJoin -benchmem ./bench/cypher_ldbc/...

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// hjBenchSide is the per-label node count for the hash-join benchmark graph.
// 500×500 makes the nested-loop product (250k candidate rows) clearly dominate
// the hash-join work (1k rows) so the win is unambiguous, while staying small
// enough to run quickly in the curated bench set.
const hjBenchSide = 500

// hjBenchMod controls join multiplicity: keys are i % hjBenchMod, so each key
// value matches hjBenchSide/hjBenchMod rows on each side.
const hjBenchMod = 100

// buildHashJoinBenchGraph seeds two disconnected labelled node sets joined by an
// integer key property.
func buildHashJoinBenchGraph() *lpg.Graph[string, float64] {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < hjBenchSide; i++ {
		k := fmt.Sprintf("hja%d", i)
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "HJA")
		_ = g.SetNodeProperty(k, "k", lpg.Int64Value(int64(i%hjBenchMod)))
	}
	for i := 0; i < hjBenchSide; i++ {
		k := fmt.Sprintf("hjb%d", i)
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "HJB")
		_ = g.SetNodeProperty(k, "k", lpg.Int64Value(int64(i%hjBenchMod)))
	}
	g.SetIndexManager(index.NewManager())
	return g
}

const hashJoinBenchQuery = "MATCH (a:HJA),(b:HJB) WHERE a.k = b.k RETURN a.k AS k"

// benchHashJoin runs the disconnected equi-join query with the hash join either
// enabled or disabled, on a freshly-seeded graph.
func benchHashJoin(b *testing.B, enabled bool) {
	b.Helper()
	g := buildHashJoinBenchGraph()
	engine := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		DisableHashJoin: !enabled,
		// The result is |A|/mod * |B|/mod * mod = 500*500/100 = 2500 rows; lift
		// the row cap so neither variant trips it.
		MaxResultRows: cypher.MaxResultRowsUnlimited,
	})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := engine.Run(ctx, hashJoinBenchQuery, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		for res.Next() {
		}
		if e := res.Err(); e != nil {
			b.Fatalf("Err: %v", e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkHashJoinDisconnectedEquiJoin_HashJoin measures the optimised plan.
func BenchmarkHashJoinDisconnectedEquiJoin_HashJoin(b *testing.B) {
	benchHashJoin(b, true)
}

// BenchmarkHashJoinDisconnectedEquiJoin_NestedLoop measures the legacy nested
// loop for the same query — the O(n·m) baseline the hash join replaces.
func BenchmarkHashJoinDisconnectedEquiJoin_NestedLoop(b *testing.B) {
	benchHashJoin(b, false)
}

// TestHashJoinBench_ResultsMatch is a fast guard run as part of the normal test
// layer: it confirms the benchmark query returns the identical row count under
// both plans, so the benchmark compares like with like.
func TestHashJoinBench_ResultsMatch(t *testing.T) {
	g := buildHashJoinBenchGraph()
	on := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: cypher.MaxResultRowsUnlimited})
	off := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableHashJoin: true, MaxResultRows: cypher.MaxResultRowsUnlimited})
	count := func(e *cypher.Engine) int {
		res, err := e.Run(context.Background(), hashJoinBenchQuery, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		_ = res.Close()
		return n
	}
	non := count(on)
	noff := count(off)
	if non != noff {
		t.Fatalf("row-count mismatch: hashjoin=%d nestedloop=%d", non, noff)
	}
	want := hjBenchSide / hjBenchMod * (hjBenchSide / hjBenchMod) * hjBenchMod
	if non != want {
		t.Fatalf("unexpected row count %d, want %d", non, want)
	}
}
