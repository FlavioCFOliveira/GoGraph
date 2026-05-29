package dot_test

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gograph/graph/adjlist"
	"gograph/graph/io/dot"
)

// ExampleWrite renders a single directed, weighted edge as Graphviz
// DOT. The output is a digraph whose body lists "src -> dst" with the
// weight as the edge label; pipe it through `dot -Tsvg` to visualise.
func ExampleWrite() {
	g := adjlist.New[string, int64](adjlist.Config{Directed: true})
	_ = g.AddEdge("a", "b", 5)

	var buf bytes.Buffer
	if err := dot.Write(&buf, g); err != nil {
		panic(err)
	}
	fmt.Print(buf.String())
	// Output:
	// digraph G {
	//   a -> b [label="5"];
	// }
}

// ExampleWrite_multiEdge renders a small directed graph. The DOT writer
// emits one line per edge in an internal NodeID order, so the example
// sorts the body lines before printing to keep the output stable.
func ExampleWrite_multiEdge() {
	g := adjlist.New[string, int64](adjlist.Config{Directed: true})
	_ = g.AddEdge("a", "b", 1)
	_ = g.AddEdge("b", "c", 2)

	var buf bytes.Buffer
	if err := dot.Write(&buf, g); err != nil {
		panic(err)
	}

	// Collect just the indented edge lines and sort them for a
	// deterministic, layout-independent rendering.
	var edges []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "  ") {
			edges = append(edges, strings.TrimSpace(line))
		}
	}
	sort.Strings(edges)
	for _, e := range edges {
		fmt.Println(e)
	}
	// Output:
	// a -> b [label="1"];
	// b -> c [label="2"];
}
