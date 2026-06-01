package ds_test

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/ds"
)

// ExampleUnionFind shows the disjoint-set workflow: elements start in
// their own singleton sets, Union merges the sets containing two
// elements, and Connected reports whether two elements share a set.
func ExampleUnionFind() {
	uf := ds.New[string]()

	// Elements are added implicitly on first reference; MakeSet is an
	// explicit way to introduce one as its own singleton set.
	uf.MakeSet("alice")

	// Merge the sets of the given elements.
	uf.Union("alice", "bob")
	uf.Union("carol", "dave")

	fmt.Println("alice~bob:", uf.Connected("alice", "bob"))
	fmt.Println("alice~carol:", uf.Connected("alice", "carol"))
	fmt.Println("elements:", uf.Len())
	// Output:
	// alice~bob: true
	// alice~carol: false
	// elements: 4
}

// ExampleUnionFind_Find shows that Find returns each set's canonical
// representative: two elements are in the same set exactly when their
// Find results are equal.
func ExampleUnionFind_Find() {
	uf := ds.New[int]()
	uf.Union(1, 2)
	uf.Union(2, 3)

	// 1, 2 and 3 are now one set, so they share a representative.
	sameRep := uf.Find(1) == uf.Find(3)
	fmt.Println("1 and 3 share a representative:", sameRep)
	// Output:
	// 1 and 3 share a representative: true
}

// ExampleUnionFind_Reset clears every set, returning the structure to
// the empty state so it can be reused without a fresh allocation.
func ExampleUnionFind_Reset() {
	uf := ds.New[int]()
	uf.Union(1, 2)

	uf.Reset()

	fmt.Println("elements after reset:", uf.Len())
	fmt.Println("1~2 after reset:", uf.Connected(1, 2))
	// Output:
	// elements after reset: 0
	// 1~2 after reset: false
}
