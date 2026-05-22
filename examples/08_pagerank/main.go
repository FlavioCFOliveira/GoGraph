// Example 08_pagerank — runs PageRank on a 5-node directed cycle
// and prints each vertex's rank.
package main

import (
	"fmt"
	"log"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/centrality"
)

func main() {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 5; i++ {
		if err := a.AddEdge(i, (i+1)%5, struct{}{}); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	ranks, iters, _ := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	// Resolve the NodeIDs of the 5 user-facing nodes so we only print
	// the live ranks (the rank slice is indexed by NodeID and rounds
	// up to MaxNodeID across the 256-shard Mapper space).
	fmt.Printf("Converged in %d iterations (5 live ranks)\n", iters)
	for v := 0; v < 5; v++ {
		id, _ := a.Mapper().Lookup(v)
		fmt.Printf("  node %d: %.6f\n", v, ranks[id])
	}
}
