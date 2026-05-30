package main

import "os"

// Example pins the deterministic stdout of the pattern-query example.
// Go's test framework captures everything run writes to os.Stdout and
// compares it against the // Output: block below; every query result is
// sorted by node key, so a future change that alters the report — or
// breaks the deterministic ordering — is caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// MATCH (n:Person:Admin) RETURN n.name, n.age, n.dept
	//   - alice  age=30  dept=eng
	//   - dave   age=42  dept=eng
	//
	// MATCH (n:Person) WHERE n.age = 30 RETURN n.name, n.age, n.dept
	//   - alice  age=30  dept=eng
	//   - carol  age=30  dept=ops
	//
	// MATCH (n:Admin)-->(b) RETURN b.name, b.age, b.dept  (one hop out)
	//   - bob    age=25  dept=eng
	//   - carol  age=30  dept=ops
	//   - erin   age=28  dept=ops
	//
	// MATCH (n:Person {dept: 'ops'}) RETURN n.name, n.age, n.dept
	//   - carol  age=30  dept=ops
	//   - erin   age=28  dept=ops
}
