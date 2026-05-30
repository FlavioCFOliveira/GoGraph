package main

import "os"

// Example pins the deterministic stdout of the out-of-core pipeline
// example. The csrfile is written under an os.MkdirTemp directory whose
// absolute path varies per run, so run prints only the file's base name
// ("graph.csr"); every other line — the CSV ingest count, the captured
// seed NodeID, the BFS visited count, the PageRank iteration count, and
// the live-rank count — is deterministic. Go's test framework captures
// everything run writes to os.Stdout and compares it against the
// // Output: block below, so a future change that alters the report is
// caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// CSV: 7 edges ingested.
	// Wrote graph.csr (7 edges).
	//
	// Semi-external BFS from alice (NodeID 7):
	//   visited 5 nodes.
	// Semi-external PageRank converged in 58 iterations (5 live ranks).
}
