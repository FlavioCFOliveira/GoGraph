// Example 22_cypher demonstrates the GoGraph Cypher engine: building a small
// social graph with the graph API and querying it with MATCH and label scans.
package main

import (
	"context"
	"fmt"
	"log"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

func main() {
	ctx := context.Background()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	buildGraph(g)

	eng := cypher.NewEngine(g)

	matchPersons(ctx, eng)
	matchAll(ctx, eng, "before CREATE")
	createGuest(ctx, eng, g)
	matchAll(ctx, eng, "after CREATE")
}

// buildGraph populates g with 5 Person nodes and 5 directed relationships.
func buildGraph(g *lpg.Graph[string, float64]) {
	type person struct {
		name string
		age  int64
	}
	for _, p := range []person{
		{"Alice", 30}, {"Bob", 25}, {"Carol", 35}, {"Dave", 28}, {"Eve", 22},
	} {
		g.AddNode(p.name)
		g.SetNodeLabel(p.name, "Person")
		g.SetNodeProperty(p.name, "name", lpg.StringValue(p.name))
		g.SetNodeProperty(p.name, "age", lpg.Int64Value(p.age))
	}
	for _, r := range [][3]string{
		{"Alice", "Bob", "KNOWS"},
		{"Bob", "Carol", "KNOWS"},
		{"Carol", "Dave", "KNOWS"},
		{"Dave", "Eve", "KNOWS"},
		{"Alice", "Carol", "FRIENDS"},
	} {
		g.AddEdge(r[0], r[1], 1.0)
		g.SetEdgeLabel(r[0], r[1], r[2])
	}
}

// drain consumes all rows from res, checking for iteration errors, then closes.
func drain(res *cypher.Result, tag string) {
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		log.Fatalf("%s result: %v", tag, err)
	}
	if err := res.Close(); err != nil {
		log.Fatalf("%s close: %v", tag, err)
	}
}

// matchPersons runs MATCH (n:Person) RETURN n and prints each matched node ID.
func matchPersons(ctx context.Context, eng *cypher.Engine) {
	fmt.Println("MATCH (n:Person) RETURN n")
	res, err := eng.Run(ctx, "MATCH (n:Person) RETURN n", nil)
	if err != nil {
		log.Fatalf("MATCH Person: %v", err)
	}
	var count int
	for res.Next() {
		rec := res.Record()
		fmt.Printf("  nodeID=%v\n", rec["n"])
		count++
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		log.Fatalf("MATCH Person result: %v", err)
	}
	if err := res.Close(); err != nil {
		log.Fatalf("MATCH Person close: %v", err)
	}
	fmt.Printf("  => %d Person nodes\n", count)
}

// matchAll runs MATCH (n) RETURN n and prints the total node count.
func matchAll(ctx context.Context, eng *cypher.Engine, label string) {
	fmt.Printf("\nMATCH (n) RETURN n  (%s)\n", label)
	res, err := eng.Run(ctx, "MATCH (n) RETURN n", nil)
	if err != nil {
		log.Fatalf("MATCH all: %v", err)
	}
	var total int
	for res.Next() {
		total++
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		log.Fatalf("MATCH all result: %v", err)
	}
	if err := res.Close(); err != nil {
		log.Fatalf("MATCH all close: %v", err)
	}
	fmt.Printf("  => %d total nodes\n", total)
}

// createGuest runs a Cypher CREATE to add a Guest node and verifies it.
func createGuest(ctx context.Context, eng *cypher.Engine, g *lpg.Graph[string, float64]) {
	fmt.Println("\nCREATE (n:Guest {name: \"Frank\"})")
	res, err := eng.RunInTx(ctx, `CREATE (n:Guest {name: "Frank"})`, nil)
	if err != nil {
		log.Fatalf("CREATE Guest: %v", err)
	}
	drain(res, "CREATE Guest")
	if _, ok := g.Registry().Lookup("Guest"); ok {
		fmt.Println("  label Guest registered in graph")
	}
}
