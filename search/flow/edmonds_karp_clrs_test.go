package flow

import (
	"testing"
	"time"
)

// TestEdmondsKarp_CLRS_Bad exercises Edmonds-Karp on the classic
// Ford-Fulkerson worst-case network. Naïve augmenting-path methods
// may require O(C) iterations on this topology; Edmonds-Karp's BFS
// shortest-path selection bounds augmentations to O(VE) regardless
// of capacity magnitude.
//
// Topology (4 vertices: s=0, a=1, b=2, t=3):
//
//	s -> a  cap C
//	s -> b  cap C
//	a -> b  cap 1
//	a -> t  cap C
//	b -> t  cap C
//
// Max flow = 2C (path s->a->t carries C, path s->b->t carries C).
// With C = 1_000_000 the naïve method would take 2*C iterations;
// Edmonds-Karp must complete in well under 1 s.
func TestEdmondsKarp_CLRS_Bad(t *testing.T) {
	t.Parallel()

	const C = 1_000_000
	const wantFlow = 2 * C

	build := func() *Network {
		g := NewNetwork(4)
		g.AddEdge(0, 1, C) // s -> a
		g.AddEdge(0, 2, C) // s -> b
		g.AddEdge(1, 2, 1) // a -> b  (the troublesome bottleneck edge)
		g.AddEdge(1, 3, C) // a -> t
		g.AddEdge(2, 3, C) // b -> t
		return g
	}

	start := time.Now()
	ekGot := EdmondsKarp(build(), 0, 3)
	dur := time.Since(start)

	if ekGot != wantFlow {
		t.Fatalf("EdmondsKarp = %d, want %d", ekGot, wantFlow)
	}
	if dur > time.Second {
		t.Fatalf("EdmondsKarp took %v, want < 1s (BFS bound must hold)", dur)
	}

	// Dinic must agree on the flow value.
	dnGot := MaxFlow(build(), 0, 3)
	if dnGot != wantFlow {
		t.Fatalf("MaxFlow = %d, want %d", dnGot, wantFlow)
	}
	if ekGot != dnGot {
		t.Fatalf("EdmondsKarp=%d != MaxFlow=%d: implementations disagree", ekGot, dnGot)
	}
}
