// Example 02_property_graph — build a small labelled property graph,
// declare a schema, attach labels and typed properties, then run a
// MATCH-style indexed query and read the typed properties back.
//
// For each matched node the report prints not only the node key but
// also the typed properties (name, age) attached to it, so the example
// demonstrates property RETRIEVAL through lpg.Graph.GetNodeProperty in
// addition to the label/property-indexed MATCH.
//
// Sample output: run `go run ./examples/02_property_graph` and capture
// the stdout — the output is deterministic for the inputs hard-coded
// above (every result group is sorted) and serves as the regression
// baseline a future change should preserve.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the labelled property graph, declares its schema, runs the
// indexed MATCH queries, and writes the report to w. Every byte goes to
// w so a test can capture and assert it; run returns wrapped errors
// rather than terminating the process. Each result group is sorted so
// the output is fully deterministic.
func run(w io.Writer) error {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	// Optional schema declarations enable Validate() against the
	// expected kinds before the values land in the live graph.
	s := schema.New(g.Registry(), g.PropertyKeys())
	s.RegisterLabel("Person")
	s.RegisterLabel("Admin")
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		return fmt.Errorf("schema.RegisterProperty age: %w", err)
	}
	if _, err := s.RegisterProperty("name", lpg.PropString); err != nil {
		return fmt.Errorf("schema.RegisterProperty name: %w", err)
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
			return fmt.Errorf("SetNodeLabel Person on %s: %w", p.id, err)
		}
		if p.isAd {
			if err := g.SetNodeLabel(p.id, "Admin"); err != nil {
				return fmt.Errorf("SetNodeLabel Admin on %s: %w", p.id, err)
			}
		}
		if err := g.SetNodeProperty(p.id, "age", lpg.Int64Value(p.age)); err != nil {
			return fmt.Errorf("SetNodeProperty age on %s: %w", p.id, err)
		}
		if err := g.SetNodeProperty(p.id, "name", lpg.StringValue(p.id)); err != nil {
			return fmt.Errorf("SetNodeProperty name on %s: %w", p.id, err)
		}
	}

	// One edge each for the demo.
	if err := g.AddEdge("alice", "bob", 1); err != nil {
		return fmt.Errorf("AddEdge alice->bob: %w", err)
	}
	if err := g.AddEdge("alice", "charlie", 1); err != nil {
		return fmt.Errorf("AddEdge alice->charlie: %w", err)
	}
	if err := g.AddEdge("bob", "dave", 1); err != nil {
		return fmt.Errorf("AddEdge bob->dave: %w", err)
	}
	g.SetEdgeLabel("alice", "bob", "KNOWS")
	g.SetEdgeLabel("alice", "charlie", "KNOWS")

	c := csr.BuildFromAdjList(g.AdjList())
	e := query.New(g, c)

	fmt.Fprintln(w, "All Admins:")
	admins := e.Match().Vertex(query.WithLabel[string, int64]("Admin")).Collect()
	if err := printNodes(w, g, admins); err != nil {
		return err
	}

	fmt.Fprintln(w, "Persons aged 30:")
	aged30 := e.Match().Vertex(
		query.WithLabel[string, int64]("Person"),
		query.WithProperty[string, int64]("age", lpg.Int64Value(30)),
	).Collect()
	if err := printNodes(w, g, aged30); err != nil {
		return err
	}

	fmt.Fprintln(w, "One-hop out from Admins:")
	oneHop := e.Match().
		Vertex(query.WithLabel[string, int64]("Admin")).
		Out().
		Collect()
	if err := printNodes(w, g, oneHop); err != nil {
		return err
	}

	return nil
}

// printNodes sorts the matched node keys for deterministic output and,
// for each, fetches and prints its typed name and age properties via
// lpg.Graph.GetNodeProperty. This is the property-retrieval half of the
// example: the MATCH locates the nodes, and GetNodeProperty reads the
// values back out.
func printNodes(w io.Writer, g *lpg.Graph[string, int64], keys []string) error {
	sort.Strings(keys)
	for _, key := range keys {
		name, err := stringProp(g, key, "name")
		if err != nil {
			return err
		}
		age, err := int64Prop(g, key, "age")
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-7s  name=%-7s  age=%d\n", key, name, age)
	}
	return nil
}

// stringProp reads a string-typed property off node key, returning an
// error if the property is missing or not a string — either of which
// would mean the graph does not match the schema this example declares.
func stringProp(g *lpg.Graph[string, int64], key, prop string) (string, error) {
	pv, ok := g.GetNodeProperty(key, prop)
	if !ok {
		return "", fmt.Errorf("node %q has no %q property", key, prop)
	}
	v, ok := pv.String()
	if !ok {
		return "", fmt.Errorf("property %q on node %q is not a string", prop, key)
	}
	return v, nil
}

// int64Prop reads an int64-typed property off node key, returning an
// error if the property is missing or not an int64.
func int64Prop(g *lpg.Graph[string, int64], key, prop string) (int64, error) {
	pv, ok := g.GetNodeProperty(key, prop)
	if !ok {
		return 0, fmt.Errorf("node %q has no %q property", key, prop)
	}
	v, ok := pv.Int64()
	if !ok {
		return 0, fmt.Errorf("property %q on node %q is not an int64", prop, key)
	}
	return v, nil
}
