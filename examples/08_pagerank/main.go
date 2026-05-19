// Example 08_pagerank — runs PageRank on a 5-node directed cycle
// and prints each vertex's rank.
package main

import (
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/centrality"
)

func main() {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 5; i++ {
		a.AddEdge(i, (i+1)%5, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	ranks, iters := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	fmt.Printf("Converged in %d iterations\n", iters)
	for i, r := range ranks {
		fmt.Printf("  node %d: %.6f\n", i, r)
	}
}
