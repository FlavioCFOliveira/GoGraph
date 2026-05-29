package cypher_test

// example_test.go — runnable godoc examples for the public query-engine API
// (#1106). These are the canonical "how to query GoGraph" reference: build a
// labelled property graph, construct an Engine, and run read and write queries.
//
// All examples use only the exported API and produce deterministic output.

import (
	"context"
	"fmt"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// ExampleNewEngine shows the minimal setup: build an empty labelled property
// graph and bind it to an Engine ready to run queries.
func ExampleNewEngine() {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	// A fresh engine over an empty graph runs queries that return no rows.
	res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer res.Close()

	var rows int
	for res.Next() {
		rows++
	}
	fmt.Println("rows:", rows)
	// Output:
	// rows: 0
}

// ExampleEngine_Run runs a read query against a populated graph and reads a
// scalar aggregate from the streaming result. Result must always be closed.
func ExampleEngine_Run() {
	g := lpg.New[string, float64](adjlist.Config{})
	for _, key := range []string{"a", "b", "c"} {
		if err := g.AddNode(key); err != nil {
			fmt.Println("error:", err)
			return
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			fmt.Println("error:", err)
			return
		}
	}

	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), "MATCH (n:Person) RETURN count(n) AS people", nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer res.Close()

	for res.Next() {
		rec := res.Record()
		fmt.Println("people:", rec["people"])
	}
	// Output:
	// people: 3
}

// ExampleEngine_RunInTx executes a CREATE inside a transaction (atomic and, for
// WAL-backed engines, durable) and then reads the data back in a second query.
func ExampleEngine_RunInTx() {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	// Write: CREATE two labelled nodes atomically.
	write, err := eng.RunInTx(context.Background(),
		`CREATE (:Account {owner: "alice"}), (:Account {owner: "bob"})`, nil)
	if err != nil {
		fmt.Println("write error:", err)
		return
	}
	for write.Next() { //nolint:revive // a write query streams no result rows; drain then close
	}
	if err := write.Err(); err != nil {
		fmt.Println("write error:", err)
		return
	}
	write.Close()

	// Read-back: the committed nodes are visible to a subsequent query.
	read, err := eng.Run(context.Background(), "MATCH (a:Account) RETURN count(a) AS accounts", nil)
	if err != nil {
		fmt.Println("read error:", err)
		return
	}
	defer read.Close()
	for read.Next() {
		fmt.Println("accounts:", read.Record()["accounts"])
	}
	// Output:
	// accounts: 2
}

// ExampleEngine_RunAny passes query parameters as a plain map[string]any, which
// the engine binds automatically. This is the convenient entry point for
// callers that do not want to import the internal value types.
func ExampleEngine_RunAny() {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	if _, err := drainTxAny(eng,
		`CREATE (:Account {owner: "alice"}), (:Account {owner: "bob"})`); err != nil {
		fmt.Println("seed error:", err)
		return
	}

	// $owner is supplied as a Go string in the params map.
	res, err := eng.RunAny(context.Background(),
		`MATCH (a:Account) WHERE a.owner = $owner RETURN a.owner AS owner`,
		map[string]any{"owner": "bob"},
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer res.Close()
	for res.Next() {
		fmt.Println("owner:", res.Record()["owner"])
	}
	// Output:
	// owner: "bob"
}

// ExampleEngine_Explain returns the logical plan for a query as text without
// executing it or touching the graph.
func ExampleEngine_Explain() {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	plan, err := eng.Explain("MATCH (n:Person) RETURN n", nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(plan)
	// Output:
	// ProduceResults
	// └─ Projection
	//    └─ NodeByLabelScan
}

// ExampleBindParams converts a map of Go values into the engine's internal
// parameter representation. Engine.RunAny calls this for you; BindParams is
// exported for callers that bind once and run a query repeatedly.
func ExampleBindParams() {
	bound, err := cypher.BindParams(map[string]any{
		"name": "acme",
		"size": int64(42),
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	_, hasName := bound["name"]
	_, hasSize := bound["size"]
	fmt.Printf("bound=%d name=%v size=%v\n", len(bound), hasName, hasSize)
	// Output:
	// bound=2 name=true size=true
}

// drainTxAny runs a write query in a transaction, draining and closing the
// result. It is a tiny helper shared by the parameter example above.
func drainTxAny(eng *cypher.Engine, query string) (int, error) {
	res, err := eng.RunInTxAny(context.Background(), query, nil)
	if err != nil {
		return 0, err
	}
	defer res.Close()
	var rows int
	for res.Next() {
		rows++
	}
	return rows, res.Err()
}
