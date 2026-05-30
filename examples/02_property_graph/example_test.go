package main

import "os"

// Example pins the deterministic stdout of the property-graph example.
// Every result group is sorted inside run, so the output is byte-stable
// across runs and machines. Go's test framework captures everything run
// writes to os.Stdout and compares it against the // Output: block
// below, so a future change that alters the report — or that stops
// reading the typed properties back — is caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// All Admins:
	//   alice    name=alice    age=30
	//   dave     name=dave     age=42
	// Persons aged 30:
	//   alice    name=alice    age=30
	//   charlie  name=charlie  age=30
	// One-hop out from Admins:
	//   bob      name=bob      age=25
	//   charlie  name=charlie  age=30
}
