package main

import "os"

// Example pins the deterministic stdout of the out-of-core PageRank
// example. The csrfile is written under an os.MkdirTemp directory whose
// absolute path varies per run, so run prints only the file's base name
// ("graph.csr"); every other line — the vertex and edge counts, the
// PageRank iteration count, the live-rank count, and the verified uniform
// rank — is deterministic for the hard-coded uniform 1000-node ring. Go's
// test framework captures everything run writes to os.Stdout and compares
// it against the // Output: block below, so a future change that alters
// the report is caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Tier 2: wrote graph.csr (1000 vertices, 1000 edges).
	// PageRank: converged in 1 iteration(s), 1000 live ranks.
	// Verify: uniform=true, min=max=0.001000, node 0 rank=0.001000 (expected 0.001000).
}
