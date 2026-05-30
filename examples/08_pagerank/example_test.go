package main

import "os"

// Example pins the deterministic stdout of the PageRank example. Go's
// test framework captures everything run writes to os.Stdout and
// compares it against the // Output: block below, so a future change
// that alters the ranking is caught as a regression. The ties between
// B/C and D/E are resolved by name, which is what keeps the order
// stable across runs.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Converged in 86 iterations (6 live pages)
	//   1. page A: 0.331875
	//   2. page H: 0.307095
	//   3. page B: 0.155515
	//   4. page C: 0.155515
	//   5. page D: 0.025000
	//   6. page E: 0.025000
}
