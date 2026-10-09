package adjlist_test

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// ExampleAdjList builds a small directed weighted graph and reads back
// its order (node count), size (edge count), and edge membership. The
// Config selects the graph variant; here Directed means AddEdge inserts
// only the forward edge.
func ExampleAdjList() {
	g := adjlist.New[string, int](adjlist.Config{})

	// AddEdge auto-creates endpoint nodes; the third argument is the
	// edge weight (here an int).
	_ = g.AddEdge("a", "b", 10)
	_ = g.AddEdge("a", "c", 20)

	fmt.Println("order:", g.Order())
	fmt.Println("size:", g.Size())
	fmt.Println("a->b:", g.HasEdge("a", "b"))
	fmt.Println("b->a:", g.HasEdge("b", "a")) // directed: reverse absent
	// Output:
	// order: 3
	// size: 2
	// a->b: true
	// b->a: false
}

// ExampleAdjList_Neighbours iterates the out-neighbours of a node with
// the Go 1.23 range-over-func form, yielding each neighbour together
// with its edge weight. Iteration order is unspecified.
func ExampleAdjList_Neighbours() {
	g := adjlist.New[string, int](adjlist.Config{})
	_ = g.AddEdge("a", "b", 10)
	_ = g.AddEdge("a", "c", 20)

	for n, w := range g.Neighbours("a") {
		fmt.Printf("a -> %s (weight %d)\n", n, w)
	}
	// Unordered output:
	// a -> b (weight 10)
	// a -> c (weight 20)
}
