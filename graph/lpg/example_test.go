package lpg_test

import (
	"fmt"
	"sort"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// ExampleGraph builds a small labelled property graph: nodes carry
// labels (their classes) and typed properties, and edges connect them.
// The Config is forwarded to the underlying adjacency list, so Directed
// selects a directed graph here.
func ExampleGraph() {
	g := lpg.New[string, int](adjlist.Config{Directed: true})

	// Create two nodes and tag each with a label.
	_ = g.AddNode("alice")
	_ = g.AddNode("bob")
	_ = g.SetNodeLabel("alice", "Person")
	_ = g.SetNodeLabel("bob", "Person")

	// Attach typed properties via the PropertyValue constructors.
	_ = g.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	_ = g.SetNodeProperty("alice", "age", lpg.Int64Value(30))

	// Connect them with a labelled edge.
	_ = g.AddEdge("alice", "bob", 0)
	g.SetEdgeLabel("alice", "bob", "KNOWS")

	name, _ := g.GetNodeProperty("alice", "name")
	nameStr, _ := name.String()
	age, _ := g.GetNodeProperty("alice", "age")
	ageInt, _ := age.Int64()

	fmt.Println("alice is Person:", g.HasNodeLabel("alice", "Person"))
	fmt.Println("alice.name:", nameStr)
	fmt.Println("alice.age:", ageInt)
	fmt.Println("alice KNOWS bob:", g.HasEdgeLabel("alice", "bob", "KNOWS"))
	// Output:
	// alice is Person: true
	// alice.name: Alice
	// alice.age: 30
	// alice KNOWS bob: true
}

// ExampleGraph_NodeLabels shows that a node may carry several labels at
// once. NodeLabels returns them in an unspecified order, so callers
// that need a stable order sort the result.
func ExampleGraph_NodeLabels() {
	g := lpg.New[string, int](adjlist.Config{Directed: true})
	_ = g.AddNode("alice")
	_ = g.SetNodeLabel("alice", "Person")
	_ = g.SetNodeLabel("alice", "Employee")

	labels := g.NodeLabels("alice")
	sort.Strings(labels)

	fmt.Println(labels)
	// Output:
	// [Employee Person]
}
