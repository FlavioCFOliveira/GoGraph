// Example 19_pattern_query — build a labelled property graph with
// a declared schema, populate it, then run several MATCH-style
// queries combining label and property predicates plus a one-hop
// expansion.
//
// Each matched node is printed together with the property values that
// make its predicate meaningful (its age and department), so the
// reader can see *why* the node matched rather than just its key.
//
// Sample output: run `go run ./examples/19_pattern_query` and capture the
// stdout — the output is deterministic for the inputs hard-coded
// above and serves as the regression baseline a future change should
// preserve. Every query result is sorted by node key so the output is
// byte-stable regardless of internal iteration order.
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

// run builds the labelled property graph, runs the pattern queries, and
// writes the report to w. All output goes to w so a test can capture
// and assert it; run returns wrapped errors rather than terminating the
// process.
func run(w io.Writer) error {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := schema.New(g.Registry(), g.PropertyKeys())
	s.RegisterLabel("Person")
	s.RegisterLabel("Admin")
	if _, err := s.RegisterProperty("age", lpg.PropInt64); err != nil {
		return fmt.Errorf("schema.RegisterProperty age: %w", err)
	}
	if _, err := s.RegisterProperty("dept", lpg.PropString); err != nil {
		return fmt.Errorf("schema.RegisterProperty dept: %w", err)
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
			return fmt.Errorf("SetNodeLabel Person %q: %w", p.id, err)
		}
		if p.admin {
			if err := g.SetNodeLabel(p.id, "Admin"); err != nil {
				return fmt.Errorf("SetNodeLabel Admin %q: %w", p.id, err)
			}
		}
		if err := g.SetNodeProperty(p.id, "age", lpg.Int64Value(p.age)); err != nil {
			return fmt.Errorf("SetNodeProperty age %q: %w", p.id, err)
		}
		if err := g.SetNodeProperty(p.id, "dept", lpg.StringValue(p.dept)); err != nil {
			return fmt.Errorf("SetNodeProperty dept %q: %w", p.id, err)
		}
	}
	for _, edge := range [][2]string{
		{"alice", "bob"},
		{"alice", "carol"},
		{"dave", "erin"},
	} {
		if err := g.AddEdge(edge[0], edge[1], 1); err != nil {
			return fmt.Errorf("AddEdge %s->%s: %w", edge[0], edge[1], err)
		}
	}
	c := csr.BuildFromAdjList(g.AdjList())
	e := query.New(g, c)

	fmt.Fprintln(w, "MATCH (n:Person:Admin) RETURN n.name, n.age, n.dept")
	admins := e.Match().
		Vertex(query.WithLabel[string, int64]("Person"), query.WithLabel[string, int64]("Admin")).
		Collect()
	if err := printNodes(w, g, admins); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nMATCH (n:Person) WHERE n.age = 30 RETURN n.name, n.age, n.dept")
	aged := e.Match().
		Vertex(query.WithLabel[string, int64]("Person"),
			query.WithProperty[string, int64]("age", lpg.Int64Value(30))).
		Collect()
	if err := printNodes(w, g, aged); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nMATCH (n:Admin)-->(b) RETURN b.name, b.age, b.dept  (one hop out)")
	out := e.Match().
		Vertex(query.WithLabel[string, int64]("Admin")).
		Out().
		Collect()
	if err := printNodes(w, g, out); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nMATCH (n:Person {dept: 'ops'}) RETURN n.name, n.age, n.dept")
	ops := e.Match().
		Vertex(query.WithLabel[string, int64]("Person"),
			query.WithProperty[string, int64]("dept", lpg.StringValue("ops"))).
		Collect()
	return printNodes(w, g, ops)
}

// printNodes sorts the matched node keys for deterministic output and
// prints each one with the age and dept property values that explain
// why it matched. It returns an error if a matched node is missing a
// property it is expected to carry, which would indicate the schema and
// the populated data have drifted apart.
func printNodes(w io.Writer, g *lpg.Graph[string, int64], keys []string) error {
	sort.Strings(keys)
	for _, key := range keys {
		age, ok := g.GetNodeProperty(key, "age")
		if !ok {
			return fmt.Errorf("node %q missing property age", key)
		}
		ageVal, ok := age.Int64()
		if !ok {
			return fmt.Errorf("node %q property age is not an int64", key)
		}
		dept, ok := g.GetNodeProperty(key, "dept")
		if !ok {
			return fmt.Errorf("node %q missing property dept", key)
		}
		deptVal, ok := dept.String()
		if !ok {
			return fmt.Errorf("node %q property dept is not a string", key)
		}
		fmt.Fprintf(w, "  - %-5s  age=%-2d  dept=%s\n", key, ageVal, deptVal)
	}
	return nil
}
