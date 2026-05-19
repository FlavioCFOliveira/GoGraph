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
