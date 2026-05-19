// Example 02_property_graph — build a small labelled property graph,
// declare a schema, attach labels and typed properties, then run a
// MATCH-style indexed query.
package main

import (
	"fmt"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/graph/lpg/schema"
	"gograph/graph/query"
)

func main() {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	// Optional schema declarations enable Validate() against the
	// expected kinds before the values land in the live graph.
	s := schema.New(g.Registry(), g.PropertyKeys())
	s.RegisterLabel("Person")
	s.RegisterLabel("Admin")
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		panic(err)
	}
	if _, err := s.RegisterProperty("name", lpg.PropString); err != nil {
		panic(err)
	}

	people := []struct {
		id   string
		age  int64
		isAd bool
	}{
		{"alice", 30, true},
		{"bob", 25, false},
		{"charlie", 30, false},
		{"dave", 42, true},
		{"erin", 28, false},
	}
	for _, p := range people {
		g.SetNodeLabel(p.id, "Person")
		if p.isAd {
			g.SetNodeLabel(p.id, "Admin")
		}
		g.SetNodeProperty(p.id, "age", lpg.Int64Value(p.age))
		g.SetNodeProperty(p.id, "name", lpg.StringValue(p.id))
	}

	// One edge each for the demo.
	g.AddEdge("alice", "bob", 1)
	g.AddEdge("alice", "charlie", 1)
	g.AddEdge("bob", "dave", 1)
	g.SetEdgeLabel("alice", "bob", "KNOWS")
	g.SetEdgeLabel("alice", "charlie", "KNOWS")

	c := csr.BuildFromAdjList(g.AdjList())
	e := query.New(g, c)

	fmt.Println("All Admins:")
	for _, n := range e.Match().Vertex(query.WithLabel[string, int64]("Admin")).Collect() {
		fmt.Printf("  %s\n", n)
	}

	fmt.Println("Persons aged 30:")
	for _, n := range e.Match().Vertex(
		query.WithLabel[string, int64]("Person"),
		query.WithProperty[string, int64]("age", lpg.Int64Value(30)),
	).Collect() {
		fmt.Printf("  %s\n", n)
	}

	fmt.Println("One-hop out from Admins:")
	for _, n := range e.Match().
		Vertex(query.WithLabel[string, int64]("Admin")).
		Out().
		Collect() {
		fmt.Printf("  %s\n", n)
	}
}
