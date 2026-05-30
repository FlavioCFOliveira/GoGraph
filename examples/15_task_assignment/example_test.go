package main

import "os"

// Example pins the deterministic stdout of the task-assignment example.
// Go's test framework captures everything run writes to os.Stdout and
// compares it against the // Output: block below, so a future change
// that alters the report — the Hungarian assignment, the willing set,
// the Hopcroft-Karp matching, or the comparison — is caught as a
// regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// === Minimum-cost assignment (Hungarian) ===
	//   alice   -> task-D  (cost 5, willing)
	//   bob     -> task-A  (cost 6, willing)
	//   carol   -> task-B  (cost 3, willing)
	//   dave    -> task-C  (cost 4, willing)
	//   total = 18
	//
	// === Willing set (worker accepts task when cost <= 6) ===
	//   alice   willing: task-B(4) task-D(5)
	//           refuses: task-A(8) task-C(7)
	//   bob     willing: task-A(6) task-C(5) task-D(6)
	//           refuses: task-B(9)
	//   carol   willing: task-A(5) task-B(3)
	//           refuses: task-C(8) task-D(7)
	//   dave    willing: task-B(6) task-C(4)
	//           refuses: task-A(7) task-D(9)
	//
	// === Maximum willing matching (Hopcroft-Karp) ===
	//   alice   -> task-D  (cost 5)
	//   bob     -> task-C  (cost 5)
	//   carol   -> task-A  (cost 5)
	//   dave    -> task-B  (cost 6)
	//   matched pairs: 4 of 4 workers
	//
	// === Comparison ===
	//   Hungarian: all 4 pairs are within the willing set (total cost 18).
	//   Hopcroft-Karp: 4 of 4 workers can be staffed using willing pairs only (total cost 21).
	//   Verdict: the willingness rule is not binding here — the cheapest
	//   assignment is already fully willing and every worker stays staffed,
	//   so cost-optimality (18) and full coverage (4/4) are achievable together.
}
