package flow

import "testing"

// TestPushRelabel_Antiparallel checks that all three max-flow
// implementations handle anti-parallel edges (u->v and v->u
// simultaneously present) correctly.  Anti-parallel edges are
// legal in the AddEdge API: each call adds an independent forward
// edge and its own reverse residual, so u->v and v->u coexist as
// four edge records without aliasing.
//
// Topology (4 vertices: s=0, u=1, v=2, t=3):
//
//	s -> u  cap 10
//	u -> v  cap  5
//	v -> u  cap  5   ← anti-parallel with u->v
//	v -> t  cap 10
//	s -> v  cap  3
//	u -> t  cap  3
//
// The three algorithms must agree on the max-flow value.  The
// correctness check is their mutual agreement: if any implementation
// mishandles anti-parallel residuals the values diverge.
//
// Additionally, the value must be positive (non-trivial flow exists)
// and must not exceed the total capacity leaving s (10+3 = 13).
func TestPushRelabel_Antiparallel(t *testing.T) {
	t.Parallel()

	build := func() *Network {
		g := NewNetwork(4)
		g.AddEdge(0, 1, 10) // s -> u
		g.AddEdge(1, 2, 5)  // u -> v
		g.AddEdge(2, 1, 5)  // v -> u  (anti-parallel)
		g.AddEdge(2, 3, 10) // v -> t
		g.AddEdge(0, 2, 3)  // s -> v
		g.AddEdge(1, 3, 3)  // u -> t
		return g
	}

	dnGot := MaxFlow(build(), 0, 3)
	ekGot := EdmondsKarp(build(), 0, 3)
	prGot := PushRelabelMaxFlow(build(), 0, 3)

	if dnGot != ekGot || dnGot != prGot {
		t.Fatalf("implementations disagree: MaxFlow=%d EdmondsKarp=%d PushRelabelMaxFlow=%d",
			dnGot, ekGot, prGot)
	}
	// Sanity: flow must be positive and cannot exceed capacity out of source.
	const maxPossible = 10 + 3
	if dnGot <= 0 || dnGot > maxPossible {
		t.Fatalf("flow=%d out of valid range (0, %d]", dnGot, maxPossible)
	}
}
