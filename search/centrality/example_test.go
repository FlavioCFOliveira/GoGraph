package centrality_test

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// ExamplePageRank ranks the nodes of a directed star where five leaves
// all point at one sink. The sink accumulates the dominant share of
// the stationary mass; the five leaves are symmetric and share the
// remainder equally. Ranks are rounded to four decimals for a stable,
// deterministic comparison.
func ExamplePageRank() {
	// Leaves 1..5 each point at sink 0.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for leaf := 1; leaf <= 5; leaf++ {
		_ = a.AddEdge(leaf, 0, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	m := a.Mapper()

	ranks, _, err := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	sink, _ := m.Lookup(0)
	leaf, _ := m.Lookup(1)
	fmt.Printf("sink rank = %.4f\n", ranks[sink])
	fmt.Printf("leaf rank = %.4f\n", ranks[leaf])
	// Output:
	// sink rank = 0.5122
	// leaf rank = 0.0976
}

// ExampleBetweenness computes (non-normalised) betweenness centrality
// on an undirected path 0-1-2. Every shortest path between the two
// ends runs through the middle node, so node 1 carries all the
// betweenness while the endpoints carry none.
func ExampleBetweenness() {
	// Undirected path: 0 - 1 - 2.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	_ = a.AddEdge(0, 1, struct{}{})
	_ = a.AddEdge(1, 2, struct{}{})
	c := csr.BuildFromAdjList(a)
	m := a.Mapper()

	bc := centrality.Betweenness(c)
	for v := 0; v < 3; v++ {
		id, _ := m.Lookup(v)
		fmt.Printf("node %d betweenness = %.1f\n", v, bc[id])
	}
	// Output:
	// node 0 betweenness = 0.0
	// node 1 betweenness = 2.0
	// node 2 betweenness = 0.0
}
