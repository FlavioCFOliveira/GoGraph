package hash_test

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
)

// ExampleIndex shows a hash index answering an exact-match property
// predicate: insert (value, NodeID) pairs keyed by a string property,
// then read back the NodeID set carrying one exact value.
func ExampleIndex() {
	// An index over a string "email domain" property.
	idx := hash.New[string]()
	idx.Insert("example.com", graph.NodeID(1))
	idx.Insert("example.org", graph.NodeID(2))
	idx.Insert("example.com", graph.NodeID(3))

	// Lookup answers "every node where domain == example.com".
	bm := idx.Lookup("example.com")
	fmt.Println("example.com nodes:", bm.ToArray())
	fmt.Println("example.com cardinality:", idx.Cardinality("example.com"))
	fmt.Println("distinct domains:", idx.DistinctValues())
	// Output:
	// example.com nodes: [1 3]
	// example.com cardinality: 2
	// distinct domains: 2
}

// ExampleIndex_Contains shows the point-membership query: Contains
// reports whether one specific NodeID carries a given value, without
// materialising the whole NodeID set.
func ExampleIndex_Contains() {
	idx := hash.New[int]()
	idx.Insert(404, graph.NodeID(7))

	fmt.Println("node 7 has 404:", idx.Contains(404, graph.NodeID(7)))
	fmt.Println("node 8 has 404:", idx.Contains(404, graph.NodeID(8)))

	// Delete removes one membership; the value disappears once its last
	// NodeID is gone.
	idx.Delete(404, graph.NodeID(7))
	fmt.Println("node 7 has 404 after delete:", idx.Contains(404, graph.NodeID(7)))
	// Output:
	// node 7 has 404: true
	// node 8 has 404: false
	// node 7 has 404 after delete: false
}
