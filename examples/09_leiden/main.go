// Example 09_leiden — runs Leiden community detection on two K4
// cliques joined by a bridge and prints the discovered communities.
package main

import (
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/community"
)

func main() {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	for i := 4; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	a.AddEdge(3, 4, struct{}{})

	c := csr.BuildFromAdjList(a)
	p := community.Leiden(c, community.DefaultLeidenOptions())
	fmt.Printf("Found %d communities across 8 live nodes\n", p.NumCommunities)
	// Print only the 8 live nodes' community IDs; ghost slots in the
	// Community slice carry the sentinel -1.
	for v := 0; v < 8; v++ {
		id, _ := a.Mapper().Lookup(v)
		fmt.Printf("  node %d -> community %d\n", v, p.Community[id])
	}
}
