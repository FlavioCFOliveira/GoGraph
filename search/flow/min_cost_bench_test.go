package flow

import "testing"

// BenchmarkMinCostMaxFlow stresses the successive-shortest-paths loop with a
// complete bipartite transportation network whose middle arcs are unit
// capacity, so the maximum flow is pushed one augmenting path at a time —
// hundreds of SSP iterations per solve. Each iteration ran its Dijkstra on a
// freshly allocated container/heap priority queue; the monomorphic heap is now
// allocated once and reused, so the per-iteration heap allocation (and the
// per-item any-boxing) is gone. The network's residual capacities are restored
// from a snapshot before each solve (an allocation-free copy) so the timed
// region is the solver, not fixture construction.
func BenchmarkMinCostMaxFlow(b *testing.B) {
	const w = 24 // sources and sinks per side
	src := 0
	sink := 2*w + 1
	g := NewCostNetwork(2*w + 2)
	for i := 0; i < w; i++ {
		g.AddCostEdge(src, 1+i, w, 0)    // src -> source_i
		g.AddCostEdge(1+w+i, sink, w, 0) // sink_i -> sink
		for j := 0; j < w; j++ {
			// Complete bipartite middle: unit capacity, deterministic cost.
			cost := (i*31+j*17)%97 + 1
			g.AddCostEdge(1+i, 1+w+j, 1, cost)
		}
	}

	// Snapshot the initial residual capacities so each timed solve starts from
	// the same network without reallocating the fixture.
	capBackup := make([]int, len(g.cap))
	copy(capBackup, g.cap)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		copy(g.cap, capBackup)
		MinCostMaxFlow(g, src, sink)
	}
}
