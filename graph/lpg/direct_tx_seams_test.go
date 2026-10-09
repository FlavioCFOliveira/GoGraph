package lpg

// direct_tx_seams_test.go — the seams the rmp #2947 regression tests drive.
//
// They are gathered here, behind three helpers, so the tests in
// direct_tx_test.go name a WINDOW rather than a field. The windows are the ones
// the audit of the rejected point-check design probed: just before and just
// after a direct edge removal's adjacency write, and just after a direct node
// removal has decided it may proceed.

// setEdgeRemovalSeams runs before just before, and after just after, the
// adjacency write of the next direct edge removal. Either may be nil.
func setEdgeRemovalSeams(g *Graph[string, float64], before, after func()) {
	if before == nil && after == nil {
		g.edgeRemovalHookForTest = nil
		return
	}
	g.edgeRemovalHookForTest = func(afterAdjacency bool) {
		switch {
		case !afterAdjacency && before != nil:
			before()
		case afterAdjacency && after != nil:
			after()
		}
	}
}

// setNodeRemovalSeam runs fn once a direct node removal has taken every claim
// and before it changes anything. nil clears it.
func setNodeRemovalSeam(g *Graph[string, float64], fn func()) {
	g.nodeRemovalClaimedHookForTest = fn
}
