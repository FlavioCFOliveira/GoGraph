package lpg_test

import (
	"fmt"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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

// ExampleGraph_RemoveNode shows that node deletion is a tombstone — the
// NodeID slot is permanent, so the node is excluded from the live count
// rather than reusing its id — and that re-creating the same key revives
// the node under the SAME stable NodeID. This is what makes a
// delete-then-recreate cycle yield exactly one live node, and (once the
// tombstone set is persisted) survive a store reopen.
func ExampleGraph_RemoveNode() {
	g := lpg.New[string, int](adjlist.Config{Directed: true})
	_ = g.SetNodeLabel("auth", "Spec")
	id, _ := g.AdjList().Mapper().Lookup("auth")

	g.RemoveNode("auth")
	fmt.Println("tombstoned:", g.IsTombstoned(id), "live:", g.LiveOrder())

	// Re-create the same key: revived under the same NodeID.
	_ = g.AddNode("auth")
	id2, _ := g.AdjList().Mapper().Lookup("auth")
	fmt.Println("revived:", !g.IsTombstoned(id), "sameID:", id == id2, "live:", g.LiveOrder())
	// Output:
	// tombstoned: true live: 0
	// revived: true sameID: true live: 1
}

// ExampleGraph_RemoveEdge shows that deleting an edge clears its per-pair
// label/property surface once the endpoint pair is fully disconnected, so
// re-creating an edge between the same endpoints does not resurrect the
// removed relationship's type.
func ExampleGraph_RemoveEdge() {
	g := lpg.New[string, int](adjlist.Config{Directed: true})
	_ = g.AddEdge("alice", "bob", 0)
	g.SetEdgeLabel("alice", "bob", "KNOWS")
	fmt.Println("before delete:", g.HasEdgeLabel("alice", "bob", "KNOWS"))

	g.RemoveEdge("alice", "bob")
	_ = g.AddEdge("alice", "bob", 0) // re-create the same pair
	fmt.Println("after re-create:", g.HasEdgeLabel("alice", "bob", "KNOWS"))
	// Output:
	// before delete: true
	// after re-create: false
}

// ExampleGraph_View shows the recommended way to read a graph that may be
// mutated concurrently: wrap a multi-op transaction in [lpg.Graph.ApplyAtomically]
// and the reads that must observe it whole in [lpg.Graph.View]. Per-operation
// accessors are always individually atomic, but only View guarantees a reader
// never sees a multi-op transaction half-applied — here, the edge without its
// endpoint labels. Inside View the cross-substructure invariant "the edge exists
// ⇔ both endpoint labels exist" always holds.
func ExampleGraph_View() {
	g := lpg.New[string, int](adjlist.Config{Directed: true})

	// One transaction establishes a cross-substructure invariant: the edge
	// alice→bob and both endpoint :Hot labels become visible together.
	_ = g.ApplyAtomically(func() error {
		_ = g.AddEdge("alice", "bob", 0)
		_ = g.SetNodeLabel("alice", "Hot")
		_ = g.SetNodeLabel("bob", "Hot")
		return nil
	})

	// A consistent read pins the whole transaction for its duration.
	g.View(func() {
		edge := g.AdjList().HasEdge("alice", "bob")
		srcHot := g.HasNodeLabel("alice", "Hot")
		dstHot := g.HasNodeLabel("bob", "Hot")
		// The invariant "edge ⇔ src:Hot ⇔ dst:Hot": all three observations
		// agree, so the set of distinct values has size one.
		consistent := edge == srcHot && srcHot == dstHot
		fmt.Println("edge:", edge, "src:Hot:", srcHot, "dst:Hot:", dstHot)
		fmt.Println("invariant holds:", consistent)
	})
	// Output:
	// edge: true src:Hot: true dst:Hot: true
	// invariant holds: true
}
