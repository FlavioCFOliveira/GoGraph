// Example 18_oocore_pipeline — out-of-core ingestion pipeline.
// Reads a graph from a CSV stream, builds a CSR snapshot, writes
// a Tier 2 csrfile, re-opens it via mmap, applies an access-
// pattern hint, and runs semi-external BFS plus PageRank over
// the mapped region.
//
// Sample output: run `go run ./examples/18_oocore_pipeline` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve. The csrfile is written to an os.MkdirTemp directory whose
// absolute path varies per run, so the report prints only the file's
// base name to keep stdout byte-stable.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/search/extern"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run executes the full out-of-core pipeline — CSV ingest, CSR build,
// Tier 2 csrfile write, mmap re-open, and semi-external BFS plus
// PageRank — and writes the report to w. All output goes to w so a
// test can capture and assert it; run returns wrapped errors rather
// than terminating the process.
//
// The csrfile is written under an os.MkdirTemp directory whose absolute
// path differs on every run. To keep stdout byte-stable, run prints
// only the file's base name, never the temp directory path.
func run(w io.Writer) error {
	dir, err := os.MkdirTemp("", "gograph-ex18-")
	if err != nil {
		return fmt.Errorf("MkdirTemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	outPath := filepath.Join(dir, "graph.csr")

	// Step 1 — read an edge-list CSV via the in-memory CSV reader
	// (csv.ReadInto returns an adjlist; for very large inputs the
	// streaming store/bulk Loader would be a better fit).
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
		return fmt.Errorf("csv.ReadInto: %w", err)
	}
	fmt.Fprintf(w, "CSV: %d edges ingested.\n", n)

	// Capture the NodeID of "alice" before we discard the in-memory
	// adjacency: the Tier 2 csrfile preserves NodeIDs across the
	// mmap boundary, but it drops the string -> NodeID mapping, so
	// we need a known seed to drive the BFS below.
	aliceID, ok := adj.Mapper().Lookup("alice")
	if !ok {
		return fmt.Errorf("alice not interned")
	}

	// Step 2 — build the CSR snapshot and atomically write the
	// Tier 2 csrfile.
	c := csr.BuildFromAdjList(adj)
	if _, err := csrfile.WriteToFile(outPath, c); err != nil {
		return fmt.Errorf("csrfile.WriteToFile: %w", err)
	}
	// Print only the base name, never the temp directory's absolute
	// path, so the report stays byte-stable across runs.
	fmt.Fprintf(w, "Wrote %s (%d edges).\n", filepath.Base(outPath), c.Size())

	// Step 3 — mmap the Tier 2 file and hint sequential access.
	r, err := csrfile.Open(outPath)
	if err != nil {
		return fmt.Errorf("csrfile.Open: %w", err)
	}
	defer func() { _ = r.Close() }()
	_ = r.SetHint(csrfile.AccessSequential)

	// Step 4 — semi-external BFS from alice plus PageRank.
	fmt.Fprintf(w, "\nSemi-external BFS from alice (NodeID %d):\n", aliceID)
	visited := 0
	_ = extern.BFS(r, aliceID, func(_ graph.NodeID, _ int) bool {
		visited++
		return true
	})
	fmt.Fprintf(w, "  visited %d nodes.\n", visited)

	ranks, iters, _ := extern.PageRank(r, extern.DefaultPageRankOptions())
	// Count live ranks (non-zero) so the report matches the actual
	// graph size, not the sharded MaxNodeID-rounded slice length.
	var live int
	for _, x := range ranks {
		if x > 0 {
			live++
		}
	}
	fmt.Fprintf(w, "Semi-external PageRank converged in %d iterations (%d live ranks).\n", iters, live)
	return nil
}
