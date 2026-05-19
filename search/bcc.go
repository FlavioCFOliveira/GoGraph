package search

import (
	"context"

	"gograph/graph"
	"gograph/graph/csr"
)

// BCCResult bundles every output of [HopcroftTarjanBCC]: the
// biconnected components (each component a slice of NodeIDs),
// the bridges (each as an (a, b) pair), and the articulation
// points.
//
// Concurrency: BCCResult is a value type returned freshly by every
// call; safe to share across goroutines for read.
type BCCResult struct {
	Components   [][]graph.NodeID
	Bridges      [][2]graph.NodeID
	Articulation []graph.NodeID
}

// HopcroftTarjanBCC runs Hopcroft-Tarjan's combined biconnected-
// components / bridges / articulation-points algorithm on an
// undirected graph captured by c. Complexity O(V + E).
//
// Multigraph correctness: when an undirected graph has multiple
// parallel edges between the same vertex pair, every such edge
// participates in a 2-cycle biconnected component. The algorithm
// tracks the specific CSR edge index used to enter each vertex
// (parentEdgeIdx) so that only that one edge is skipped — every
// other parallel edge to the parent is correctly treated as a
// real back-edge.
//
// The implementation uses an explicit DFS stack (no recursion) so
// it survives deep graphs.
func HopcroftTarjanBCC[W any](c *csr.CSR[W]) BCCResult {
	out, _ := HopcroftTarjanBCCCtx(context.Background(), c)
	return out
}

// HopcroftTarjanBCCCtx is the context-aware variant of
// [HopcroftTarjanBCC]. ctx.Err() is checked at every outer-DFS root;
// on cancellation returns (zero BCCResult, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical Hopcroft-Tarjan: DFS frame stack + edge-stack + articulation + bridge detection
func HopcroftTarjanBCCCtx[W any](ctx context.Context, c *csr.CSR[W]) (BCCResult, error) {
	maxID := int(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	const unvisited = -1
	disc := make([]int, maxID)
	low := make([]int, maxID)
	isArtic := make([]bool, maxID)
	for i := range disc {
		disc[i] = unvisited
	}
	// parentEdgeIdx in each frame is the CSR edge index used to
	// arrive at v (i.e. the matching back-edge in v's CSR pointing
	// to v's DFS parent). -1 for the DFS root.
	type frame struct {
		v             int
		parentEdgeIdx int
		next          uint64
	}
	var stack []frame
	var edgeStack [][2]graph.NodeID
	var components [][]graph.NodeID
	var bridges [][2]graph.NodeID
	timer := 0

	for start := 0; start < maxID; start++ {
		if disc[start] != unvisited {
			continue
		}
		if verts[start+1] == verts[start] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return BCCResult{}, err
		}
		disc[start] = timer
		low[start] = timer
		timer++
		stack = append(stack, frame{v: start, parentEdgeIdx: -1, next: verts[start]})
		rootChildren := 0
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			if top.next >= verts[top.v+1] {
				v := top.v
				stack = stack[:len(stack)-1]
				if len(stack) > 0 {
					p := stack[len(stack)-1].v
					if low[v] < low[p] {
						low[p] = low[v]
					}
					if low[v] >= disc[p] {
						if disc[p] != 0 || rootChildren > 1 {
							isArtic[p] = true
						}
						// Pop edges off edgeStack until the tree edge (p, v).
						// Tree edges were pushed as {parent, child}; back
						// edges as {descendant, ancestor}. Matching only on
						// the tree-edge ordering ensures we pop every
						// back-edge in the same BCC instead of exiting
						// early on a back-edge whose endpoints happen to
						// match the (p, v) pair.
						var comp []graph.NodeID
						for len(edgeStack) > 0 {
							e := edgeStack[len(edgeStack)-1]
							edgeStack = edgeStack[:len(edgeStack)-1]
							comp = append(comp, e[0], e[1])
							if uint64(e[0]) == uint64(p) && uint64(e[1]) == uint64(v) {
								break
							}
						}
						if len(comp) > 0 {
							components = append(components, dedupe(comp))
						}
					}
					if low[v] > disc[p] {
						bridges = append(bridges, [2]graph.NodeID{graph.NodeID(p), graph.NodeID(v)})
					}
				}
				continue
			}
			e := top.next
			top.next++
			// Skip only the specific edge we used to descend into top.v.
			// In multigraphs, other parallel edges back to the parent
			// must still be processed as real back-edges to form the
			// 2-cycle BCC they belong to.
			if int(e) == top.parentEdgeIdx {
				continue
			}
			w := int(edges[e])
			if disc[w] == unvisited {
				if top.v == start {
					rootChildren++
				}
				disc[w] = timer
				low[w] = timer
				timer++
				edgeStack = append(edgeStack, [2]graph.NodeID{graph.NodeID(top.v), graph.NodeID(w)})
				// Locate the back-edge in w's CSR pointing to top.v.
				// In simple graphs there's exactly one; in multigraphs
				// any unmarked parallel edge works (linear scan).
				childParentEdgeIdx := -1
				for k := verts[w]; k < verts[w+1]; k++ {
					if int(edges[k]) == top.v {
						childParentEdgeIdx = int(k)
						break
					}
				}
				stack = append(stack, frame{v: w, parentEdgeIdx: childParentEdgeIdx, next: verts[w]})
			} else if disc[w] < disc[top.v] {
				if disc[w] < low[top.v] {
					low[top.v] = disc[w]
				}
				edgeStack = append(edgeStack, [2]graph.NodeID{graph.NodeID(top.v), graph.NodeID(w)})
			}
		}
		if rootChildren > 1 {
			isArtic[start] = true
		}
		// Drain remaining edges as the last component of this tree.
		if len(edgeStack) > 0 {
			var comp []graph.NodeID
			for _, e := range edgeStack {
				comp = append(comp, e[0], e[1])
			}
			components = append(components, dedupe(comp))
			edgeStack = edgeStack[:0]
		}
	}
	var artic []graph.NodeID
	for i, a := range isArtic {
		if a {
			artic = append(artic, graph.NodeID(i))
		}
	}
	return BCCResult{Components: components, Bridges: bridges, Articulation: artic}, nil
}

func dedupe(in []graph.NodeID) []graph.NodeID {
	seen := make(map[graph.NodeID]struct{}, len(in))
	var out []graph.NodeID
	for _, n := range in {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}
