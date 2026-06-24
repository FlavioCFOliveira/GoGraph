package sim

import (
	"fmt"
	"slices"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// undirectedAdj builds the dense-id undirected adjacency from the undirected
// edge set; both k-core and biconnected-components are undirected notions.
func undirectedAdj(n int, edges []mstEdge) [][]int {
	adj := make([][]int, n)
	for _, e := range edges {
		adj[e.u] = append(adj[e.u], e.v)
		adj[e.v] = append(adj[e.v], e.u)
	}
	return adj
}

// kcoreViolations cross-checks search.KCore coreness against a definition-based
// reference: the coreness of v is the largest k for which v survives in the
// k-core (the maximal subgraph in which every vertex has degree >= k). Computing
// it by the definition (rather than by the same peeling the library uses) makes
// the reference independent. Exact integer comparison.
func kcoreViolations(tick int64, g *nameGraph) []Violation {
	n := len(g.names)
	if n == 0 || n > maxDenseCheckNodes {
		return nil
	}
	edges := undirectedEdges(g)
	c := undirectedWeightedCSR(g, edges)
	adj := undirectedAdj(n, edges)

	got := search.KCore(c)
	want := naiveCoreness(n, adj)
	for v := 0; v < n; v++ {
		if v < len(got) && got[v] != want[v] {
			return []Violation{{
				Kind: ViolationSearchDivergence, Tick: tick, Op: "search:KCore",
				Message: fmt.Sprintf("coreness of %q got %d want %d", g.names[v], got[v], want[v]),
			}}
		}
	}
	return nil
}

// naiveCoreness computes the coreness of every node directly from the k-core
// definition: for each k, repeatedly delete vertices whose current degree is
// below k until none remain; every surviving vertex has coreness at least k.
func naiveCoreness(n int, adj [][]int) []int {
	core := make([]int, n)
	maxDeg := 0
	for v := range adj {
		if len(adj[v]) > maxDeg {
			maxDeg = len(adj[v])
		}
	}
	for k := 1; k <= maxDeg; k++ {
		inCore := make([]bool, n)
		deg := make([]int, n)
		for v := 0; v < n; v++ {
			inCore[v] = true
			deg[v] = len(adj[v])
		}
		for changed := true; changed; {
			changed = false
			for v := 0; v < n; v++ {
				if inCore[v] && deg[v] < k {
					inCore[v] = false
					changed = true
					for _, w := range adj[v] {
						if inCore[w] {
							deg[w]--
						}
					}
				}
			}
		}
		for v := 0; v < n; v++ {
			if inCore[v] {
				core[v] = k
			}
		}
	}
	return core
}

// bccViolations cross-checks the articulation points and bridges reported by
// search.HopcroftTarjanBCC against definition-based references: a vertex is an
// articulation point iff removing it increases the number of connected
// components; an edge is a bridge iff removing it disconnects its endpoints.
// The biconnected-component edge partition itself is not compared (its labelling
// is not unique); the articulation-point and bridge sets capture the same
// structure and have clean independent references.
func bccViolations(tick int64, g *nameGraph) []Violation {
	n := len(g.names)
	if n == 0 || n > maxDenseCheckNodes {
		return nil
	}
	edges := undirectedEdges(g)
	c := undirectedWeightedCSR(g, edges)
	adj := undirectedAdj(n, edges)
	incident := g.incidentMask()

	res := search.HopcroftTarjanBCC(c)
	var vs []Violation

	if got, want := sortNodeIDs(res.Articulation), naiveArticulation(n, adj, incident); !slices.Equal(got, want) {
		vs = append(vs, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:BCC",
			Message: fmt.Sprintf("articulation points got %v want %v", got, want),
		})
	}
	if got, want := canonicalBridges(res.Bridges), naiveBridges(n, adj); !slices.Equal(got, want) {
		vs = append(vs, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:BCC",
			Message: fmt.Sprintf("bridges got %v want %v", got, want),
		})
	}
	return vs
}

// naiveArticulation returns the sorted dense ids of the articulation points, by
// removing each incident vertex and checking whether the component count rises.
func naiveArticulation(n int, adj [][]int, incident []bool) []int {
	base := countComponents(n, adj, incident, -1)
	var arts []int
	for v := 0; v < n; v++ {
		if !incident[v] {
			continue
		}
		if countComponents(n, adj, incident, v) > base {
			arts = append(arts, v)
		}
	}
	sort.Ints(arts)
	return arts
}

// countComponents counts the connected components among the incident nodes,
// optionally excluding one vertex (exclude == -1 for none).
func countComponents(n int, adj [][]int, incident []bool, exclude int) int {
	seen := make([]bool, n)
	comps := 0
	for s := 0; s < n; s++ {
		if !incident[s] || s == exclude || seen[s] {
			continue
		}
		comps++
		stack := []int{s}
		seen[s] = true
		for len(stack) > 0 {
			u := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, w := range adj[u] {
				if w == exclude || !incident[w] || seen[w] {
					continue
				}
				seen[w] = true
				stack = append(stack, w)
			}
		}
	}
	return comps
}

// naiveBridges returns the bridges as canonical "u-v" (u<v) keys, by removing
// each undirected edge and testing whether its endpoints remain connected.
func naiveBridges(n int, adj [][]int) []string {
	var bridges []string
	for u := 0; u < n; u++ {
		for _, v := range adj[u] {
			if u >= v {
				continue // visit each undirected edge once
			}
			if !reachableWithoutEdge(n, adj, u, v) {
				bridges = append(bridges, fmt.Sprintf("%d-%d", u, v))
			}
		}
	}
	sort.Strings(bridges)
	return bridges
}

// reachableWithoutEdge reports whether dst is reachable from src with the single
// undirected edge {src,dst} removed.
func reachableWithoutEdge(n int, adj [][]int, src, dst int) bool {
	seen := make([]bool, n)
	seen[src] = true
	stack := []int{src}
	for len(stack) > 0 {
		u := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, w := range adj[u] {
			if (u == src && w == dst) || (u == dst && w == src) {
				continue // the edge under test
			}
			if !seen[w] {
				seen[w] = true
				stack = append(stack, w)
			}
		}
	}
	return seen[dst]
}

// sortNodeIDs converts a NodeID slice to a sorted []int for comparison.
func sortNodeIDs(ids []graph.NodeID) []int {
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = int(id)
	}
	sort.Ints(out)
	return out
}

// canonicalBridges renders a bridge list as sorted canonical "u-v" (u<v) keys.
func canonicalBridges(bridges [][2]graph.NodeID) []string {
	out := make([]string, 0, len(bridges))
	for _, b := range bridges {
		a, c := int(b[0]), int(b[1])
		if a > c {
			a, c = c, a
		}
		out = append(out, fmt.Sprintf("%d-%d", a, c))
	}
	sort.Strings(out)
	return out
}
