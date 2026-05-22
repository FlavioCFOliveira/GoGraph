// Example 19_pattern_query — build a labelled property graph with
// a declared schema, populate it, then run several MATCH-style
// queries combining label and property predicates plus a one-hop
// expansion.
package main

import (
	"fmt"
	"log"
	"sort"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/graph/lpg/schema"
	"gograph/graph/query"
)

func main() {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := schema.New(g.Registry(), g.PropertyKeys())
	s.RegisterLabel("Person")
	s.RegisterLabel("Admin")
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		log.Fatalf("schema.RegisterProperty: %v", err)
	}
	if _, err := s.RegisterProperty("dept", lpg.PropString); err != nil {
		log.Fatalf("schema.RegisterProperty: %v", err)
	}

	type person struct {
		id    string
		age   int64
		dept  string
		admin bool
	}
	people := []person{
		{"alice", 30, "eng", true},
		{"bob", 25, "eng", false},
		{"carol", 30, "ops", false},
		{"dave", 42, "eng", true},
		{"erin", 28, "ops", false},
	}
	for _, p := range people {
		if err := g.SetNodeLabel(p.id, "Person"); err != nil {
			log.Fatalf("SetNodeLabel: %v", err)
		}
		if p.admin {
			if err := g.SetNodeLabel(p.id, "Admin"); err != nil {
				log.Fatalf("SetNodeLabel: %v", err)
			}
		}
		if err := g.SetNodeProperty(p.id, "age", lpg.Int64Value(p.age)); err != nil {
			log.Fatalf("SetNodeProperty: %v", err)
		}
		if err := g.SetNodeProperty(p.id, "dept", lpg.StringValue(p.dept)); err != nil {
			log.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for _, e := range [][2]string{
		{"alice", "bob"},
		{"alice", "carol"},
		{"dave", "erin"},
	} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			log.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(g.AdjList())
	e := query.New(g, c)

	fmt.Println("MATCH (n:Person:Admin) RETURN n.name")
	for _, n := range e.Match().
		Vertex(query.WithLabel[string, int64]("Person"), query.WithLabel[string, int64]("Admin")).
		Collect() {
		fmt.Printf("  - %s\n", n)
	}

	fmt.Println("\nMATCH (n:Person) WHERE n.age = 30 RETURN n.name")
	for _, n := range e.Match().
		Vertex(query.WithLabel[string, int64]("Person"),
			query.WithProperty[string, int64]("age", lpg.Int64Value(30))).
		Collect() {
		fmt.Printf("  - %s\n", n)
	}

	fmt.Println("\nMATCH (n:Admin)-->(b) RETURN b.name  (one hop out)")
	out := e.Match().
		Vertex(query.WithLabel[string, int64]("Admin")).
		Out().
		Collect()
	sort.Strings(out)
	for _, n := range out {
		fmt.Printf("  - %s\n", n)
	}

	fmt.Println("\nMATCH (n:Person {dept: 'ops'}) RETURN n.name")
	for _, n := range e.Match().
		Vertex(query.WithLabel[string, int64]("Person"),
			query.WithProperty[string, int64]("dept", lpg.StringValue("ops"))).
		Collect() {
		fmt.Printf("  - %s\n", n)
	}
}
