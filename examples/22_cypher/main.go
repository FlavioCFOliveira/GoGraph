// Example 22_cypher — the GoGraph Cypher engine, the module's flagship
// (100% openCypher TCK compliant at the execution level).
//
// It builds a small social graph with the property-graph API and then
// queries it with five Cypher idioms: a label scan with projection and
// ORDER BY, a WHERE filter, a relationship pattern, and a CREATE inside
// a write transaction. Every value is read back from the result record
// and printed in human-readable form — names and ages, never raw node
// IDs.
//
// Every query that can return more than one row carries an ORDER BY, so
// the output is fully deterministic for the hard-coded inputs and serves
// as the regression baseline a future change should preserve. Run
// `go run ./examples/22_cypher` to capture the stdout.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds the social graph, runs the Cypher queries, and writes the
// report to w. All output goes to w so a test can capture and assert it;
// run returns wrapped errors rather than terminating the process.
func run(ctx context.Context, w io.Writer) error {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := buildGraph(g); err != nil {
		return fmt.Errorf("build graph: %w", err)
	}

	eng := cypher.NewEngine(g)

	if err := matchPersonNames(ctx, eng, w); err != nil {
		return err
	}
	if err := matchOlderThan(ctx, eng, w, 25); err != nil {
		return err
	}
	if err := matchKnows(ctx, eng, w); err != nil {
		return err
	}
	if err := createGuest(ctx, eng, g, w); err != nil {
		return err
	}
	return nil
}

// buildGraph populates g with 5 Person nodes and 5 directed
// relationships (4 KNOWS, 1 FRIENDS). Each Person carries name and age
// properties.
func buildGraph(g *lpg.Graph[string, float64]) error {
	type person struct {
		name string
		age  int64
	}
	for _, p := range []person{
		{"Alice", 30}, {"Bob", 25}, {"Carol", 35}, {"Dave", 28}, {"Eve", 22},
	} {
		if err := g.AddNode(p.name); err != nil {
			return fmt.Errorf("AddNode %q: %w", p.name, err)
		}
		if err := g.SetNodeLabel(p.name, "Person"); err != nil {
			return fmt.Errorf("SetNodeLabel %q: %w", p.name, err)
		}
		if err := g.SetNodeProperty(p.name, "name", lpg.StringValue(p.name)); err != nil {
			return fmt.Errorf("SetNodeProperty name %q: %w", p.name, err)
		}
		if err := g.SetNodeProperty(p.name, "age", lpg.Int64Value(p.age)); err != nil {
			return fmt.Errorf("SetNodeProperty age %q: %w", p.name, err)
		}
	}
	for _, r := range [][3]string{
		{"Alice", "Bob", "KNOWS"},
		{"Bob", "Carol", "KNOWS"},
		{"Carol", "Dave", "KNOWS"},
		{"Dave", "Eve", "KNOWS"},
		{"Alice", "Carol", "FRIENDS"},
	} {
		if err := g.AddEdge(r[0], r[1], 1.0); err != nil {
			return fmt.Errorf("AddEdge %s->%s: %w", r[0], r[1], err)
		}
		g.SetEdgeLabel(r[0], r[1], r[2])
	}
	return nil
}

// matchPersonNames runs a label scan with a property projection and an
// ORDER BY, printing every Person's name in ascending order.
func matchPersonNames(ctx context.Context, eng *cypher.Engine, w io.Writer) error {
	const query = "MATCH (n:Person) RETURN n.name AS name ORDER BY name"
	fmt.Fprintf(w, "%s\n", query)
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("MATCH Person names: %w", err)
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
		rec := res.Record()
		name, err := stringCell(rec, "name")
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %s\n", name)
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("MATCH Person names result: %w", err)
	}
	return nil
}

// matchOlderThan runs a WHERE filter on the age property, printing each
// matching person's name and age in ascending name order.
func matchOlderThan(ctx context.Context, eng *cypher.Engine, w io.Writer, minAge int64) error {
	const query = "MATCH (n:Person) WHERE n.age > $min RETURN n.name AS name, n.age AS age ORDER BY name"
	fmt.Fprintf(w, "\nMATCH (n:Person) WHERE n.age > %d RETURN n.name AS name, n.age AS age ORDER BY name\n", minAge)
	params := map[string]expr.Value{"min": expr.IntegerValue(minAge)}
	res, err := eng.Run(ctx, query, params)
	if err != nil {
		return fmt.Errorf("MATCH WHERE age: %w", err)
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
		rec := res.Record()
		name, err := stringCell(rec, "name")
		if err != nil {
			return err
		}
		age, err := intCell(rec, "age")
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %-5s age %d\n", name, age)
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("MATCH WHERE age result: %w", err)
	}
	return nil
}

// matchKnows runs a relationship pattern over the KNOWS edges, printing
// each directed pair in ascending (from, to) order.
func matchKnows(ctx context.Context, eng *cypher.Engine, w io.Writer) error {
	const query = "MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS from, b.name AS to ORDER BY from, to"
	fmt.Fprintf(w, "\n%s\n", query)
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("MATCH KNOWS: %w", err)
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
		rec := res.Record()
		from, err := stringCell(rec, "from")
		if err != nil {
			return err
		}
		to, err := stringCell(rec, "to")
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  %s KNOWS %s\n", from, to)
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("MATCH KNOWS result: %w", err)
	}
	return nil
}

// createGuest runs a Cypher CREATE inside a write transaction to add a
// Guest node, then confirms the new label is registered in the graph.
func createGuest(ctx context.Context, eng *cypher.Engine, g *lpg.Graph[string, float64], w io.Writer) error {
	const query = `CREATE (n:Guest {name: "Frank"})`
	fmt.Fprintf(w, "\n%s\n", query)
	res, err := eng.RunInTx(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("CREATE Guest: %w", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return fmt.Errorf("CREATE Guest result: %w", err)
	}
	if err := res.Close(); err != nil {
		return fmt.Errorf("CREATE Guest close: %w", err)
	}
	if _, ok := g.Registry().Lookup("Guest"); !ok {
		return fmt.Errorf("CREATE Guest: label %q not registered after commit", "Guest")
	}
	fmt.Fprintf(w, "  created Guest{name: \"Frank\"} — label registered in graph\n")
	return nil
}

// stringCell reads column col from rec and returns its underlying Go
// string. The Cypher engine returns a projected string property as an
// expr.StringValue; this unwraps it to the bare string (printing the
// value directly would emit the quoted "Alice" form).
func stringCell(rec map[string]any, col string) (string, error) {
	v, ok := rec[col]
	if !ok {
		return "", fmt.Errorf("column %q missing from record", col)
	}
	s, ok := v.(expr.StringValue)
	if !ok {
		return "", fmt.Errorf("column %q is %T, want expr.StringValue", col, v)
	}
	return string(s), nil
}

// intCell reads column col from rec and returns its underlying int64.
// The Cypher engine returns a projected integer property as an
// expr.IntegerValue.
func intCell(rec map[string]any, col string) (int64, error) {
	v, ok := rec[col]
	if !ok {
		return 0, fmt.Errorf("column %q missing from record", col)
	}
	n, ok := v.(expr.IntegerValue)
	if !ok {
		return 0, fmt.Errorf("column %q is %T, want expr.IntegerValue", col, v)
	}
	return int64(n), nil
}
