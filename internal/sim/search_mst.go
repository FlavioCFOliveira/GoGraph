package sim

import (
	"fmt"
	"slices"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// mstEdge is one undirected edge of the symmetric view of the live graph, with
// its symmetric weight; u < v always.
type mstEdge struct {
	u, v int
	w    float64
}

// symWeight is the symmetric (direction-independent) edge weight used for the
// undirected MST view: it orders the endpoint names so the same weight is seen
// for {a,b} regardless of which direction the live KNOWS edge ran.
func symWeight(a, b string) float64 {
	if a <= b {
		return edgeWeight(a, b)
	}
	return edgeWeight(b, a)
}

// undirectedEdges returns the de-duplicated undirected edge set of the graph
// (self-loops dropped), in a deterministic (u, v) order. The dedup map is used
// for membership only; the output order is fixed by the final sort.
func undirectedEdges(g *nameGraph) []mstEdge {
	seen := make(map[[2]int]struct{})
	var out []mstEdge
	for u := range g.out {
		for _, v := range g.out[u] {
			if u == v {
				continue
			}
			a, b := u, v
			if a > b {
				a, b = b, a
			}
			key := [2]int{a, b}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, mstEdge{u: a, v: b, w: symWeight(g.names[a], g.names[b])})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].u != out[j].u {
			return out[i].u < out[j].u
		}
		return out[i].v < out[j].v
	})
	return out
}

// undirectedWeightedCSR materialises the symmetric (both-directions) weighted CSR
// the MST algorithms consume, from the undirected edge set.
func undirectedWeightedCSR(g *nameGraph, edges []mstEdge) *csr.CSR[float64] {
	n := len(g.names)
	if n == 0 {
		return csr.FromArrays[float64]([]uint64{0}, nil, nil, 0, 0)
	}
	deg := make([]int, n)
	for _, e := range edges {
		deg[e.u]++
		deg[e.v]++
	}
	vertices := make([]uint64, n+1)
	var total uint64
	for i := 0; i < n; i++ {
		vertices[i] = total
		total += uint64(deg[i])
	}
	vertices[n] = total
	out := make([]graph.NodeID, total)
	weights := make([]float64, total)
	pos := make([]uint64, n)
	copy(pos, vertices[:n])
	for _, e := range edges {
		out[pos[e.u]] = graph.NodeID(e.v)
		weights[pos[e.u]] = e.w
		pos[e.u]++
		out[pos[e.v]] = graph.NodeID(e.u)
		weights[pos[e.v]] = e.w
		pos[e.v]++
	}
	return csr.FromArrays[float64](vertices, out, weights, uint64(n), total)
}

// mstUF is a small union-find used by the independent Kruskal reference and for
// undirected component identification.
type mstUF struct{ parent []int }

func newMSTUF(n int) *mstUF {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &mstUF{parent: p}
}

func (u *mstUF) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *mstUF) union(a, b int) bool {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return false
	}
	u.parent[ra] = rb
	return true
}

// naiveKruskalTotal is the independent minimum-spanning-forest weight over the
// given undirected edges, by sorting and union-find. The total weight of an MST
// is unique (even under tied edge weights), so it is a sound comparison invariant.
func naiveKruskalTotal(n int, edges []mstEdge) float64 {
	es := slices.Clone(edges)
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].w != es[j].w {
			return es[i].w < es[j].w
		}
		if es[i].u != es[j].u {
			return es[i].u < es[j].u
		}
		return es[i].v < es[j].v
	})
	uf := newMSTUF(n)
	var total float64
	for _, e := range es {
		if uf.union(e.u, e.v) {
			total += e.w
		}
	}
	return total
}

// mstViolations cross-checks the MST algorithms against an independent Kruskal
// reference, comparing TOTAL WEIGHT plus spanning-forest validity (never the
// edge set, which is not unique). Kruskal is checked globally; Prim is checked
// per source against the MST weight of that source's undirected component.
func mstViolations(tick int64, g *nameGraph) []Violation {
	n := len(g.names)
	if n == 0 {
		return nil
	}
	edges := undirectedEdges(g)
	c := undirectedWeightedCSR(g, edges)

	// Undirected components, for Prim's per-component comparison and the
	// spanning-forest edge-count validity.
	comp := newMSTUF(n)
	for _, e := range edges {
		comp.union(e.u, e.v)
	}
	incident := g.incidentMask()
	incCount := 0
	roots := make(map[int]struct{})
	for i := 0; i < n; i++ {
		if incident[i] {
			incCount++
			roots[comp.find(i)] = struct{}{}
		}
	}

	var vs []Violation

	// Kruskal: global minimum spanning forest.
	kEdges, kTotal, err := search.KruskalMST(c)
	if err != nil {
		vs = append(vs, searchDeviation(tick, "KruskalMST", err))
	} else {
		if want := naiveKruskalTotal(n, edges); kTotal != want {
			vs = append(vs, mstDiverge(tick, "KruskalMST",
				fmt.Sprintf("total weight got %v want %v", kTotal, want)))
		}
		if wantEdges := incCount - len(roots); len(kEdges) != wantEdges {
			vs = append(vs, mstDiverge(tick, "KruskalMST",
				fmt.Sprintf("spanning-forest edge count got %d want %d (incident=%d components=%d)",
					len(kEdges), wantEdges, incCount, len(roots))))
		}
	}

	// Prim: per-source spanning tree of the source's component.
	for _, src := range g.checkSources() {
		_, _, pTotal, err := search.PrimMST(c, graph.NodeID(src))
		if err != nil {
			vs = append(vs, searchDeviation(tick, "PrimMST", err))
			continue
		}
		want := naiveComponentMSTWeight(n, edges, comp, src)
		if pTotal != want {
			vs = append(vs, mstDiverge(tick, "PrimMST",
				fmt.Sprintf("component MST weight from %q got %v want %v", g.names[src], pTotal, want)))
		}
	}
	return vs
}

// naiveComponentMSTWeight is the independent MST weight of the undirected
// component containing src, used as the reference for PrimMST.
func naiveComponentMSTWeight(n int, edges []mstEdge, comp *mstUF, src int) float64 {
	root := comp.find(src)
	var within []mstEdge
	for _, e := range edges {
		if comp.find(e.u) == root {
			within = append(within, e)
		}
	}
	return naiveKruskalTotal(n, within)
}

// mstDiverge builds a single MST divergence violation.
func mstDiverge(tick int64, algo, msg string) Violation {
	return Violation{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo, Message: msg}
}
