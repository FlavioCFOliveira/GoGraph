// Example 07_graphml_roundtrip — reads a GraphML document, prints
// the edges, then writes the graph back to GraphML and DOT.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gograph/graph/io/dot"
	"gograph/graph/io/graphml"
)

func main() {
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
		panic(err)
	}
	fmt.Printf("Ingested %d edges from GraphML\n\n", n)

	var buf bytes.Buffer
	_ = graphml.Write(&buf, g)
	fmt.Println("GraphML out:")
	fmt.Println(buf.String())

	fmt.Println("DOT out:")
	_ = dot.Write(os.Stdout, g)
}
