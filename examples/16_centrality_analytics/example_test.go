package main

import "os"

// Example pins the deterministic stdout of the centrality-analytics
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters the rankings is caught as a regression.
//
// The output is byte-stable because run sorts both reports: the
// betweenness ranking breaks score ties by node name, and the cluster
// listing sorts the community IDs and the member names.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Betweenness (higher = more critical):
	//   jose    12.00
	//   marie   12.00
	//   ana     0.00
	//   anne    0.00
	//   luis    0.00
	//   pierre  0.00
	//
	// Label propagation clusters:
	//   community 0: [ana jose luis]
	//   community 1: [anne marie pierre]
}
