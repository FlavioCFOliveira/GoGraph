// Example 14_routing_alternatives — compare three flavours of
// shortest-path computation on the same routing graph:
// classical Dijkstra, Yen's k-shortest for alternatives, and A*
// with a simple coordinate-based heuristic.
//
// Sample output: run `go run ./examples/14_routing_alternatives` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"log"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

func main() {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	type leg struct {
		from, to string
		w        int64
	}
	for _, l := range []leg{
		{"lisbon", "madrid", 624},
		{"lisbon", "porto", 313},
		{"porto", "madrid", 568},
		{"madrid", "barcelona", 622},
		{"porto", "barcelona", 1200},
		{"madrid", "paris", 1274},
		{"barcelona", "paris", 1000},
		{"paris", "berlin", 1054},
		{"barcelona", "berlin", 1500},
	} {
		if err := a.AddEdge(l.from, l.to, l.w); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)

	src, _ := a.Mapper().Lookup("lisbon")
	dst, _ := a.Mapper().Lookup("berlin")

	// 1. Single shortest path.
	d, _ := search.Dijkstra(c, src)
	cost, _ := d.Distance(dst)
	fmt.Printf("Dijkstra lisbon -> berlin: %d km\n", cost)
	for _, n := range d.Path(dst) {
		name, _ := a.Mapper().Resolve(n)
		fmt.Printf("  -> %s\n", name)
	}

	// 2. Three k-shortest alternatives.
	fmt.Println("\nYen's 3 shortest paths lisbon -> berlin:")
	paths := search.YenKShortest(c, src, dst, 3)
	for i, p := range paths {
		names := make([]string, 0, len(p.Nodes))
		for _, n := range p.Nodes {
			name, _ := a.Mapper().Resolve(n)
			names = append(names, name)
		}
		fmt.Printf("  %d. %d km via %v\n", i+1, p.Cost, names)
	}

	// 3. A* with a heuristic that returns 0 for everyone (so the
	// search degenerates to Dijkstra). In a real routing setup the
	// heuristic would use great-circle distances; the structural
	// point of this example is the api shape.
	fmt.Println("\nA* lisbon -> berlin (zero heuristic):")
	h := func(graph.NodeID) int64 { return 0 }
	path, cost, err := search.AStar(c, src, dst, h)
	if err != nil {
		fmt.Println("  no path:", err)
		return
	}
	fmt.Printf("  cost = %d km, %d hops\n", cost, len(path)-1)
}
