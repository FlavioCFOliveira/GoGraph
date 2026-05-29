package label_test

import (
	"fmt"

	"gograph/graph"
	"gograph/graph/index/label"
)

// Interned label identifiers used by the examples below. A real graph
// obtains these from its lpg.LabelID registry; here they are constants.
const (
	labelPerson = uint32(1)
	labelActive = uint32(2)
)

// ExampleIndex shows membership queries on the label bitmap index: add
// NodeIDs under a label, test single-node membership with Has, count
// the carriers, and Scan the full member set in ascending NodeID order.
func ExampleIndex() {
	idx := label.NewNodeIndex()
	idx.Add(labelPerson, graph.NodeID(1))
	idx.Add(labelPerson, graph.NodeID(2))
	idx.Add(labelPerson, graph.NodeID(3))

	fmt.Println("node 2 is Person:", idx.Has(labelPerson, graph.NodeID(2)))
	fmt.Println("node 9 is Person:", idx.Has(labelPerson, graph.NodeID(9)))
	fmt.Println("Person count:", idx.Count(labelPerson))
	fmt.Println("Person members:", idx.Scan(labelPerson))
	// Output:
	// node 2 is Person: true
	// node 9 is Person: false
	// Person count: 3
	// Person members: [1 2 3]
}

// ExampleIndex_Intersect shows compound label queries via bitmap set
// operations. Intersect answers "every node carrying all of these
// labels"; Union answers "every node carrying any of them".
func ExampleIndex_Intersect() {
	idx := label.NewNodeIndex()
	for _, n := range []graph.NodeID{1, 2, 3} {
		idx.Add(labelPerson, n)
	}
	for _, n := range []graph.NodeID{2, 3, 4} {
		idx.Add(labelActive, n)
	}

	both := idx.Intersect(labelPerson, labelActive)
	either := idx.Union(labelPerson, labelActive)

	fmt.Println("Person AND Active:", both.ToArray())
	fmt.Println("Person OR Active:", either.ToArray())
	// Output:
	// Person AND Active: [2 3]
	// Person OR Active: [1 2 3 4]
}
