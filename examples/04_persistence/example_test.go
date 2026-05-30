package main

import "os"

// Example pins the deterministic stdout of the persistence
// walk-through. The example writes its WAL and snapshot to a directory
// created with os.MkdirTemp, but that path is never printed, so the
// report below is byte-stable. Go's test framework captures everything
// run writes to os.Stdout and compares it against the // Output: block,
// so a future change that alters the report — or that breaks recovery
// of labels or typed properties — is caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Committed 3 transactions to the WAL.
	// Typed properties set on alice and edge alice->bob.
	// v2 snapshot persisted: csr.bin + labels.bin + properties.bin + manifest.json.
	// Recovered: WAL ops=12, snapshot hit=true, snapshot label records=7, snapshot property records=5.
	//   recovered alice -[KNOWS]-> bob (src carries "Person")
	//   recovered bob -[KNOWS]-> carol (src carries "Person")
	//   recovered carol -[FOLLOWS]-> dave (src carries "Person")
	//   recovered alice.name = "Alice"
	//   recovered alice.age = 30
	//   recovered alice.joined = 2026-05-19T12:00:00Z
	//   recovered edge(alice,bob).since = "2026"
	//   recovered edge(alice,bob).weight = 7
}
