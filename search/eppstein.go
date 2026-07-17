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
// 2026-05-20 renamed it to reflect what the code actually does. Like the
// bare [KShortestPathsLoopless] it forwards to, it is unbounded and
// worst-case exponential in V, so new code should use the bounded,
// cancellable [KShortestPathsLooplessCtxWithOpts] or the polynomial
// [YenKShortest] instead.
//
// Deprecated: use [KShortestPathsLooplessCtxWithOpts] (bounded and
// cancellable) or [YenKShortest] (polynomial, node-simple).
func EppsteinKShortest[W Weight](c *csr.CSR[W], src, dst graph.NodeID, k int) []YenPath[W] {
	defer metrics.Time("search.EppsteinKShortest").Stop()
	// This deprecated backwards-compatibility alias intentionally forwards
	// verbatim to the (now also deprecated) bare entry it renamed.
	return KShortestPathsLoopless(c, src, dst, k) //nolint:staticcheck // deliberate forward to the deprecated bare entry this alias preserves
}

// EppsteinKShortestCtx is the deprecated former name of
// [KShortestPathsLooplessCtx].
//
// Deprecated: use [KShortestPathsLooplessCtx].
func EppsteinKShortestCtx[W Weight](ctx context.Context, c *csr.CSR[W], src, dst graph.NodeID, k int) ([]YenPath[W], error) {
	defer metrics.Time("search.EppsteinKShortestCtx").Stop()
	return KShortestPathsLooplessCtx(ctx, c, src, dst, k)
}
