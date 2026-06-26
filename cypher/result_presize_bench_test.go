package cypher

// result_presize_bench_test.go — benchmark for #1720: presizing the matRows
// backing slice in Result.materialize() for full-scan plans.
//
// On a full-scan RETURN the materialise drain appends every row's values into a
// single flat backing slice (matRows). Without a capacity hint, append grows the
// slice geometrically, reallocating and copying O(log N) times across a large
// scan. AllNodesScan knows its exact node count after Init, so the row count is a
// knowable upper bound; presizing matRows from it makes the slice allocate once.
//
// This benchmark drains a sizeable whole-graph MATCH and reports ns/op +
// allocs/op + B/op so a benchstat before/after comparison proves the win. It
// lives in the white-box cypher package so it shares the same scope as the
// existing materialise benchmarks.

import (
	"context"
	"testing"
)

// benchPresizeRows is large enough that the geometric growth of the un-presized
// matRows backing slice incurs several reallocation/copy cycles, so the
// before/after delta is a clear signal rather than noise.
const benchPresizeRows = 50_000

// BenchmarkResultMaterialize_FullScanLarge measures the end-to-end cost of
// running and fully draining a large whole-graph MATCH (a full-node scan with a
// bare-node RETURN). This is the path #1720 targets: the matRows backing slice is
// grown by append without a capacity hint, so a large scan pays repeated
// reallocation and copy. The before/after delta of this benchmark is the
// regression/improvement signal for the presize change.
func BenchmarkResultMaterialize_FullScanLarge(b *testing.B) {
	g := newBenchGraph(b, benchPresizeRows)
	eng := NewEngine(g)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		var count int
		for res.Next() {
			count++
		}
		if err := res.Err(); err != nil {
			b.Fatalf("Result.Err: %v", err)
		}
		_ = res.Close()
		if count != benchPresizeRows {
			b.Fatalf("drained %d rows, want %d", count, benchPresizeRows)
		}
	}
}
