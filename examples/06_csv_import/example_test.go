package main

import "os"

// Example pins the deterministic stdout of the CSV-import example. Go's
// test framework captures everything run writes to os.Stdout and
// compares it against the // Output: block below, so a future change
// that alters the ingest count or either serialisation is caught as a
// regression. The node and edge orderings are stable because both
// writers iterate by ascending NodeID, which the mapper assigns in
// insertion order.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Ingested 3 rows
	//
	// CSV out:
	// alice,bob,1
	// bob,carol,2
	// carol,alice,3
	//
	// JSON Lines out:
	// {"type":"node","id":"alice"}
	// {"type":"node","id":"bob"}
	// {"type":"node","id":"carol"}
	// {"type":"edge","src":"alice","dst":"bob","weight":1}
	// {"type":"edge","src":"bob","dst":"carol","weight":2}
	// {"type":"edge","src":"carol","dst":"alice","weight":3}
}
