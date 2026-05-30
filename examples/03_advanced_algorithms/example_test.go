package main

import "os"

// Example pins the deterministic stdout of the advanced-algorithms
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters any of the four algorithm reports is caught
// as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// BFS from alice:
	//   alice at depth 0
	//   bob   at depth 1
	//   carol at depth 1
	//   dave  at depth 2
	// Dijkstra alice -> dave: 3
	// Betweenness centrality:
	//   alice 0.0000
	//   bob   0.0000
	//   carol 4.0000
	//   dave  0.0000
	// PageRank converged in 29 iterations:
	//   alice 0.2459
	//   bob   0.2459
	//   carol 0.3667
	//   dave  0.1414
}
