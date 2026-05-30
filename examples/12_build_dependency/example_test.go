package main

import "os"

// Example pins the deterministic stdout of the build-dependency
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters the report is caught as a regression.
//
// The build order is stable because NodeIDs are assigned in a fixed
// first-appearance order and Kahn's algorithm emits in ascending
// NodeID order; the cycle members are sorted alphabetically, so this
// output does not depend on Tarjan's internal traversal order.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// === Build order (no cycles) ===
	//   1. logging
	//   2. db
	//   3. crypto
	//   4. store
	//   5. auth
	//   6. app
	//
	// === Detecting a cycle ===
	// topological sort rejects the cycle (ErrCycle).
	// Strongly connected components (size > 1 are cycles):
	//   cycle: [app auth db logging store]
}
