package adjlist

import "testing"

// mustAddEdge wraps [AdjList.AddEdge] for tests that expect the call
// to succeed. It fails the test on any error rather than letting a
// would-be-silent failure mask further assertions.
func mustAddEdge[N comparable, W any](tb testing.TB, a *AdjList[N, W], src, dst N, w W) {
	tb.Helper()
	if err := a.AddEdge(src, dst, w); err != nil {
		tb.Fatalf("AddEdge(%v, %v): %v", src, dst, err)
	}
}

// mustAddNode wraps [AdjList.AddNode] for tests that expect the call
// to succeed.
func mustAddNode[N comparable, W any](tb testing.TB, a *AdjList[N, W], n N) {
	tb.Helper()
	if err := a.AddNode(n); err != nil {
		tb.Fatalf("AddNode(%v): %v", n, err)
	}
}
