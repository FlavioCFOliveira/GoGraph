// Package csr provides an immutable Compressed Sparse Row (CSR) view
// of a graph for read-mostly analytical workloads.
//
// CSR stores adjacency as two parallel arrays: vertices, a length V+1
// offsets array such that the out-neighbours of node id occupy the
// half-open range edges[vertices[id]:vertices[id+1]]; and edges
// itself, a flat array of NodeIDs sorted by source. Weighted graphs
// additionally carry a parallel weights array of the same length as
// edges.
//
// The layout is the de-facto standard for high-performance graph
// analytics (Mehlhorn-Sanders, GraphBLAS, GAP, Gunrock). Because the
// arrays are contiguous and source-sorted, full-graph scans achieve
// peak memory bandwidth; because the structure is immutable, reads
// are completely lock-free and trivially safe under any level of
// concurrency.
package csr

import (
	"iter"

	"gograph/graph"
	"gograph/graph/adjlist"
)

// CSR is an immutable compressed-sparse-row adjacency snapshot.
//
// CSR is safe for concurrent reads by any number of goroutines and
// requires no synchronisation: the backing arrays are not mutated
// after [BuildFromAdjList] returns.
type CSR[W any] struct {
	vertices []uint64       // offsets, length len(vertices); vertices[i] = start of node-i out-neighbours, vertices[len-1] = total edges
	edges    []graph.NodeID // length size; out-neighbours sorted by source
	weights  []W            // parallel to edges; nil when the source graph carries no weight payload of interest
	order    uint64         // number of distinct nodes in the snapshot
	size     uint64         // number of edges in the snapshot
}

// BuildFromAdjList constructs an immutable CSR snapshot of the
// adjacency stored in adj. The build is consistent against any single
// quiescent state of adj; callers responsible for ingestion typically
// invoke this once their writers have completed.
//
// Complexity is O(V + E) work plus O(V + E) memory for the resulting
// arrays. The function performs no concurrent fan-out and never
// blocks the caller on adjacency mutations.
func BuildFromAdjList[N comparable, W any](adj *adjlist.AdjList[N, W]) *CSR[W] {
	maxID := uint64(adj.MaxNodeID())
	if maxID == 0 {
		return &CSR[W]{
			vertices: []uint64{0},
		}
	}

	vertices := make([]uint64, maxID+1)

	// Pass 1: count out-edges per source. Also accumulates total
	// edge count so the edges slice can be allocated exactly once.
	var total uint64
	for id := uint64(0); id < maxID; id++ {
		nb, _ := adj.LoadEntry(graph.NodeID(id))
		count := uint64(len(nb))
		vertices[id] = total
		total += count
	}
	vertices[maxID] = total

	edges := make([]graph.NodeID, total)
	var weights []W
	if hasWeights[W]() {
		weights = make([]W, total)
	}

	// Pass 2: write out-edges. Two passes avoid the O(V) cost of a
	// follow-up shift to compute offsets from edge writes.
	for id := uint64(0); id < maxID; id++ {
		nb, ws := adj.LoadEntry(graph.NodeID(id))
		if len(nb) == 0 {
			continue
		}
		start := vertices[id]
		copy(edges[start:], nb)
		if weights != nil {
			copy(weights[start:], ws)
		}
	}

	return &CSR[W]{
		vertices: vertices,
		edges:    edges,
		weights:  weights,
		order:    uint64(adj.Order()),
		size:     total,
	}
}

// hasWeights returns true unless W is the empty struct, in which case
// the weights array carries no information and can be omitted to
// save memory and allocation time.
func hasWeights[W any]() bool {
	var zero W
	if _, ok := any(zero).(struct{}); ok {
		return false
	}
	return true
}

// Order returns the number of distinct nodes in the snapshot.
func (c *CSR[W]) Order() uint64 { return c.order }

// Size returns the number of edges in the snapshot.
func (c *CSR[W]) Size() uint64 { return c.size }

// MaxNodeID returns the smallest NodeID strictly greater than every
// NodeID used as a source in the snapshot. The vertices offsets array
// has length MaxNodeID()+1.
func (c *CSR[W]) MaxNodeID() graph.NodeID {
	if len(c.vertices) == 0 {
		return 0
	}
	return graph.NodeID(len(c.vertices) - 1)
}

