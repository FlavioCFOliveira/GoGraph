package main

import "os"

// Example pins the deterministic stdout of the Leiden community-detection
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters the discovered communities or the report is
// caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Found 2 communities across 8 live nodes
	//   node 0 -> community 0
	//   node 1 -> community 0
	//   node 2 -> community 0
	//   node 3 -> community 0
	//   node 4 -> community 1
	//   node 5 -> community 1
	//   node 6 -> community 1
	//   node 7 -> community 1
}
