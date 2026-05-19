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
	fmt.Printf("Found %d communities\n", p.NumCommunities)
	for i, cid := range p.Community {
		fmt.Printf("  node %d -> community %d\n", i, cid)
	}
}
