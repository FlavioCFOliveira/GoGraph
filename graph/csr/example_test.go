package csr_test

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// ExampleBuildFromAdjList freezes a mutable adjacency list into an
// immutable CSR snapshot suitable for lock-free analytical reads, and
// reads back its order (node count) and size (edge count).
func ExampleBuildFromAdjList() {
	g := adjlist.New[string, int](adjlist.Config{Directed: true})
	_ = g.AddEdge("a", "b", 1)
	_ = g.AddEdge("b", "c", 1)

	snap := csr.BuildFromAdjList(g)

	fmt.Println("order:", snap.Order())
	fmt.Println("size:", snap.Size())
	// Output:
	// order: 3
	// size: 2
}

// ExampleCSR_LiveCount counts the nodes that participate in the
// snapshot — those with at least one incident edge. LiveCount and the
// length of LiveNodes always agree, and LiveMask is the underlying
// per-NodeID boolean view they are both derived from.
func ExampleCSR_LiveCount() {
	g := adjlist.New[string, int](adjlist.Config{Directed: true})
	_ = g.AddEdge("a", "b", 1)
	_ = g.AddEdge("b", "c", 1)

	snap := csr.BuildFromAdjList(g)

	fmt.Println("live count:", snap.LiveCount())
	fmt.Println("live nodes len:", len(snap.LiveNodes()))
	fmt.Println("count == len(nodes):", snap.LiveCount() == len(snap.LiveNodes()))

	// LiveMask reports liveness per NodeID; the number of true entries
	// equals LiveCount.
	var liveInMask int
	for _, live := range snap.LiveMask() {
		if live {
			liveInMask++
		}
	}
	fmt.Println("count == mask trues:", snap.LiveCount() == liveInMask)
	// Output:
	// live count: 3
	// live nodes len: 3
	// count == len(nodes): true
	// count == mask trues: true
}
