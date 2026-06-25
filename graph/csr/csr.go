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
	"errors"
	"fmt"
	"iter"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// CSR is an immutable compressed-sparse-row adjacency snapshot.
//
// CSR is safe for concurrent reads by any number of goroutines and
// requires no synchronisation: the backing arrays are not mutated
// after [BuildFromAdjList] returns.
type CSR[W any] struct {
	vertices []uint64       // offsets, length len(vertices); vertices[i] = start of node-i out-neighbours, vertices[len-1] = total edges
	edges    []graph.NodeID // length size; out-neighbours sorted by source
	weights  []W            // parallel to edges; nil when W is zero-size (struct{}) OR the source adjacency is weightless (see BuildFromAdjList)
	handles  []uint64       // parallel to edges; nil unless the source carries per-slot stable edge handles (see HandlesSlice)
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
	// edge count so the edges slice can be allocated exactly once, and
	// detects whether the source adjacency carries per-slot stable edge
	// handles so the handle column is allocated only when needed (lazy /
	// opt-in, mirroring the hasWeights gate below). A non-multigraph
	// graph that never called AddEdgeH carries no handles, so its CSR
	// never pays for the extra column and the read path falls back to
	// its prior positional behaviour.
	var total uint64
	var anyHandles bool
	for id := uint64(0); id < maxID; id++ {
		nb, _, h := adj.LoadEntryH(graph.NodeID(id))
		count := uint64(len(nb))
		vertices[id] = total
		total += count
		if h != nil {
			anyHandles = true
		}
	}
	vertices[maxID] = total

	edges := make([]graph.NodeID, total)
	var weights []W
	// Allocate the weights array only when the source genuinely carries
	// weights: the weight type W must carry a payload (hasWeights[W](), a
	// compile-time gate that excludes struct{}) AND the source adjacency must
	// not be in weightless mode (adj.Weightless(), a runtime gate the caller
	// opts into to drop the per-edge weight column on an unweighted property
	// graph). The two conditions are independent — a non-empty W can be paired
	// with a deliberately weightless graph — so both must hold. A weightless
	// source therefore yields a nil-weights CSR, which NeighboursByID renders
	// as the zero W and the snapshot writer persists with hasWeights=0.
	if hasWeights[W]() && !adj.Weightless() {
		weights = make([]W, total)
	}
	var handles []uint64
	if anyHandles {
		handles = make([]uint64, total)
	}

	// Pass 2: write out-edges. Two passes avoid the O(V) cost of a
	// follow-up shift to compute offsets from edge writes.
	for id := uint64(0); id < maxID; id++ {
		nb, ws, h := adj.LoadEntryH(graph.NodeID(id))
		if len(nb) == 0 {
			continue
		}
		start := vertices[id]
		copy(edges[start:], nb)
		if weights != nil {
			copy(weights[start:], ws)
		}
		if handles != nil {
			// h may be nil for a source whose edges predate the first
			// AddEdgeH on this graph; copy leaves those slots at 0 (the
			// "no handle" sentinel), keeping the column slot-aligned.
			copy(handles[start:], h)
		}
	}

	return &CSR[W]{
		vertices: vertices,
		edges:    edges,
		weights:  weights,
		handles:  handles,
		order:    uint64(adj.Order()),
		size:     total,
	}
}

// FromArrays assembles a CSR directly from caller-supplied, already
// final-sized adjacency arrays, bypassing [BuildFromAdjList] and its
// source [adjlist.AdjList]. It is the building block for high-throughput
// loaders (see store/bulk) that compute the offsets, flat edge array, and
// parallel weights in a single counting-sort pass and would otherwise pay
// for an intermediate mutable adjacency list.
//
// The arguments map one-to-one onto the fields [BuildFromAdjList]
// produces, and the caller is responsible for supplying values that are
// already consistent with that builder's output:
//
//   - vertices is the length V+1 offsets array; vertices[id] is the start
//     of node id's out-neighbours and vertices[len-1] is the total edge
//     count. For an empty graph it is exactly []uint64{0}.
//   - edges is the flat out-neighbour array, length vertices[len-1],
//     grouped by source in ascending NodeID order; within a source the
//     order is the caller's (the bulk loader preserves input order).
//   - weights is parallel to edges, or nil for an unweighted/weightless
//     snapshot (rendered as the zero W by [CSR.NeighboursByID]).
//   - order is the number of distinct nodes; size is the number of edges
//     (it must equal vertices[len-1]).
//
// FromArrays does not copy: it retains the supplied slices, which must
// therefore be treated as immutable from the call onward, exactly like a
// CSR returned by [BuildFromAdjList]. The handle column is always nil;
// loaders that need stable per-slot edge handles must use the adjacency
// path. The function performs no validation beyond what the type system
// enforces — it is the zero-copy bulk-load fast path and intentionally
// carries no O(E) scan. Supplying inconsistent arrays yields a malformed
// snapshot whose later traversal panics with a raw out-of-range index.
// A caller that does not fully trust its inputs should call [CSR.Validate]
// once after construction to obtain a typed [ErrMalformedCSR] at the
// boundary instead.
func FromArrays[W any](vertices []uint64, edges []graph.NodeID, weights []W, order, size uint64) *CSR[W] {
	return &CSR[W]{
		vertices: vertices,
		edges:    edges,
		weights:  weights,
		order:    order,
		size:     size,
	}
}

// ErrMalformedCSR is returned (wrapped) by [CSR.Validate] when the snapshot's
// backing arrays are internally inconsistent — for example an out-of-range
// destination NodeID or a non-monotonic offsets array. It signals a caller
// contract violation at the [FromArrays] boundary, not a runtime fault.
var ErrMalformedCSR = errors.New("csr: malformed snapshot")

