// Example 07_graphml_roundtrip — reads a GraphML document, prints the
// number of ingested edges, then writes the graph back out to both
// GraphML and DOT.
//
// Sample output: run `go run ./examples/07_graphml_roundtrip` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"gograph/graph/io/dot"
	"gograph/graph/io/graphml"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run reads a hard-coded GraphML document into an adjacency list,
// reports the edge count, then re-serialises the graph to GraphML and
// DOT. All output goes to w so a test can capture and assert it; run
// returns wrapped errors rather than terminating the process.
func run(w io.Writer) error {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<graphml xmlns="http://graphml.graphdrawing.org/xmlns">
  <key id="w" for="edge" attr.name="weight" attr.type="long"/>
  <graph id="G" edgedefault="directed">
    <node id="alice"/><node id="bob"/><node id="carol"/>
    <edge source="alice" target="bob"><data key="w">7</data></edge>
    <edge source="bob" target="carol"><data key="w">9</data></edge>
  </graph>
</graphml>`

	g, n, err := graphml.ReadInto(strings.NewReader(doc))
	if err != nil {
		return fmt.Errorf("graphml.ReadInto: %w", err)
	}
	fmt.Fprintf(w, "Ingested %d edges from GraphML\n\n", n)

	var buf bytes.Buffer
	if err := graphml.Write(&buf, g); err != nil {
		return fmt.Errorf("graphml.Write: %w", err)
	}
	fmt.Fprintln(w, "GraphML out:")
	fmt.Fprintln(w, buf.String())

	fmt.Fprintln(w, "DOT out:")
	if err := dot.Write(w, g); err != nil {
		return fmt.Errorf("dot.Write: %w", err)
	}
	return nil
}
