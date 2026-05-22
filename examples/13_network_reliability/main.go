// Example 13_network_reliability — locate single points of failure
// (bridges and articulation points) in a small communication
// network, then compute the maximum throughput between two sites
// via Dinic's max-flow.
package main

import (
	"fmt"
	"log"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
	"gograph/search/flow"
)

func main() {
	// An undirected communication backbone with seven sites.
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: false})
	for _, e := range [][2]string{
		{"lisbon", "porto"},
		{"porto", "madrid"},
		{"madrid", "paris"},
		{"paris", "berlin"},
		{"berlin", "warsaw"},
		{"berlin", "amsterdam"},
		{"amsterdam", "london"},
	} {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)

	fmt.Println("Single points of failure:")
	res := search.HopcroftTarjanBCC(c)
	if len(res.Articulation) == 0 {
		fmt.Println("  none — network is biconnected.")
	} else {
		for _, id := range res.Articulation {
			name, _ := a.Mapper().Resolve(id)
			fmt.Printf("  - %s is an articulation point\n", name)
		}
	}
	if len(res.Bridges) == 0 {
		fmt.Println("  no bridges.")
	} else {
		for _, b := range res.Bridges {
			u, _ := a.Mapper().Resolve(b[0])
			v, _ := a.Mapper().Resolve(b[1])
			fmt.Printf("  - bridge between %s and %s\n", u, v)
		}
	}

	// Throughput between two sites. Build a capacitated network
	// where every backbone link carries 10 units; show the max
	// possible throughput from lisbon to warsaw.
	fmt.Println("\nMax throughput lisbon -> warsaw:")
	sites := map[string]int{
		"lisbon": 0, "porto": 1, "madrid": 2, "paris": 3,
		"berlin": 4, "warsaw": 5, "amsterdam": 6, "london": 7,
	}
	g := flow.NewNetwork(len(sites))
	add := func(u, v string, c int) {
		g.AddEdge(sites[u], sites[v], c)
		g.AddEdge(sites[v], sites[u], c)
	}
	add("lisbon", "porto", 10)
	add("porto", "madrid", 10)
	add("madrid", "paris", 10)
	add("paris", "berlin", 10)
	add("berlin", "warsaw", 10)
	add("berlin", "amsterdam", 10)
	add("amsterdam", "london", 10)
	fmt.Printf("  %d units/sec\n", flow.MaxFlow(g, sites["lisbon"], sites["warsaw"]))
}
