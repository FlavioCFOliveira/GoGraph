package search

import (
	"gograph/graph"
	"gograph/graph/csr"
)

// BCCResult bundles every output of [HopcroftTarjanBCC]: the
// biconnected components (each component a slice of NodeIDs),
// the bridges (each as an (a, b) pair), and the articulation
// points.
type BCCResult struct {
	Components   [][]graph.NodeID
	Bridges      [][2]graph.NodeID
	Articulation []graph.NodeID
}

// HopcroftTarjanBCC runs Hopcroft-Tarjan's combined biconnected-
// components / bridges / articulation-points algorithm on an
// undirected graph captured by c. Complexity O(V + E).
//
// The implementation uses an explicit DFS stack (no recursion) so
// it survives deep graphs.
//
// loop maintaining DFS state + articulation/bridge detection +
// edge stack management.
//
//nolint:gocyclo // canonical structure of the algorithm: a single
func HopcroftTarjanBCC[W any](c *csr.CSR[W]) BCCResult {
	maxID := int(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

	const unvisited = -1
	disc := make([]int, maxID)
	low := make([]int, maxID)
	parent := make([]int, maxID)
	isArtic := make([]bool, maxID)
	for i := range disc {
		disc[i] = unvisited
		parent[i] = unvisited
	}
	type frame struct {
		v    int
		next uint64
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
		disc[start] = timer
		low[start] = timer
		timer++
		stack = append(stack, frame{v: start, next: verts[start]})
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
						// Pop edges off edgeStack until (p, v).
						var comp []graph.NodeID
						for len(edgeStack) > 0 {
							e := edgeStack[len(edgeStack)-1]
							edgeStack = edgeStack[:len(edgeStack)-1]
							comp = append(comp, e[0], e[1])
							if (uint64(e[0]) == uint64(p) && uint64(e[1]) == uint64(v)) ||
								(uint64(e[1]) == uint64(p) && uint64(e[0]) == uint64(v)) {
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
			w := int(edges[top.next])
			top.next++
			if w == parent[top.v] {
				continue
			}
			if disc[w] == unvisited {
				parent[w] = top.v
				if top.v == start {
					rootChildren++
				}
				disc[w] = timer
				low[w] = timer
				timer++
				edgeStack = append(edgeStack, [2]graph.NodeID{graph.NodeID(top.v), graph.NodeID(w)})
				stack = append(stack, frame{v: w, next: verts[w]})
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
	return BCCResult{Components: components, Bridges: bridges, Articulation: artic}
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