// NeighboursByID returns an iterator over the out-neighbours of src
// and the weight (if any) of each connecting edge. The iterator is
// backed by a slice owned by the CSR and performs no allocation.
//
// If src is outside the snapshot's NodeID range the iterator yields
// no values. The zero value of W is yielded for unweighted snapshots.
func (c *CSR[W]) NeighboursByID(src graph.NodeID) iter.Seq2[graph.NodeID, W] {
	return func(yield func(graph.NodeID, W) bool) {
		id := uint64(src)
		if id+1 >= uint64(len(c.vertices)) {
			return
		}
		start := c.vertices[id]
		end := c.vertices[id+1]
		var zero W
		for i := start; i < end; i++ {
			w := zero
			if c.weights != nil {
				w = c.weights[i]
			}
			if !yield(c.edges[i], w) {
				return
			}
		}
	}
}

// EdgesSlice returns the underlying edges array. The slice must be
// treated as immutable; callers that mutate it break the snapshot's
// contract and any concurrent readers.
func (c *CSR[W]) EdgesSlice() []graph.NodeID { return c.edges }

// VerticesSlice returns the underlying offsets array. The slice must
// be treated as immutable.
func (c *CSR[W]) VerticesSlice() []uint64 { return c.vertices }

// WeightsSlice returns the underlying weights array, or nil if the
// snapshot is unweighted. The slice must be treated as immutable.
func (c *CSR[W]) WeightsSlice() []W { return c.weights }

// LiveMask returns a NodeID-indexed bitmap of length MaxNodeID() where
// mask[i] is true iff NodeID i participates in at least one edge as
// source or destination.
//
// On graphs constructed via a sharded Mapper, the NodeID space is
// sparse: MaxNodeID() rounds up to multiples driven by the shard
// count, so many indices are ghost slots with no incident edge.
// Algorithms that iterate the full [0, MaxNodeID()) range and treat
// every slot as a real vertex must filter through LiveMask to avoid
// O(MaxNodeID()) blow-up on small graphs.
//
// Complexity: O(V + E). The returned slice is freshly allocated; the
// CSR's own state is not retained or cached.
func (c *CSR[W]) LiveMask() []bool {
	maxID := uint64(c.MaxNodeID())
	if maxID == 0 {
		return nil
	}
	mask := make([]bool, maxID)
	for from := uint64(0); from < maxID; from++ {
		start := c.vertices[from]
		end := c.vertices[from+1]
		if end > start {
			mask[from] = true
			for k := start; k < end; k++ {
				mask[uint64(c.edges[k])] = true
			}
		}
	}
	return mask
}

// LiveNodes returns the sorted slice of NodeIDs with at least one
// incident edge. The companion to [CSR.LiveMask] when callers need a
// compact enumeration rather than a bitmap.
//
// Complexity: O(V + E). The returned slice is freshly allocated.
func (c *CSR[W]) LiveNodes() []graph.NodeID {
	mask := c.LiveMask()
	if len(mask) == 0 {
		return nil
	}
	out := make([]graph.NodeID, 0, len(mask))
	for i, ok := range mask {
		if ok {
			out = append(out, graph.NodeID(i))
		}
	}
	return out
}

// LiveCount returns the number of NodeIDs with at least one incident
// edge. Equivalent to len(c.LiveNodes()) but cheaper when the caller
// only needs the cardinality.
//
// Complexity: O(V + E).
func (c *CSR[W]) LiveCount() int {
	mask := c.LiveMask()
	var n int
	for _, ok := range mask {
		if ok {
			n++
		}
	}
	return n
}

// IsSymmetric reports whether the CSR is symmetric — that is, whether
// every directed edge (u, v) has a matching reverse edge (v, u). A
// symmetric CSR is the canonical representation of an undirected
// graph built via [adjlist.AdjList] with Directed: false.
//
// Algorithms that conceptually operate on undirected graphs ([BiBFS],
// connected components, undirected Eulerian circuits) use this check
// to reject directed input early with a typed error rather than
// returning silently-wrong results.
//
// Complexity: O(V + E) time, O(E) space (hash set of edge pairs).
func (c *CSR[W]) IsSymmetric() bool {
	verts := c.vertices
	edges := c.edges
	set := make(map[[2]graph.NodeID]struct{}, len(edges))
	for u := uint64(0); u+1 < uint64(len(verts)); u++ {
		for k := verts[u]; k < verts[u+1]; k++ {
			v := edges[k]
			set[[2]graph.NodeID{graph.NodeID(u), v}] = struct{}{}
		}
	}
	for pair := range set {
		if _, ok := set[[2]graph.NodeID{pair[1], pair[0]}]; !ok {
			return false
		}
	}
	return true
}
