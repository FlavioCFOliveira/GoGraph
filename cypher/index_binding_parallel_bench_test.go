package cypher

// index_binding_parallel_bench_test.go — #1723 wall-clock evidence: the CREATE
// INDEX hash backfill over a large graph. Compare -cpu=1 (serial path) against
// -cpu=N (parallel phase-2) to observe the DDL wall-clock scaling.

import (
	"context"
	"testing"
)

func BenchmarkBackfillHashIndexLarge(b *testing.B) {
	const n = 200_000
	g, _ := seedLabeledNamed(b, n, "Person")
	e := NewEngine(g)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx, err := newBoundNodeHashIndex(e.g, "Person", "name")
		if err != nil {
			b.Fatalf("newBoundNodeHashIndex: %v", err)
		}
		if berr := e.backfillNodeHashIndex(ctx, idx, "Person", "name"); berr != nil {
			b.Fatalf("backfill: %v", berr)
		}
	}
}
