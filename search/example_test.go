package search_test

import (
	"fmt"
	"sort"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

// buildDirected assembles a directed, unweighted CSR from (src, dst)
// integer pairs and returns it together with the mapper that resolves
// NodeIDs back to the user-facing int values.
func buildDirected(edges [][2]int) (*csr.CSR[struct{}], *graph.Mapper[int]) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for _, e := range edges {
		_ = a.AddEdge(e[0], e[1], struct{}{})
	}
	return csr.BuildFromAdjList(a), a.Mapper()
}

// buildWeighted assembles a CSR over int64-weighted edges and returns
// it together with the mapper. directed selects a directed graph; when
// false the AdjList mirrors every edge, yielding the symmetric CSR the
// undirected algorithms (Kruskal, Prim) expect.
func buildWeighted(directed bool, edges [][3]int) (*csr.CSR[int64], *graph.Mapper[int]) {
	a := adjlist.New[int, int64](adjlist.Config{Directed: directed})
	for _, e := range edges {
		_ = a.AddEdge(e[0], e[1], int64(e[2]))
	}
	return csr.BuildFromAdjList(a), a.Mapper()
}

// resolvePath maps a path expressed in NodeID space back to the
// user-facing int values, so output is stable regardless of the
// opaque NodeIDs the mapper assigns.
func resolvePath(m *graph.Mapper[int], path []graph.NodeID) []int {
	out := make([]int, len(path))
	for i, id := range path {
		out[i], _ = m.Resolve(id)
	}
	return out
}

// ExampleBFS traverses an unweighted chain breadth-first and reports
// the depth at which each node is discovered.
func ExampleBFS() {
	// Chain: 0 -> 1 -> 2 -> 3.
	c, m := buildDirected([][2]int{{0, 1}, {1, 2}, {2, 3}})
	src, _ := m.Lookup(0)

	depth := map[int]int{}
	search.BFS(c, src, func(node graph.NodeID, d int) bool {
		v, _ := m.Resolve(node)
		depth[v] = d
		return true // keep traversing
	})

	for v := 0; v < 4; v++ {
		fmt.Printf("node %d at depth %d\n", v, depth[v])
	}
	// Output:
	// node 0 at depth 0
	// node 1 at depth 1
	// node 2 at depth 2
	// node 3 at depth 3
}

// ExampleDFS traverses an unweighted chain depth-first; on a chain the
// visit order and per-node depth coincide with the natural sequence.
func ExampleDFS() {
	// Chain: 0 -> 1 -> 2 -> 3.
	c, m := buildDirected([][2]int{{0, 1}, {1, 2}, {2, 3}})
	src, _ := m.Lookup(0)

	search.DFS(c, src, func(node graph.NodeID, d int) bool {
		v, _ := m.Resolve(node)
		fmt.Printf("visit %d (depth %d)\n", v, d)
		return true // keep traversing
	})
	// Output:
	// visit 0 (depth 0)
	// visit 1 (depth 1)
	// visit 2 (depth 2)
	// visit 3 (depth 3)
}

