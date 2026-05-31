package main

import "os"

// Example pins the deterministic stdout of the GraphML round-trip
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters the serialised GraphML or DOT is caught as
// a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Ingested 2 edges from GraphML
	//
	// GraphML out:
	// <?xml version="1.0" encoding="UTF-8"?>
	// <graphml xmlns="http://graphml.graphdrawing.org/xmlns">
	//   <key id="w" for="edge" attr.name="weight" attr.type="long"></key>
	//   <graph id="G" edgedefault="directed">
	//     <node id="alice"></node>
	//     <node id="bob"></node>
	//     <node id="carol"></node>
	//     <edge source="alice" target="bob">
	//       <data key="w">7</data>
	//     </edge>
	//     <edge source="bob" target="carol">
	//       <data key="w">9</data>
	//     </edge>
	//   </graph>
	// </graphml>
	// DOT out:
	// digraph G {
	//   alice -> bob [label="7"];
	//   bob -> carol [label="9"];
	// }
}
