package cypher_test

// agg_columnar_arg_bench_test.go — evidence for #2185: the columnar aggregation
// pre-projection must fill its aggregate ARGUMENT column unboxed, exactly as it
// already fills the grouping key.
//
// The round-3 audit (docs/audit-2026-07-26-streams/s05-runtime.md F4) measured a
// 1.0 → 7.7 allocations-per-input-row step between an aggregate with no argument
// (count(*)) and one whose argument is a plain node property (min/sum/avg of n.v),
// and attributed the whole step to the argument filler: the grouping key went
// through buildScalarPropertyFiller (unboxed, straight off the raw NodeID) while
// every argument went through evalPutColumnFiller (the boxed row evaluator).
//
// These benchmarks reproduce that comparison on the same shapes so the fix can be
// A/B'd with benchstat. Read allocs/op divided by aggBenchN as the allocations per
// input row; the count(*) arms are the floor the property arms must approach.
//
//	go test -run=^$ -bench='BenchmarkColumnarAggArg' -benchmem -count=6 ./cypher/
//
// Every arm disables the parallel scan so the serial columnar EagerAggregation is
// what is measured; the parallel aggregate scan is a different physical path (and a
// different axis — task #2187), and letting it engage at aggBenchN would mask the
// pre-projection cost this file is about.
//
// Layer: short.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// aggBenchN is the audit's input size: 100 000 rows, large enough that the
// per-row filler cost dominates the fixed query cost.
const aggBenchN = 100_000

// aggBenchGroups is the audit's group count: 7 groups, so the grouping hash map
// stays tiny and the measurement is about the per-row fillers, not group upkeep.
const aggBenchGroups = 7

// seedAggArgGraph builds an aggBenchN-node graph carrying an int64 `v` (the
// aggregate argument) and an int64 `w` (the grouping key, aggBenchGroups distinct
// values). Both are plain scalars, which is precisely the shape
// buildScalarPropertyFiller can serve unboxed.
func seedAggArgGraph(b *testing.B) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < aggBenchN; i++ {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			b.Fatalf("SetNodeProperty v: %v", err)
		}
		if err := g.SetNodeProperty(k, "w", lpg.Int64Value(int64(i%aggBenchGroups))); err != nil {
			b.Fatalf("SetNodeProperty w: %v", err)
		}
	}
	return g
}

// benchAggArg runs q over the seeded graph with the parallel scan disabled.
func benchAggArg(b *testing.B, q string) {
	silenceBenchLogs(b)
	g := seedAggArgGraph(b)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, q)
	}
}

// ── grouped: the argument is the only difference between the two arms ──

// BenchmarkColumnarAggArg_GroupCount is the floor: no aggregate argument, so no
// argument filler runs at all.
func BenchmarkColumnarAggArg_GroupCount(b *testing.B) {
	benchAggArg(b, "MATCH (n) RETURN n.w AS w, count(*) AS c")
}

func BenchmarkColumnarAggArg_GroupMin(b *testing.B) {
	benchAggArg(b, "MATCH (n) RETURN n.w AS w, min(n.v) AS m")
}

func BenchmarkColumnarAggArg_GroupMax(b *testing.B) {
	benchAggArg(b, "MATCH (n) RETURN n.w AS w, max(n.v) AS m")
}

func BenchmarkColumnarAggArg_GroupSum(b *testing.B) {
	benchAggArg(b, "MATCH (n) RETURN n.w AS w, sum(n.v) AS s")
}

func BenchmarkColumnarAggArg_GroupAvg(b *testing.B) {
	benchAggArg(b, "MATCH (n) RETURN n.w AS w, avg(n.v) AS a")
}

// ── global (no grouping key): the same argument tax, on the path that
// tryBuildColumnarAggInput declines outright today ──

func BenchmarkColumnarAggArg_GlobalMin(b *testing.B) {
	benchAggArg(b, "MATCH (n) RETURN min(n.v) AS m")
}

func BenchmarkColumnarAggArg_GlobalSum(b *testing.B) {
	benchAggArg(b, "MATCH (n) RETURN sum(n.v) AS s")
}

func BenchmarkColumnarAggArg_GlobalAvg(b *testing.B) {
	benchAggArg(b, "MATCH (n) RETURN avg(n.v) AS a")
}
