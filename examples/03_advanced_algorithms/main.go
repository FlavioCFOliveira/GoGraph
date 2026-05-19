// Example 03_advanced_algorithms — exercises BFS, Dijkstra,
// PageRank, and Brandes betweenness on a small graph.
package main

import (
	"fmt"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
	"gograph/search/centrality"
)

func main() {
	a := adjlist.New[string, int64](adjlist.Config{Directed: false})
	for _, e := range [][3]any{
		{"alice", "bob", int64(1)},
		{"bob", "carol", int64(1)},
		{"carol", "dave", int64(1)},
		{"alice", "carol", int64(2)},
	} {
		a.AddEdge(e[0].(string), e[1].(string), e[2].(int64))
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup("alice")

	fmt.Println("BFS from alice:")
	search.BFS(c, src, func(n graph.NodeID, depth int) bool {
		name, _ := a.Mapper().Resolve(n)
		fmt.Printf("  %s at depth %d\n", name, depth)
		return true
	})

	d, _ := search.Dijkstra(c, src)
	dave, _ := a.Mapper().Lookup("dave")
	dist, _ := d.Distance(dave)
	fmt.Printf("Dijkstra alice -> dave: %d\n", dist)

	ranks, iters := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	fmt.Printf("PageRank converged in %d iterations\n", iters)
	_ = ranks
}
