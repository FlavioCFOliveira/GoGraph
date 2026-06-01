package graph_test

import (
	"fmt"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ExampleMapper shows the core contract of a Mapper: arbitrary
// comparable user keys are interned to compact NodeID values, and the
// mapping is stable — the same key always yields the same NodeID, which
// resolves back to the original key.
func ExampleMapper() {
	m := graph.NewMapper[string]()

	// Interning a key returns its NodeID; re-interning the same key
	// returns the identical NodeID (the mapping is stable for the
	// Mapper's lifetime).
	alice := m.Intern("alice")
	again := m.Intern("alice")
	bob := m.Intern("bob")

	// Resolve reverses the mapping back to the user key.
	key, ok := m.Resolve(alice)

	fmt.Println("alice stable:", alice == again)
	fmt.Println("alice != bob:", alice != bob)
	fmt.Println("resolve:", key, ok)
	fmt.Println("interned:", m.Len())
	// Output:
	// alice stable: true
	// alice != bob: true
	// resolve: alice true
	// interned: 2
}

// ExampleMapper_Lookup distinguishes Lookup (read-only) from Intern
// (which assigns a NodeID on first reference). Lookup never mutates the
// Mapper, so it reports whether a key has already been interned.
func ExampleMapper_Lookup() {
	m := graph.NewMapper[string]()
	m.Intern("alice")

	_, known := m.Lookup("alice")
	_, unknown := m.Lookup("carol")

	fmt.Println("alice known:", known)
	fmt.Println("carol known:", unknown)
	// Output:
	// alice known: true
	// carol known: false
}

// ExampleMapper_Walk performs a bulk export of every interned
// (NodeID, key) pair. Walk visits each pair exactly once; the iteration
// order follows the Mapper's internal sharding rather than insertion
// order, so callers that need a stable order sort the result themselves.
func ExampleMapper_Walk() {
	m := graph.NewMapper[string]()
	m.Intern("alice")
	m.Intern("bob")
	m.Intern("carol")

	var keys []string
	m.Walk(func(_ graph.NodeID, key string) bool {
		keys = append(keys, key)
		return true // returning false would stop the walk early
	})
	sort.Strings(keys)

	fmt.Println(keys)
	// Output:
	// [alice bob carol]
}
