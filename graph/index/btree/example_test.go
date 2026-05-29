package btree_test

import (
	"fmt"

	"gograph/graph"
	"gograph/graph/index/btree"
)

// ExampleIndex shows an order-preserving property index keyed by an
// ordered value type: insert (value, NodeID) pairs, then read back the
// NodeIDs carrying one exact value and the half-open count of distinct
// values.
func ExampleIndex() {
	// An index over an integer "age" property.
	idx := btree.New[int]()
	idx.Insert(30, graph.NodeID(1))
	idx.Insert(25, graph.NodeID(2))
	idx.Insert(30, graph.NodeID(3)) // same value, different node

	// Lookup returns the NodeID set carrying exactly age == 30.
	bm := idx.Lookup(30)
	fmt.Println("age==30 nodes:", bm.ToArray())
	fmt.Println("age==30 cardinality:", idx.Cardinality(30))
	fmt.Println("distinct ages:", idx.DistinctValues())
	// Output:
	// age==30 nodes: [1 3]
	// age==30 cardinality: 2
	// distinct ages: 2
}

// ExampleIndex_Range shows an inclusive range predicate [lo, hi]: Range
// returns every NodeID whose value satisfies lo <= v <= hi, and
// RangeFirst returns the smallest matching value together with one of
// its NodeIDs (the order-preserving property the index exists for).
func ExampleIndex_Range() {
	idx := btree.New[int]()
	if err := idx.BulkLoad(
		[]int{10, 20, 30, 40},
		[]graph.NodeID{1, 2, 3, 4},
	); err != nil {
		fmt.Println("bulk load:", err)
		return
	}

	// Range is inclusive on both ends: [20, 40] selects 20, 30 and 40.
	bm := idx.Range(20, 40)
	fmt.Println("nodes in [20,40]:", bm.ToArray())

	v, node, ok := idx.RangeFirst(20, 40)
	fmt.Printf("first in [20,40]: value=%d node=%d ok=%t\n", v, node, ok)
	// Output:
	// nodes in [20,40]: [2 3 4]
	// first in [20,40]: value=20 node=2 ok=true
}
