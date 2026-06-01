//go:build stress

// Package stress holds CI-only stress tests: a short concurrent
// workload that exercises the read-mostly hot path under -race so
// scheduler-dependent issues surface in regression. Activated by
// the 'stress' build tag (and the stress job in .github/workflows/ci.yml).
package stress

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestStress_MixedReadWorkload spawns N readers across BFS, DFS and
// Dijkstra over a shared immutable CSR for a fixed wall-clock
// budget. The race detector catches any shared-state mutation.
func TestStress_MixedReadWorkload(t *testing.T) {
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < 1000; i++ {
		a.AddEdge(i, (i+1)%1000, int64(i%10+1))
		if i%5 == 0 {
			a.AddEdge(i, (i+17)%1000, int64(i%7+1))
		}
	}
	c := csr.BuildFromAdjList(a)

	deadline := time.Now().Add(2 * time.Second)
	ctx := context.Background()

	const goroutines = 32
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				src, _ := a.Mapper().Lookup(seed % 1000)
				_ = search.BFSCtx(ctx, c, src, func(_ graph.NodeID, _ int) bool { return true })
				_ = search.DFSCtx(ctx, c, src, func(_ graph.NodeID, _ int) bool { return true })
				_, _ = search.DijkstraCtx(ctx, c, src)
				seed++
			}
		}(g * 11)
	}
	wg.Wait()
}
