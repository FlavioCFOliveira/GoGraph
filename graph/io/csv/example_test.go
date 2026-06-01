package csv_test

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// ExampleReadInto parses a CSV edge list (src,dst[,weight] per row,
// '#' comment lines skipped) into a mutable adjacency list and reports
// the resulting order, size and one edge.
func ExampleReadInto() {
	const data = "# a tiny directed triangle\n" +
		"a,b,1\n" +
		"b,c,2\n" +
		"c,a,3\n"

	opts := csv.DefaultOptions()
	opts.Directed = true

	g, rows, err := csv.ReadInto(strings.NewReader(data), opts)
	if err != nil {
		panic(err)
	}

	fmt.Println("rows:", rows)
	fmt.Println("order:", g.Order())
	fmt.Println("size:", g.Size())
	fmt.Println("a->b:", g.HasEdge("a", "b"))
	// Output:
	// rows: 3
	// order: 3
	// size: 3
	// a->b: true
}

// ExampleWrite shows a CSV round-trip: build a graph, Write it to a
// buffer, then ReadInto a fresh graph and confirm the edges survived.
// The serialised row order follows internal NodeID assignment, so the
// example asserts on edge presence rather than on exact bytes.
func ExampleWrite() {
	src := adjlist.New[string, int64](adjlist.Config{Directed: true})
	_ = src.AddEdge("a", "b", 1)
	_ = src.AddEdge("a", "c", 2)
	_ = src.AddEdge("b", "c", 3)

	var buf bytes.Buffer
	rows, err := csv.Write(&buf, src, csv.DefaultOptions())
	if err != nil {
		panic(err)
	}

	readOpts := csv.DefaultOptions()
	readOpts.Directed = true
	dst, _, err := csv.ReadInto(&buf, readOpts)
	if err != nil {
		panic(err)
	}

	fmt.Println("rows written:", rows)
	fmt.Println("edges survive:", dst.HasEdge("a", "b") && dst.HasEdge("a", "c") && dst.HasEdge("b", "c"))
	// Output:
	// rows written: 3
	// edges survive: true
}
