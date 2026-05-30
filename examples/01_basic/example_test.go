package main

import "os"

// Example pins the deterministic stdout of the basic shortest-paths
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters the report is caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Lisbon -> Madrid  :  624 km   route: Lisbon -> Madrid
	// Lisbon -> Paris   : 1737 km   route: Lisbon -> Paris
	// Lisbon -> Rome    : 2593 km   route: Lisbon -> Madrid -> Rome
}