// Validate checks that the snapshot's backing arrays are internally
// consistent and returns a wrapped [ErrMalformedCSR] describing the first
// violation, or nil when the snapshot is well-formed. It is the opt-in
// boundary check for callers of [FromArrays] that pass untrusted arrays;
// snapshots produced by [BuildFromAdjList] / [BuildReverse] are always
// well-formed and need not be validated.
//
// Validate is O(order + size). It does not allocate and never panics.
func (c *CSR[W]) Validate() error {
	if len(c.vertices) == 0 {
		return fmt.Errorf("%w: vertices offsets array is empty (must be at least []uint64{0})", ErrMalformedCSR)
	}
	last := len(c.vertices) - 1
	if uint64(len(c.edges)) != c.vertices[last] {
		return fmt.Errorf("%w: vertices[%d]=%d does not equal len(edges)=%d", ErrMalformedCSR, last, c.vertices[last], len(c.edges))
	}
	for i := 1; i < len(c.vertices); i++ {
		if c.vertices[i] < c.vertices[i-1] {
			return fmt.Errorf("%w: offsets not monotonic non-decreasing at vertices[%d]=%d < vertices[%d]=%d", ErrMalformedCSR, i, c.vertices[i], i-1, c.vertices[i-1])
		}
	}
	if c.weights != nil && len(c.weights) != len(c.edges) {
		return fmt.Errorf("%w: len(weights)=%d does not equal len(edges)=%d", ErrMalformedCSR, len(c.weights), len(c.edges))
	}
	maxNode := uint64(last) // valid destination NodeIDs are in [0, last)
	for k, dst := range c.edges {
		if uint64(dst) >= maxNode {
			return fmt.Errorf("%w: edges[%d]=%d is out of range for a snapshot of %d nodes", ErrMalformedCSR, k, dst, maxNode)
		}
	}
	return nil
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
// backed by a slice owned by the CSR.
//
// Allocation contract: the returned iter.Seq2 is allocation-free
// when used as a direct range expression at the call site
// ("for x, y := range g.NeighboursByID(src) { }"); the Go compiler
// inlines the closure in that case. Storing the iterator in a
// variable or passing it across function boundaries triggers
// closure heap-escape and one allocation per call. Hot paths in
// search/ deliberately bypass this method and read VerticesSlice()
// / EdgesSlice() directly to keep the inner loop allocation-free
// regardless of compiler decisions.
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

// HandlesSlice returns the underlying stable-edge-handle array, or nil
// when the source graph carried no per-slot handles (a simple graph that
// never used [adjlist.AdjList.AddEdgeH]). When non-nil the slice is the
// same length as [CSR.EdgesSlice] and aligns slot-for-slot with it:
// handles[pos] is the stable handle of the edge stored at edges[pos]. The
// slice must be treated as immutable.
//
// A nil return is the read path's signal to fall back to its prior
// positional per-instance inference, so callers must nil-check before
// indexing.
func (c *CSR[W]) HandlesSlice() []uint64 { return c.handles }

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

// BuildReverse returns a fresh CSR representing the same vertex set
// as c but with every edge (u, v) replaced by its reverse (v, u).
// Weights are carried over unchanged.
//
// The reverse CSR is the canonical adjacency for in-edge enumeration:
// it pairs with the forward CSR to support algorithms that require
// both directions (bidirectional Dijkstra, weakly-connected
// components, semi-external in-degree queries). On an undirected
// graph (one whose CSR is already symmetric) the returned CSR is
// structurally identical to c.
//
// Complexity: O(V + E) time, O(V + E) memory. The returned CSR is
// independent of c; mutating its slices does not affect c (per the
// immutable-snapshot contract callers must in any case respect).
func (c *CSR[W]) BuildReverse() *CSR[W] {
	maxID := uint64(c.MaxNodeID())
	if maxID == 0 {
		return &CSR[W]{vertices: []uint64{0}}
	}
	// Pass 1: count incoming edges per destination.
	inDeg := make([]uint64, maxID+1)
	for u := uint64(0); u+1 < uint64(len(c.vertices)); u++ {
		for k := c.vertices[u]; k < c.vertices[u+1]; k++ {
			inDeg[uint64(c.edges[k])+1]++
		}
	}
	// Prefix sum -> reversed-vertices offsets.
	revVerts := make([]uint64, maxID+1)
	for i := uint64(1); i <= maxID; i++ {
		revVerts[i] = revVerts[i-1] + inDeg[i]
	}
	totalEdges := revVerts[maxID]
	revEdges := make([]graph.NodeID, totalEdges)
	var revWeights []W
	if c.weights != nil {
		revWeights = make([]W, totalEdges)
	}
	var revHandles []uint64
	if c.handles != nil {
		revHandles = make([]uint64, totalEdges)
	}
	// Pass 2: scatter edges into their reversed slots. The stable handle
	// travels with its edge: the reverse slot for edge (u, v) carries the
	// same handle as the forward slot, so a logical edge keeps one
	// identity across both adjacency directions.
	cursor := make([]uint64, maxID)
	for u := uint64(0); u+1 < uint64(len(c.vertices)); u++ {
		for k := c.vertices[u]; k < c.vertices[u+1]; k++ {
			v := uint64(c.edges[k])
			pos := revVerts[v] + cursor[v]
			revEdges[pos] = graph.NodeID(u)
			if revWeights != nil {
				revWeights[pos] = c.weights[k]
			}
			if revHandles != nil {
				revHandles[pos] = c.handles[k]
			}
			cursor[v]++
		}
	}
	return &CSR[W]{
		vertices: revVerts,
		edges:    revEdges,
		weights:  revWeights,
		handles:  revHandles,
		order:    c.order,
		size:     c.size,
	}
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
