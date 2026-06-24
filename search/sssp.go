package search

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// SSSP is a reusable single-source shortest-path engine bound to one
// immutable CSR snapshot. It is the stateful counterpart to the one-shot
// [Dijkstra] for workloads that issue many queries against the same
// graph (all-pairs via repeated SSSP, multi-source fan-outs, k-shortest
// paths).
//
// The win over calling [Dijkstra] in a loop is that the O(E) weight
// validation (no negative weight; no NaN/+-Inf) runs exactly once, at
// construction, rather than on every query — the snapshot is immutable,
// so a column validated once stays valid (rmp #1516). Each [SSSP.From]
// then performs only the traversal, drawing its working buffers from the
// same per-W pool [Dijkstra] uses, so the steady state is allocation-free
// in the inner loop.
//
// Concurrency: SSSP holds no mutable state after construction; From
// acquires private pooled buffers per call and writes only its own
// result, so any number of goroutines may call From concurrently on a
// shared SSSP and on the shared immutable CSR.
type SSSP[W Weight] struct {
	c *csr.CSR[W]
}

// NewSSSP validates c's weight column once (returning [ErrNegativeWeight]
// or [ErrInvalidInput] exactly as [Dijkstra] would) and returns an engine
// that serves repeated queries without re-validating. A nil error
// guarantees every subsequent [SSSP.From] skips the O(E) weight scan.
func NewSSSP[W Weight](c *csr.CSR[W]) (*SSSP[W], error) {
	defer metrics.Time("search.NewSSSP").Stop()
	if err := validateDijkstraWeights(c.WeightsSlice()); err != nil {
		metrics.IncCounter("search.NewSSSP.errors", 1)
		return nil, err
	}
	return &SSSP[W]{c: c}, nil
}

// From computes single-source shortest paths from src using
// [context.Background]. See [SSSP.FromCtx] for the cancellable variant.
func (s *SSSP[W]) From(src graph.NodeID) (*Distances[W], error) {
	return s.FromCtx(context.Background(), src)
}

// FromCtx computes single-source shortest paths from src, honouring
// ctx cancellation (checked every 4096 heap pops; on cancellation
// returns (nil, wrapped ctx.Err())). The weight column was validated at
// construction, so FromCtx performs no per-query weight scan; the only
// error it can surface is the context error.
func (s *SSSP[W]) FromCtx(ctx context.Context, src graph.NodeID) (*Distances[W], error) {
	defer metrics.Time("search.SSSP.From").Stop()
	maxID := uint64(s.c.MaxNodeID())
	st := acquireDijkstra[W](maxID)
	defer releaseDijkstra(st)
	if err := dijkstraCore[W](ctx, s.c, src, st.dist[:maxID], st.parent[:maxID], st.found[:maxID], &st.heap); err != nil {
		metrics.IncCounter("search.SSSP.From.errors", 1)
		return nil, err
	}
	return newDistancesCopy(st, src, maxID), nil
}
