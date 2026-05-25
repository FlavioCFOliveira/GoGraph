package flow

import "testing"

// TestMaxFlow_ZeroCap exercises Dinic's algorithm on a network that
// contains zero-capacity edges mixed with positive-capacity ones.
// Zero-cap edges must never carry flow; the algorithm must route
// around them and still saturate the available positive-cap paths.
//
// Topology (n=6, s=0, t=5):
//
//	s  -> 1  cap 3
//	s  -> 2  cap 3
//	1  -> 3  cap 0  (zero — must be skipped)
//	1  -> 3  cap 3  (positive parallel arc)
//	2  -> 4  cap 3
//	3  -> t  cap 3
//	4  -> t  cap 3
//	1  -> 4  cap 0  (zero — must be skipped)
//
// Two augmenting paths: s->1->3->t (flow 3) and s->2->4->t (flow 3);
// max flow = 6.
func TestMaxFlow_ZeroCap(t *testing.T) {
	t.Parallel()

	build := func() *Network {
		g := NewNetwork(6)
		g.AddEdge(0, 1, 3)
		g.AddEdge(0, 2, 3)
		g.AddEdge(1, 3, 0) // zero — must not carry flow
		g.AddEdge(1, 3, 3) // positive parallel arc
		g.AddEdge(2, 4, 3)
		g.AddEdge(3, 5, 3)
		g.AddEdge(4, 5, 3)
		g.AddEdge(1, 4, 0) // zero — must not carry flow
		return g
	}

	const wantFlow = 6

	got := MaxFlow(build(), 0, 5)
	if got != wantFlow {
		t.Fatalf("MaxFlow = %d, want %d", got, wantFlow)
	}

	ekGot := EdmondsKarp(build(), 0, 5)
	if ekGot != wantFlow {
		t.Fatalf("EdmondsKarp = %d, want %d", ekGot, wantFlow)
	}

	if got != ekGot {
		t.Fatalf("MaxFlow=%d != EdmondsKarp=%d: implementations disagree", got, ekGot)
	}
}
