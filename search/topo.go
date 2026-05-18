package search

import (
	"errors"

	"gograph/graph"
	"gograph/graph/csr"
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

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
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
	if emitted != totalLive {
		return nil, ErrCycle
	}
	return out, nil
}