// ExampleDijkstra computes single-source shortest-path distances on a
// positively weighted directed graph and queries the cost to each node.
func ExampleDijkstra() {
	// CLRS-style graph; shortest distances from 0 are 0,7,3,8,5.
	c, m := buildWeighted(true, [][3]int{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	src, _ := m.Lookup(0)

	d, err := search.Dijkstra(c, src)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for v := 0; v < 5; v++ {
		id, _ := m.Lookup(v)
		dist, _ := d.Distance(id)
		fmt.Printf("dist to %d = %d\n", v, dist)
	}
	// Output:
	// dist to 0 = 0
	// dist to 1 = 7
	// dist to 2 = 3
	// dist to 3 = 8
	// dist to 4 = 5
}

// ExampleAStar finds an optimal path on a weighted graph. With a
// zero heuristic (trivially admissible) A* degenerates to Dijkstra and
// still returns the least-cost path and its total cost.
func ExampleAStar() {
	c, m := buildWeighted(true, [][3]int{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8},
	})
	src, _ := m.Lookup(0)
	dst, _ := m.Lookup(3)

	// Zero heuristic: never overestimates the remaining cost.
	path, cost, err := search.AStar(c, src, dst, func(graph.NodeID) int64 { return 0 })
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("path %v cost %d\n", resolvePath(m, path), cost)
	// Output:
	// path [0 2 1 3] cost 8
}

// ExampleBellmanFord computes shortest paths on a graph that contains a
// negative-weight edge but no negative cycle — the case Dijkstra cannot
// handle.
func ExampleBellmanFord() {
	// 0->1 (4), 0->2 (5), 1->3 (5), 2->1 (-3), 2->3 (6).
	// Cheapest 0->1 is via 2: 5 + (-3) = 2, so 0->3 is 2 + 5 = 7.
	c, m := buildWeighted(true, [][3]int{
		{0, 1, 4}, {0, 2, 5},
		{1, 3, 5},
		{2, 1, -3}, {2, 3, 6},
	})
	src, _ := m.Lookup(0)

	d, err := search.BellmanFord(c, src)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for v := 0; v < 4; v++ {
		id, _ := m.Lookup(v)
		dist, _ := d.Distance(id)
		fmt.Printf("dist to %d = %d\n", v, dist)
	}
	// Output:
	// dist to 0 = 0
	// dist to 1 = 2
	// dist to 2 = 5
	// dist to 3 = 7
}

// ExampleTarjanSCC finds the strongly connected components of a
// directed graph. Component membership is stable but the order in
// which components and their nodes are emitted is not, so the result
// is normalised (each component sorted, then components sorted) before
// printing.
func ExampleTarjanSCC() {
	// {0,1,2} form a cycle; 2->3 and 3 is its own component.
	c, m := buildDirected([][2]int{{0, 1}, {1, 2}, {2, 0}, {2, 3}})

	sccs := search.TarjanSCC(c)

	normalised := make([][]int, 0, len(sccs))
	for _, comp := range sccs {
		vs := resolvePath(m, comp)
		sort.Ints(vs)
		normalised = append(normalised, vs)
	}
	sort.Slice(normalised, func(i, j int) bool { return normalised[i][0] < normalised[j][0] })

	fmt.Printf("%d components: %v\n", len(normalised), normalised)
	// Output:
	// 2 components: [[0 1 2] [3]]
}

// ExampleTopologicalSort orders the vertices of a DAG so that every
// edge points forwards. The chain below has a single valid ordering,
// so the resolved output is deterministic.
func ExampleTopologicalSort() {
	// DAG chain: 0 -> 1 -> 2 -> 3 -> 4.
	c, m := buildDirected([][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}})

	order, err := search.TopologicalSort(c)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(resolvePath(m, order))
	// Output:
	// [0 1 2 3 4]
}

// ExampleKruskalMST computes a minimum spanning tree of an undirected
// weighted graph. The graph is built undirected so the CSR is
// symmetric, as Kruskal requires; the total weight is deterministic.
func ExampleKruskalMST() {
	// Undirected weighted graph on {0,1,2,3}.
	c, _ := buildWeighted(false, [][3]int{
		{0, 1, 1}, {1, 2, 2}, {2, 3, 3}, {0, 3, 4}, {0, 2, 5},
	})

	edges, total, err := search.KruskalMST(c)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%d edges, total weight %d\n", len(edges), total)
	// Output:
	// 3 edges, total weight 6
}

// ExampleWCC labels the weakly connected components of a directed
// graph (edges treated as undirected). The component count is stable;
// raw labels are compared for equality rather than printed.
func ExampleWCC() {
	// Two components: {0,1,2} and {3,4}.
	c, m := buildDirected([][2]int{{0, 1}, {1, 2}, {3, 4}})

	comp, k, err := search.WCC(c)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	id0, _ := m.Lookup(0)
	id2, _ := m.Lookup(2)
	id3, _ := m.Lookup(3)

	fmt.Printf("components: %d\n", k)
	fmt.Printf("0 and 2 together: %v\n", comp[id0] == comp[id2])
	fmt.Printf("0 and 3 together: %v\n", comp[id0] == comp[id3])
	// Output:
	// components: 2
	// 0 and 2 together: true
	// 0 and 3 together: false
}

// ExampleYenKShortest enumerates the k loopless shortest paths between
// two nodes, returned in non-decreasing total cost.
func ExampleYenKShortest() {
	// Two parallel routes 0 -> 3: cost 2 (via 1) and cost 4 (via 2).
	c, m := buildWeighted(true, [][3]int{
		{0, 1, 1}, {1, 3, 1},
		{0, 2, 2}, {2, 3, 2},
	})
	src, _ := m.Lookup(0)
	dst, _ := m.Lookup(3)

	paths := search.YenKShortest(c, src, dst, 2)
	for i, p := range paths {
		fmt.Printf("path %d: %v cost %d\n", i, resolvePath(m, p.Nodes), p.Cost)
	}
	// Output:
	// path 0: [0 1 3] cost 2
	// path 1: [0 2 3] cost 4
}
