package flow_test

import (
	"fmt"

	"gograph/search/flow"
)

// ExampleMaxFlow computes the maximum s-t flow on the classic CLRS
// network (Cormen et al., Fig. 26.1). Nodes are plain int indices in
// [0, N); the maximum flow from source 0 to sink 5 is 23.
func ExampleMaxFlow() {
	g := flow.NewNetwork(6)
	g.AddEdge(0, 1, 16)
	g.AddEdge(0, 2, 13)
	g.AddEdge(1, 2, 10)
	g.AddEdge(2, 1, 4)
	g.AddEdge(1, 3, 12)
	g.AddEdge(2, 4, 14)
	g.AddEdge(3, 2, 9)
	g.AddEdge(3, 5, 20)
	g.AddEdge(4, 3, 7)
	g.AddEdge(4, 5, 4)

	fmt.Println("max flow:", flow.MaxFlow(g, 0, 5))
	// Output:
	// max flow: 23
}

// ExampleStoerWagner finds the global minimum cut of an undirected
// weighted graph supplied as a dense, symmetric n*n weight matrix in
// row-major order. Two pairs {0,1} and {2,3} are tightly bound
// (weight 3) and joined by a single bridge of weight 1, so the global
// minimum cut severs that bridge for a total weight of 1.
func ExampleStoerWagner() {
	const n = 4
	w := make([]int, n*n)
	set := func(i, j, v int) { w[i*n+j], w[j*n+i] = v, v }
	set(0, 1, 3) // dense pair {0,1}
	set(2, 3, 3) // dense pair {2,3}
	set(1, 2, 1) // the bridge between them

	res := flow.StoerWagner(w, n)
	fmt.Println("min-cut weight:", res.Weight)
	fmt.Println("all nodes partitioned:", len(res.A)+len(res.B) == n)
	// Output:
	// min-cut weight: 1
	// all nodes partitioned: true
}
