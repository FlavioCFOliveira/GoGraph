// Example 09_leiden — runs Leiden community detection on two K4
// cliques joined by a bridge and prints the discovered communities.
//
// Sample output: run `go run ./examples/09_leiden` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"log"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/community"
)

func main() {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				log.Fatalf("AddEdge: %v", err)
			}
		}
	}
	for i := 4; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				log.Fatalf("AddEdge: %v", err)
			}
		}
	}
	if err := a.AddEdge(3, 4, struct{}{}); err != nil {
		log.Fatalf("AddEdge: %v", err)
	}

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
