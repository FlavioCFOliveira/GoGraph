// Example 01_basic — build a small weighted directed graph, snapshot
// it to a CSR view, and run a single-source shortest-paths query.
//
// For each reachable city it prints both the shortest distance from
// Lisbon and the route taken (the parent chain reconstructed from the
// Dijkstra result, resolved back to city names through the adjacency
// list's mapper).
//
// Sample output: run `go run ./examples/01_basic` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the graph, runs the single-source shortest-paths query,
// and writes the report to w. All output goes to w so a test can
// capture and assert it; run returns wrapped errors rather than
// terminating the process.
func run(w io.Writer) error {
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
			return fmt.Errorf("AddEdge %s->%s: %w", edge.s, edge.d, err)
		}
	}

	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()
	src, ok := mapper.Lookup("Lisbon")
	if !ok {
		return fmt.Errorf("source city %q not found in graph", "Lisbon")
	}

	d, err := search.Dijkstra(c, src)
	if err != nil {
		return fmt.Errorf("dijkstra: %w", err)
	}

	for _, city := range []string{"Madrid", "Paris", "Rome"} {
		id, ok := mapper.Lookup(city)
		if !ok {
			return fmt.Errorf("destination city %q not found in graph", city)
		}
		dist, reachable := d.Distance(id)
		if !reachable {
			fmt.Fprintf(w, "Lisbon -> %-7s : unreachable\n", city)
			continue
		}
		route, err := routeNames(mapper, d.Path(id))
		if err != nil {
			return fmt.Errorf("resolve route to %s: %w", city, err)
		}
		fmt.Fprintf(w, "Lisbon -> %-7s : %4d km   route: %s\n", city, dist, route)
	}
	return nil
}

// routeNames resolves a path of NodeIDs back to a human-readable
// "A -> B -> C" string through the mapper. It returns an error if any
// id cannot be resolved, which would indicate a corrupted result.
func routeNames(mapper *graph.Mapper[string], path []graph.NodeID) (string, error) {
	names := make([]string, len(path))
	for i, id := range path {
		name, ok := mapper.Resolve(id)
		if !ok {
			return "", fmt.Errorf("unresolved node id %d", id)
		}
		names[i] = name
	}
	return strings.Join(names, " -> "), nil
}
