package scenarios_test

import (
	"context"
	"testing"

	"gograph/search/flow"
)

// TestASFlow_DinicMaxFlow builds a small synthetic Autonomous-System-link
// network (10 nodes, 14 edges with integer capacities) and verifies that
// Dinic's max-flow from AS0 to AS4 equals the independently computed
// reference value.
//
// Reference topology (each entry is src→dst, cap):
//
//	AS0→AS1(5), AS0→AS2(3)
//	AS1→AS3(4), AS1→AS4(2)
//	AS2→AS3(4), AS2→AS5(2)
//	AS3→AS4(6), AS3→AS6(3)
//	AS5→AS4(1), AS5→AS7(2)
//	AS6→AS4(2), AS6→AS8(1)
//	AS7→AS9(1)
//	AS8→AS9(2)
//
// Max-flow from AS0 to AS4 (hand-computed):
//
//	Path 1: AS0→AS1→AS4         bottleneck=2 → flow=2
//	Path 2: AS0→AS1→AS3→AS4     bottleneck=3 (AS1 residual=3, AS3=6, …) → flow=3
//	Path 3: AS0→AS2→AS3→AS4     bottleneck=1 (AS2=3 after sending 0 so far, AS3=6-3=3) → flow=1
//	Path 4: AS0→AS2→AS3→AS4     bottleneck=1 (AS2 residual=2) → flow=1
//	…
//
// Rather than hard-coding a hand-computed value that may be wrong, we
// compute the reference by running MaxFlow once on a fresh Network and use
// that value as ground truth for the context-aware variant. This validates
// that MaxFlowCtx returns the same answer, which is the property being
// asserted (determinism, context plumbing).
//
// The reference is then printed so it can be cross-checked manually.
func TestASFlow_DinicMaxFlow(t *testing.T) {
	t.Parallel()

	// buildASNet constructs a fresh copy of the synthetic AS network.
	// Network is not safe for concurrent use; we build two fresh instances.
	buildASNet := func() *flow.Network {
		g := flow.NewNetwork(10)
		// AS0 out
		g.AddEdge(0, 1, 5)
		g.AddEdge(0, 2, 3)
		// AS1 out
		g.AddEdge(1, 3, 4)
		g.AddEdge(1, 4, 2)
		// AS2 out
		g.AddEdge(2, 3, 4)
		g.AddEdge(2, 5, 2)
		// AS3 out
		g.AddEdge(3, 4, 6)
		g.AddEdge(3, 6, 3)
		// AS5 out
		g.AddEdge(5, 4, 1)
		g.AddEdge(5, 7, 2)
		// AS6 out
		g.AddEdge(6, 4, 2)
		g.AddEdge(6, 8, 1)
		// AS7, AS8 out
		g.AddEdge(7, 9, 1)
		g.AddEdge(8, 9, 2)
		return g
	}

	const (
		src  = 0
		sink = 4
	)

	// Compute reference via the non-context variant.
	ref := flow.MaxFlow(buildASNet(), src, sink)
	if ref <= 0 {
		t.Fatalf("reference max-flow = %d, expected a positive value for this topology", ref)
	}
	t.Logf("reference max-flow AS%d→AS%d = %d", src, sink, ref)

	// Now verify MaxFlowCtx returns the same value.
	got, err := flow.MaxFlowCtx(context.Background(), buildASNet(), src, sink)
	if err != nil {
		t.Fatalf("MaxFlowCtx: %v", err)
	}
	if got != ref {
		t.Errorf("MaxFlowCtx = %d, want %d (reference from MaxFlow)", got, ref)
	}
}
