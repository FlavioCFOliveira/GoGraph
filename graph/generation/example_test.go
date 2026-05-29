package generation_test

import (
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/generation"
)

// snapshot is a helper that freezes a directed graph with the given
// edges into a CSR view, the immutable unit a Publisher hands out.
func snapshot(edges [][2]string) *csr.CSR[int] {
	g := adjlist.New[string, int](adjlist.Config{Directed: true})
	for _, e := range edges {
		_ = g.AddEdge(e[0], e[1], 1)
	}
	return csr.BuildFromAdjList(g)
}

// ExamplePublisher shows the read side of the MVCC-style snapshot
// pattern: a reader Acquires the current generation, uses its immutable
// CSR, and Releases it. The refcount tracks outstanding readers so an
// old generation is reclaimed only once every reader has let go.
func ExamplePublisher() {
	pub := generation.New(snapshot([][2]string{{"a", "b"}}))
	defer pub.Close()

	gen := pub.Acquire()
	fmt.Println("edges:", gen.CSR().Size())
	fmt.Println("readers while held:", gen.Refcount())

	pub.Release(gen)
	fmt.Println("readers after release:", pub.Current().Refcount())
	// Output:
	// edges: 1
	// readers while held: 1
	// readers after release: 0
}

// ExamplePublisher_Publish shows the write side: Publish installs a new
// immutable generation and makes it the one future Acquire calls see,
// without disturbing readers still holding the previous generation.
func ExamplePublisher_Publish() {
	pub := generation.New(snapshot([][2]string{{"a", "b"}}))
	defer pub.Close()

	fmt.Println("before:", pub.Current().CSR().Size())

	next, err := pub.Publish(snapshot([][2]string{{"a", "b"}, {"b", "c"}}))
	if err != nil {
		panic(err) // a healthy (non-closed) publisher never errors here
	}

	fmt.Println("published:", next.CSR().Size())
	fmt.Println("current:", pub.Current().CSR().Size())
	// Output:
	// before: 1
	// published: 2
	// current: 2
}
