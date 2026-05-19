package flow

import "testing"

func TestMaxFlow_CLRS(t *testing.T) {
	t.Parallel()
	// CLRS Fig. 26.1(a): s=0, t=5; max flow = 23.
	g := NewNetwork(6)
	g.AddEdge(0, 1, 16)
	g.AddEdge(0, 2, 13)
	g.AddEdge(1, 2, 10)
	g.AddEdge(2, 1, 4)
	g.AddEdge(1, 3, 12)
	g.AddEdge(2, 4, 14)
	g.AddEdge(3, 2, 9)
	g.AddEdge(3, 5, 20)
	g.AddEdge(4, 3, 7)
	g.AddEdge(4, 5, 4)
	if got := MaxFlow(g, 0, 5); got != 23 {
		t.Fatalf("MaxFlow = %d, want 23", got)
	}
}

func TestMaxFlow_NoPath(t *testing.T) {
	t.Parallel()
	g := NewNetwork(3)
	g.AddEdge(0, 1, 10)
	if got := MaxFlow(g, 0, 2); got != 0 {
		t.Fatalf("MaxFlow = %d, want 0 when sink unreachable", got)
	}
}

// TestMaxFlow_DeepPipeline runs Dinic on a linear pipeline V=1e5 nodes
// (each only connected to the next). The previous recursive
// augmentFlow would blow the goroutine stack at this depth; the
// iterative replacement must complete cleanly.
func TestMaxFlow_DeepPipeline(t *testing.T) {
	t.Parallel()
	const n = 100_000
	g := NewNetwork(n)
	for i := 0; i < n-1; i++ {
		g.AddEdge(i, i+1, 1)
	}
	if got := MaxFlow(g, 0, n-1); got != 1 {
		t.Fatalf("MaxFlow on pipeline = %d, want 1", got)
	}
}

// BenchmarkDinic_Pipeline measures Dinic on a moderately deep
// pipeline so the per-augment work is dominated by stack manipulation
// — the new iterative version should match the recursive one within
// 5% on this geometry (task #130 acceptance).
func BenchmarkDinic_Pipeline(b *testing.B) {
	const n = 1024
	g := NewNetwork(n)
	for i := 0; i < n-1; i++ {
		g.AddEdge(i, i+1, 100)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Reset capacities between runs.
		for j := range g.cap {
			if j%2 == 0 {
				g.cap[j] = 100
			} else {
				g.cap[j] = 0
			}
		}
		_ = MaxFlow(g, 0, n-1)
	}
}
