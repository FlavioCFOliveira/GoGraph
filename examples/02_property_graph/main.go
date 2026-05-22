// Example 02_property_graph — build a small labelled property graph,
// declare a schema, attach labels and typed properties, then run a
// MATCH-style indexed query.
//
// Sample output: run `go run ./examples/02_property_graph` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve.
package main

import (
	"fmt"
	"log"

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
		log.Fatalf("schema.RegisterProperty: %v", err)
	}
	if _, err := s.RegisterProperty("name", lpg.PropString); err != nil {
		log.Fatalf("schema.RegisterProperty: %v", err)
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
		if err := g.SetNodeLabel(p.id, "Person"); err != nil {
			log.Fatalf("SetNodeLabel: %v", err)
		}
		if p.isAd {
			if err := g.SetNodeLabel(p.id, "Admin"); err != nil {
				log.Fatalf("SetNodeLabel: %v", err)
			}
		}
		if err := g.SetNodeProperty(p.id, "age", lpg.Int64Value(p.age)); err != nil {
			log.Fatalf("SetNodeProperty: %v", err)
		}
		if err := g.SetNodeProperty(p.id, "name", lpg.StringValue(p.id)); err != nil {
			log.Fatalf("SetNodeProperty: %v", err)
		}
	}

	// One edge each for the demo.
	if err := g.AddEdge("alice", "bob", 1); err != nil {
		log.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("alice", "charlie", 1); err != nil {
		log.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("bob", "dave", 1); err != nil {
		log.Fatalf("AddEdge: %v", err)
	}
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
