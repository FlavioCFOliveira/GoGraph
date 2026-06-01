package search

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// EppsteinKShortest is the deprecated former name of
// [KShortestPathsLoopless]. The implementation shipped under this
// symbol is a best-first enumeration over the loopless-path tree, not
// the heap-of-heaps construction of Eppstein 1998; the audit on
// 2026-05-20 renamed it to reflect what the code actually does. The
// alias is kept for one major version so existing callers do not
// break; new code should use [KShortestPathsLoopless].
//
// Deprecated: use [KShortestPathsLoopless].
func EppsteinKShortest[W Weight](c *csr.CSR[W], src, dst graph.NodeID, k int) []YenPath[W] {
	defer metrics.Time("search.EppsteinKShortest")()
	return KShortestPathsLoopless(c, src, dst, k)
}

// EppsteinKShortestCtx is the deprecated former name of
// [KShortestPathsLooplessCtx].
//
// Deprecated: use [KShortestPathsLooplessCtx].
func EppsteinKShortestCtx[W Weight](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID, k int) ([]YenPath[W], error) {
	defer metrics.Time("search.EppsteinKShortestCtx")()
	return KShortestPathsLooplessCtx(ctx, c, src, dst, k)
}
