// Example 03_advanced_algorithms — exercises BFS, Dijkstra, Brandes
// betweenness centrality, and PageRank on one small undirected graph.
//
// It builds the graph with the mutable adjlist builder, freezes it into
// an immutable CSR snapshot, then runs all four algorithms against that
// snapshot and prints their results. Every per-node report resolves the
// compact NodeID back to its name through the adjacency list's mapper,
// skips ghost slots by enumerating only live nodes, and is sorted by
// name so the output is byte-for-byte deterministic.
//
// Sample output: run `go run ./examples/03_advanced_algorithms` and
// capture the stdout — the output is deterministic for the inputs
// hard-coded above and serves as the regression baseline a future
// change should preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sort"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
	"gograph/search/centrality"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the graph, runs the four algorithms, and writes the report
// to w. All output goes to w so a test can capture and assert it; run
// returns wrapped errors rather than terminating the process.
func run(w io.Writer) error {
	a := adjlist.New[string, int64](adjlist.Config{Directed: false})
	for _, e := range [...]struct {
		s, d string
		w    int64
	}{
		{"alice", "bob", 1},
		{"bob", "carol", 1},
		{"carol", "dave", 1},
		{"alice", "carol", 2},
	} {
		if err := a.AddEdge(e.s, e.d, e.w); err != nil {
			return fmt.Errorf("AddEdge %s->%s: %w", e.s, e.d, err)
		}
	}

	c := csr.BuildFromAdjList(a)
	mapper := a.Mapper()

	src, ok := mapper.Lookup("alice")
	if !ok {
		return fmt.Errorf("source node %q not found in graph", "alice")
	}

	// BFS — reachability layers from alice, in non-decreasing depth.
	fmt.Fprintln(w, "BFS from alice:")
	var bfsErr error
	search.BFS(c, src, func(n graph.NodeID, depth int) bool {
		name, resolved := mapper.Resolve(n)
		if !resolved {
			bfsErr = fmt.Errorf("unresolved node id %d during BFS", n)
			return false
		}
		fmt.Fprintf(w, "  %-5s at depth %d\n", name, depth)
		return true
	})
	if bfsErr != nil {
		return bfsErr
	}

	// Dijkstra — shortest weighted distance alice -> dave.
	d, err := search.Dijkstra(c, src)
	if err != nil {
		return fmt.Errorf("dijkstra: %w", err)
	}
	dave, ok := mapper.Lookup("dave")
	if !ok {
		return fmt.Errorf("destination node %q not found in graph", "dave")
	}
	dist, reachable := d.Distance(dave)
	if !reachable {
		return fmt.Errorf("dave is unreachable from alice")
	}
	fmt.Fprintf(w, "Dijkstra alice -> dave: %d\n", dist)

	// Live nodes resolved to names, sorted by name. This is the stable
	// ordering reused by both centrality reports; LiveNodes already skips
	// ghost slots from the sharded NodeID packing.
	live, err := liveNamed(mapper, c.LiveNodes())
	if err != nil {
		return err
	}

	// Brandes betweenness — how often each node lies on shortest paths.
	bc := centrality.Betweenness(c)
	fmt.Fprintln(w, "Betweenness centrality:")
	for _, ln := range live {
		fmt.Fprintf(w, "  %-5s %.4f\n", ln.name, bc[ln.id])
	}

	// PageRank — stationary importance under the random-surfer model.
	ranks, iters, err := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	if err != nil {
		return fmt.Errorf("PageRank: %w", err)
	}
	fmt.Fprintf(w, "PageRank converged in %d iterations:\n", iters)
	for _, ln := range live {
		fmt.Fprintf(w, "  %-5s %.4f\n", ln.name, ranks[ln.id])
	}

	return nil
}

// liveNode pairs a live NodeID with the name it resolves to.
type liveNode struct {
	id   graph.NodeID
	name string
}

// liveNamed resolves each live NodeID to its name and returns the pairs
// sorted by name, so any map-derived or NodeID-indexed report can be
// printed in a stable, deterministic order. It returns an error if any
// id fails to resolve, which would indicate a corrupted snapshot.
func liveNamed(mapper *graph.Mapper[string], ids []graph.NodeID) ([]liveNode, error) {
	out := make([]liveNode, len(ids))
	for i, id := range ids {
		name, ok := mapper.Resolve(id)
		if !ok {
			return nil, fmt.Errorf("unresolved live node id %d", id)
		}
		out[i] = liveNode{id: id, name: name}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].id < out[j].id
	})
	return out, nil
}
