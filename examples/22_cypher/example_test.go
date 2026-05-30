package main

import (
	"context"
	"os"
)

// Example pins the deterministic stdout of the Cypher example. Every
// query that returns more than one row carries an ORDER BY, so the
// output is byte-stable. Go's test framework captures everything run
// writes to os.Stdout and compares it against the // Output: block
// below, so a future change that alters the report — or that regresses
// MATCH, WHERE, ORDER BY, the relationship pattern, or CREATE — is
// caught as a regression.
func Example() {
	_ = run(context.Background(), os.Stdout)
	// Output:
	// MATCH (n:Person) RETURN n.name AS name ORDER BY name
	//   Alice
	//   Bob
	//   Carol
	//   Dave
	//   Eve
	//
	// MATCH (n:Person) WHERE n.age > 25 RETURN n.name AS name, n.age AS age ORDER BY name
	//   Alice age 30
	//   Carol age 35
	//   Dave  age 28
	//
	// MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS from, b.name AS to ORDER BY from, to
	//   Alice KNOWS Bob
	//   Bob KNOWS Carol
	//   Carol KNOWS Dave
	//   Dave KNOWS Eve
	//
	// CREATE (n:Guest {name: "Frank"})
	//   created Guest{name: "Frank"} — label registered in graph
}
