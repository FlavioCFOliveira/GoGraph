package search

import (
	"context"
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrCycle is returned by algorithms that require a directed acyclic
// graph when the input contains a cycle.
var ErrCycle = errors.New("search: cycle detected in directed graph")

// TopologicalSort returns a topological ordering of the directed
// acyclic graph captured by c using Kahn's algorithm. Vertices with
// no incoming edges are repeatedly emitted; removing them exposes
// new sources. If any vertex remains unemitted after the algorithm
// completes, the input has a cycle and [ErrCycle] is returned.
//
// The returned ordering covers every NodeID for which the CSR has a
// non-empty out-edge range or at least one incoming edge. Sparse
// gaps in the NodeID space (NodeIDs that were never assigned by the
// Mapper) are omitted from the output.
func TopologicalSort[W any](c *csr.CSR[W]) ([]graph.NodeID, error) {
	defer metrics.Time("search.TopologicalSort").Stop()
	res, err := TopologicalSortCtx(context.Background(), c)
	if err != nil {
		metrics.IncCounter("search.TopologicalSort.errors", 1)
	}
	return res, err
}

// TopologicalSortCtx is the context-aware variant of [TopologicalSort].
// ctx.Err() is checked on entry to the emit loop, every 4096 emits
// thereafter, and once more before the cycle verdict is returned; on
// cancellation returns (nil, the raw ctx.Err()). A cancelled context
// outranks [ErrCycle].
func TopologicalSortCtx[W any](ctx context.Context, c *csr.CSR[W]) ([]graph.NodeID, error) {
	defer metrics.Time("search.TopologicalSortCtx").Stop()
	maxID := uint64(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	indegree := make([]uint64, maxID)
	live := make([]bool, maxID)

	for from := uint64(0); from < maxID; from++ {
		start := verts[from]
		end := verts[from+1]
		if end > start {
			live[from] = true
		}
		for k := start; k < end; k++ {
			indegree[uint64(edges[k])]++
			live[uint64(edges[k])] = true
		}
	}

	queue := make([]graph.NodeID, 0, maxID)
	for id := uint64(0); id < maxID; id++ {
		if live[id] && indegree[id] == 0 {
			queue = append(queue, graph.NodeID(id))
		}
	}

	out := make([]graph.NodeID, 0, maxID)
	emitted := 0
	totalLive := 0
	for _, v := range live {
		if v {
			totalLive++
		}
	}

	for qh := 0; qh < len(queue); qh++ {
		if emitted&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				metrics.IncCounter("search.TopologicalSortCtx.errors", 1)
				return nil, err
			}
		}
		n := queue[qh]
		out = append(out, n)
		emitted++
		start := verts[uint64(n)]
		end := verts[uint64(n)+1]
		for k := start; k < end; k++ {
			nb := uint64(edges[k])
			indegree[nb]--
			if indegree[nb] == 0 {
				queue = append(queue, graph.NodeID(nb))
			}
		}
	}
	// Poll once more before the cycle verdict. On a fully cyclic graph every
	// live vertex has indegree >= 1, so the Kahn queue starts EMPTY, the
	// polled loop above never runs, and ErrCycle was returned in preference
	// to the context error at every graph size -- the entry point could not
	// report cancellation on that shape at all (rmp #2593). The same poll
	// also covers the empty-CSR shape, where the loop is likewise never
	// entered; cancellation now wins on every input shape. ErrCycle is
	// still returned whenever the context is live.
	if err := ctx.Err(); err != nil {
		metrics.IncCounter("search.TopologicalSortCtx.errors", 1)
		return nil, err
	}
	if emitted != totalLive {
		metrics.IncCounter("search.TopologicalSortCtx.errors", 1)
		return nil, ErrCycle
	}
	return out, nil
}
