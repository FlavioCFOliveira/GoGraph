package jsonl_test

import (
	"bytes"
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/io/jsonl"
	"gograph/graph/lpg"
)

// ExampleWrite shows a JSON Lines round-trip: marshal a directed,
// weighted graph to NDJSON (one node/edge record per line) with Write,
// then unmarshal it back with ReadInto and confirm the structure.
func ExampleWrite() {
	cfg := adjlist.Config{Directed: true}

	src := adjlist.New[string, int64](cfg)
	_ = src.AddEdge("a", "b", 7)
	_ = src.AddEdge("b", "c", 9)

	var buf bytes.Buffer
	if _, err := jsonl.Write(&buf, src); err != nil {
		panic(err)
	}

	dst, records, err := jsonl.ReadInto(&buf, cfg)
	if err != nil {
		panic(err)
	}

	fmt.Println("records read:", records)
	fmt.Println("order:", dst.Order())
	fmt.Println("size:", dst.Size())
	fmt.Println("b->c:", dst.HasEdge("b", "c"))
	// Output:
	// records read: 5
	// order: 3
	// size: 2
	// b->c: true
}

// ExampleWriteWithProps shows the labelled-property-graph round-trip:
// WriteWithProps emits a property record per typed property and
// ReadWithProps restores it, so a string property recovers its value.
func ExampleWriteWithProps() {
	cfg := adjlist.Config{Directed: true}

	src := lpg.New[string, int64](cfg)
	_ = src.AddEdge("alice", "bob", 1)
	_ = src.SetNodeProperty("alice", "city", lpg.StringValue("Lisbon"))

	var buf bytes.Buffer
	if _, err := jsonl.WriteWithProps(&buf, src); err != nil {
		panic(err)
	}

	dst, _, err := jsonl.ReadWithProps(&buf, cfg)
	if err != nil {
		panic(err)
	}

	got, ok := dst.GetNodeProperty("alice", "city")
	city, _ := got.String()
	fmt.Println("alice has city:", ok)
	fmt.Println("alice.city:", city)
	// Output:
	// alice has city: true
	// alice.city: Lisbon
}
