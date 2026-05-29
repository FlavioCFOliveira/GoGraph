package graphml_test

import (
	"bytes"
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/io/graphml"
	"gograph/graph/lpg"
)

// ExampleWrite shows a GraphML round-trip: marshal a directed, weighted
// graph to XML with Write, then unmarshal it back with ReadInto and
// confirm the structure survived.
func ExampleWrite() {
	src := adjlist.New[string, int64](adjlist.Config{Directed: true})
	_ = src.AddEdge("a", "b", 7)
	_ = src.AddEdge("b", "c", 9)

	var buf bytes.Buffer
	if err := graphml.Write(&buf, src); err != nil {
		panic(err)
	}

	dst, edges, err := graphml.ReadInto(&buf)
	if err != nil {
		panic(err)
	}

	fmt.Println("edges read:", edges)
	fmt.Println("order:", dst.Order())
	fmt.Println("size:", dst.Size())
	fmt.Println("a->b:", dst.HasEdge("a", "b"))
	// Output:
	// edges read: 2
	// order: 3
	// size: 2
	// a->b: true
}

// ExampleWriteWithProps shows the labelled-property-graph round-trip:
// WriteWithProps serialises node properties as <data> elements and
// ReadWithProps restores them, so a typed property recovers its value.
func ExampleWriteWithProps() {
	src := lpg.New[string, int64](adjlist.Config{Directed: true})
	_ = src.AddEdge("alice", "bob", 1)
	_ = src.SetNodeProperty("alice", "age", lpg.Int64Value(30))

	var buf bytes.Buffer
	if err := graphml.WriteWithProps(&buf, src); err != nil {
		panic(err)
	}

	dst, _, err := graphml.ReadWithProps(&buf)
	if err != nil {
		panic(err)
	}

	got, ok := dst.GetNodeProperty("alice", "age")
	age, _ := got.Int64()
	fmt.Println("alice has age:", ok)
	fmt.Println("alice.age:", age)
	// Output:
	// alice has age: true
	// alice.age: 30
}
