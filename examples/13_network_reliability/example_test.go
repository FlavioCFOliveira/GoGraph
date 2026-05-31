package main

import "os"

// Example pins the deterministic stdout of the network-reliability
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters the structural analysis, the max-flow
// value, or the derived bottleneck is caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Single points of failure:
	//   articulation point: frankfurt
	//   articulation point: berlin
	//   articulation point: paris
	//   bridge: paris -- london
	//   bridge: berlin -- warsaw
	//   bridge: frankfurt -- berlin
	//
	// Max throughput lisbon -> frankfurt: 17 Gb/s
	// Bottleneck (min-cut, 17 Gb/s) — saturated links:
	//   madrid -- frankfurt (10 Gb/s, fully utilised)
	//   paris -- frankfurt (7 Gb/s, fully utilised)
}
