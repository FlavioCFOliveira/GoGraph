package main

import "os"

// Example pins the deterministic stdout of the typed-recovery example.
// Although run persists to an os.MkdirTemp directory, the temp path
// never reaches stdout, so every printed line is byte-stable. Go's
// test framework captures everything run writes to os.Stdout and
// compares it against the // Output: block below, so a future change
// that alters the recovery report — counts, recovered weights, the
// schema-version confirmation — is caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Committed 3 weighted edges; snapshot persisted.
	// Recovered: WAL ops=9, snapshot hit=true, schema version=v2, label records=6, property records=3.
	//   recovered 1001 -[PRIMARY]-> 1002  weight=1  (label OK: true, weight bit-exact: true)
	//   recovered 1002 -[ALTERNATE]-> 1003  weight=3.141592653589793  (label OK: true, weight bit-exact: true)
	//   recovered 1003 -[DEGRADED]-> 1004  weight=1e-300  (label OK: true, weight bit-exact: true)
	//   node 1001.name = "origin"
	//   edge (1001,1002).latency_ms = 0.5
	//   schema version v2 confirmed (non-string graph: no mapper.bin).
}
