package query_test

import (
	"fmt"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

// buildSocialGraph returns a tiny labelled property graph and its CSR
// snapshot: two :Person nodes (alice, bob) who both BOUGHT a :Product
// (widget).
func buildSocialGraph() (*lpg.Graph[string, int], *csr.CSR[int]) {
	g := lpg.New[string, int](adjlist.Config{Directed: true})
	for _, p := range []string{"alice", "bob"} {
		_ = g.AddNode(p)
		_ = g.SetNodeLabel(p, "Person")
		_ = g.SetNodeProperty(p, "name", lpg.StringValue(p))
	}
	_ = g.AddNode("widget")
	_ = g.SetNodeLabel("widget", "Product")
	_ = g.AddEdge("alice", "widget", 0)
	_ = g.AddEdge("bob", "widget", 0)
	return g, csr.BuildFromAdjList(g.AdjList())
}

// ExampleEngine_Match expresses "MATCH (n:Person) RETURN n" with the
// fluent pattern API. The label predicate seeds the working set from
// the graph's label index; Cardinality reports its size and Collect
// returns the matching user keys (order unspecified, sorted here).
func ExampleEngine_Match() {
	g, snap := buildSocialGraph()
	eng := query.New(g, snap)

	people := eng.Match().Vertex(query.WithLabel[string, int]("Person"))

	keys := people.Collect()
	sort.Strings(keys)
	fmt.Println("count:", people.Cardinality())
	fmt.Println("people:", keys)
	// Output:
	// count: 2
	// people: [alice bob]
}

// ExampleWithProperty filters a pattern by an exact property match —
// "MATCH (n) WHERE n.name = 'alice' RETURN n".
func ExampleWithProperty() {
	g, snap := buildSocialGraph()
	eng := query.New(g, snap)

	match := eng.Match().Vertex(
		query.WithProperty[string, int]("name", lpg.StringValue("alice")),
	)

	fmt.Println("count:", match.Cardinality())
	fmt.Println("keys:", match.Collect())
	// Output:
	// count: 1
	// keys: [alice]
}

// ExamplePattern_Out follows out-edges one hop — "MATCH
// (:Person)-[]->(p) RETURN p" — collapsing both buyers onto the single
// product they bought.
func ExamplePattern_Out() {
	g, snap := buildSocialGraph()
	eng := query.New(g, snap)

	bought := eng.Match().
		Vertex(query.WithLabel[string, int]("Person")).
		Out()

	fmt.Println("count:", bought.Cardinality())
	fmt.Println("products:", bought.Collect())
	// Output:
	// count: 1
	// products: [widget]
}
