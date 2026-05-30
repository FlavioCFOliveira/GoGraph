// Example 09_leiden — runs Leiden community detection on two K4
// cliques joined by a single bridge edge and prints the discovered
// communities.
//
// The graph has a textbook community structure: each K4 clique is
// densely connected internally and the two cliques touch only through
// one bridge edge, so a modularity-optimising method recovers exactly
// two communities. The example also shows how to read a Partition
// back: the Community slice is NodeID-indexed and ghost slots created
// by sharded packing carry the sentinel -1, so the report iterates the
// eight live nodes explicitly rather than walking the raw slice.
//
// Sample output: run `go run ./examples/09_leiden` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/community"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the two-clique graph, runs Leiden community detection, and
// writes the report to w. All output goes to w so a test can capture
// and assert it; run returns wrapped errors rather than terminating the
// process.
func run(w io.Writer) error {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})

	// First K4 clique: nodes 0..3, every pair connected.
	if err := addClique(a, 0, 4); err != nil {
		return err
	}
	// Second K4 clique: nodes 4..7, every pair connected.
	if err := addClique(a, 4, 8); err != nil {
		return err
	}
	// Single bridge edge joining the two cliques.
	if err := a.AddEdge(3, 4, struct{}{}); err != nil {
		return fmt.Errorf("AddEdge bridge 3->4: %w", err)
	}

	c := csr.BuildFromAdjList(a)
	p := community.Leiden(c, community.DefaultLeidenOptions())

	fmt.Fprintf(w, "Found %d communities across 8 live nodes\n", p.NumCommunities)

	// Print only the eight live nodes' community IDs in node order; ghost
	// slots in the Community slice carry the sentinel -1 and are skipped.
	mapper := a.Mapper()
	for v := 0; v < 8; v++ {
		id, ok := mapper.Lookup(v)
		if !ok {
			return fmt.Errorf("node %d not found in graph", v)
		}
		fmt.Fprintf(w, "  node %d -> community %d\n", v, p.Community[id])
	}
	return nil
}

// addClique adds every undirected edge between the nodes in the
// half-open range [lo, hi), turning them into a complete subgraph.
func addClique(a *adjlist.AdjList[int, struct{}], lo, hi int) error {
	for i := lo; i < hi; i++ {
		for j := i + 1; j < hi; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				return fmt.Errorf("AddEdge clique %d->%d: %w", i, j, err)
			}
		}
	}
	return nil
}
