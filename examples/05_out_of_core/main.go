// Example 05_out_of_core — writes a Tier 2 csrfile, opens it via
// mmap, applies a SEQUENTIAL madvise hint, and runs PageRank
// directly over the mapped region.
//
// Sample output: run `go run ./examples/05_out_of_core` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/extern"
	"gograph/store/csrfile"
)

func main() {
	dir, _ := os.MkdirTemp("", "gograph-ex05-")
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "graph.csr")

	a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true})
	for i := uint32(0); i < 1000; i++ {
		if err := a.AddEdge(i, (i+1)%1000, struct{}{}); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		log.Fatalf("csrfile.WriteToFile: %v", err)
	}

	r, err := csrfile.Open(path)
	if err != nil {
		log.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	_ = r.SetHint(csrfile.AccessSequential)

	ranks, iters, _ := extern.PageRank(r, extern.DefaultPageRankOptions())
	fmt.Printf("Tier 2 PageRank converged in %d iters; %d ranks\n", iters, len(ranks))
}
