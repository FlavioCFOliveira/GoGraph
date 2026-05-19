// Example 18_oocore_pipeline — out-of-core ingestion pipeline.
// Reads a graph from a CSV stream, builds a CSR snapshot, writes
// a Tier 2 csrfile, re-opens it via mmap, applies an access-
// pattern hint, and runs semi-external BFS plus PageRank over
// the mapped region.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/graph/io/csv"
	"gograph/search/extern"
	"gograph/store/csrfile"
)

func main() {
	dir, _ := os.MkdirTemp("", "gograph-ex18-")
	defer func() { _ = os.RemoveAll(dir) }()
	outPath := filepath.Join(dir, "graph.csr")

	// Step 1 — read an edge-list CSV into the bulk loader.
	const input = `# a tiny social graph
alice,bob,1
bob,carol,1
carol,dave,1
dave,erin,1
erin,alice,1
alice,carol,1
bob,dave,1
`
	adj, n, err := csv.ReadInto(strings.NewReader(input), csv.DefaultOptions())
	if err != nil {
		panic(err)
	}
	fmt.Printf("CSV: %d edges ingested.\n", n)

	// Capture the NodeID of "alice" before we discard the in-memory
	// adjacency: the Tier 2 csrfile preserves NodeIDs across the
	// mmap boundary, but it drops the string -> NodeID mapping, so
	// we need a known seed to drive the BFS below.
	aliceID, ok := adj.Mapper().Lookup("alice")
	if !ok {
		panic("alice not interned")
	}

	// Step 2 — build the CSR snapshot and atomically write the
	// Tier 2 csrfile.
	c := csr.BuildFromAdjList(adj)
	if _, err := csrfile.WriteToFile(outPath, c); err != nil {
		panic(err)
	}
	fmt.Printf("Wrote %s (%d edges).\n", outPath, c.Size())

	// Step 3 — mmap the Tier 2 file and hint sequential access.
	r, err := csrfile.Open(outPath)
	if err != nil {
		panic(err)
	}
	defer func() { _ = r.Close() }()
	_ = r.SetHint(csrfile.AccessSequential)

	// Step 4 — semi-external BFS from alice plus PageRank.
	fmt.Printf("\nSemi-external BFS from alice (NodeID %d):\n", aliceID)
	visited := 0
	_ = extern.BFS(r, aliceID, func(_ graph.NodeID, _ int) bool {
		visited++
		return true
	})
	fmt.Printf("  visited %d nodes.\n", visited)

	ranks, iters := extern.PageRank(r, extern.DefaultPageRankOptions())
	fmt.Printf("Semi-external PageRank converged in %d iterations (%d ranks).\n", iters, len(ranks))
}
