package index_test

import (
	"errors"
	"fmt"

	"gograph/graph"
	"gograph/graph/index"
	"gograph/graph/index/label"
)

// ExampleManager shows the Manager lifecycle: register a concrete index
// under a name, list and count the registered indexes, and reject a
// duplicate registration with ErrIndexExists.
func ExampleManager() {
	m := index.NewManager()

	if err := m.CreateIndex("by_label", label.NewNodeIndex()); err != nil {
		fmt.Println("unexpected:", err)
	}

	// Re-registering the same name is rejected.
	err := m.CreateIndex("by_label", label.NewNodeIndex())
	fmt.Println("duplicate is ErrIndexExists:", errors.Is(err, index.ErrIndexExists))

	fmt.Println("count:", m.Count())
	fmt.Println("names:", m.ListIndexes())
	// Output:
	// duplicate is ErrIndexExists: true
	// count: 1
	// names: [by_label]
}

// ExampleManager_Apply shows the Manager fanning a change out to every
// registered subscriber. The label index observes OpAddNodeLabel events
// and can then be queried back through GetIndex for the NodeIDs that
// carry a given label.
func ExampleManager_Apply() {
	const labelPerson = uint32(7)

	m := index.NewManager()
	_ = m.CreateIndex("node_labels", label.NewNodeIndex())

	// A mutation observed by the owning graph is fanned out to every
	// subscriber. Here two nodes acquire the Person label.
	m.Apply(index.Change{Op: index.OpAddNodeLabel, Node: graph.NodeID(1), Label: labelPerson})
	m.Apply(index.Change{Op: index.OpAddNodeLabel, Node: graph.NodeID(4), Label: labelPerson})

	// Recover the concrete index to run a query.
	sub, _ := m.GetIndex("node_labels")
	idx := sub.(*label.Index)

	fmt.Println("kind:", idx.Kind())
	fmt.Println("Person count:", idx.Count(labelPerson))
	fmt.Println("Person members:", idx.Scan(labelPerson))
	// Output:
	// kind: label
	// Person count: 2
	// Person members: [1 4]
}
