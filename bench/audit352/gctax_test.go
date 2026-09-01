package audit352_test

// gctax_test.go — the short-layer remainder of the GC-tax concern.
//
// The resident-graph GC tax sweep itself, TestGCTax_ResidentGraph, lives in
// gctax_soak_test.go: it is a measurement with no assertion on what it measures
// and it cost 138.37 s of this package's 399.77 s under -race (rmp #2667).

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// --- concurrency ----------------------------------------------------------

// BenchmarkConcurrentRead drives the same read query from G goroutines
// against one shared engine, at the concurrency levels the module publishes.
// Run with -blockprofile / -mutexprofile to attribute any serialisation.
//
//	go test -run='^$' -bench='^BenchmarkConcurrentRead$' -benchmem -count=5 \
//	    -blockprofile=block.pb.gz -mutexprofile=mutex.pb.gz ./bench/audit352/
func BenchmarkConcurrentRead(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	// A selective query, so each goroutine does real scan+filter work but
	// ships few rows — the shape a concurrent read fleet actually runs.
	const q = `MATCH (p:Person) WHERE p.bucket < 2 RETURN p.salary`
	for _, g := range []int{1, 8, 64} {
		g := g
		b.Run(fmt.Sprintf("goroutines=%03d", g), func(b *testing.B) {
			ctx := context.Background()
			b.ReportAllocs()
			b.SetParallelism(g)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					res, err := engine.Run(ctx, q, nil)
					if err != nil {
						b.Fatalf("Run: %v", err)
					}
					n := 0
					for res.Next() {
						n++
					}
					if e := res.Err(); e != nil {
						b.Fatalf("Err: %v", e)
					}
					if n != nodeCount*2/100 {
						b.Fatalf("shipped %d rows, want %d", n, nodeCount*2/100)
					}
					if err := res.Close(); err != nil {
						b.Fatalf("Close: %v", err)
					}
				}
			})
		})
	}
}
