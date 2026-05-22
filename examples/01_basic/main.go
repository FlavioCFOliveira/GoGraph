// Example 01_basic — build a small weighted directed graph, snapshot
// it to a CSR view, and run a single-source shortest-paths query.
package main

import (
	"fmt"
	"log"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

func main() {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for _, edge := range [...]struct {
		s, d string
		w    int64
	}{
		{"Lisbon", "Madrid", 624},
		{"Lisbon", "Paris", 1737},
		{"Madrid", "Paris", 1274},
		{"Madrid", "Rome", 1969},
		{"Paris", "Rome", 1422},
	} {
		if err := a.AddEdge(edge.s, edge.d, edge.w); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}

	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup("Lisbon")

	d, err := search.Dijkstra(c, src)
	if err != nil {
		log.Fatalf("Dijkstra: %v", err)
	}
	for _, city := range []string{"Madrid", "Paris", "Rome"} {
		id, _ := a.Mapper().Lookup(city)
		dist, _ := d.Distance(id)
		fmt.Printf("Lisbon -> %-7s : %4d km\n", city, dist)
	}
}
