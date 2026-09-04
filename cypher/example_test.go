package cypher_test

// example_test.go — runnable godoc examples for the public query-engine API
// (#1106). These are the canonical "how to query GoGraph" reference: build a
// labelled property graph, construct an Engine, and run read and write queries.
//
// All examples use only the exported API and produce deterministic output.

import (
	"context"
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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
	for write.Next() { // a write query streams no result rows; drain then close
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

// ExampleEngine_Explain returns the PHYSICAL plan for a query as text without
// executing it or touching the graph: the operator tree the builder actually
// produced, each node named after its concrete operator type. Because the name
// comes from the operator itself, the rendering cannot disagree with what runs —
// an index seek appears as NodeByIndexSeek only when a NodeByIndexSeek is what
// was built.
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
	// Project
	// └─ NodeByLabelScan [Person]
}

// ExampleEngine_ExplainLogical returns the LOGICAL plan, which is where the
// planner's cardinality ESTIMATES live: each row-producing operator carries an
// estimate and its provenance tag (exact / stats / heuristic), and the label scan
// over an empty graph is an exact count of zero (#2099). Those estimates have no
// counterpart on a built operator, so they are visible only here — use this to
// understand why a plan was chosen, and [cypher.Engine.Explain] to see what runs.
func ExampleEngine_ExplainLogical() {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	plan, err := eng.ExplainLogical("MATCH (n:Person) RETURN n", nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(plan)
	// Output:
	// ProduceResults
	// └─ Projection
	//    └─ NodeByLabelScan [n:Person] (est. rows=0, exact)
}

// ExampleEngine_ExplainTable returns the same logical plan
// [cypher.Engine.ExplainLogical] returns, as a Neo4j-style columnar table. The
// Est.Rows column carries the planner's estimate — an exact count of zero for the
// label scan over an empty graph — and the Vars column the variables each
// operator exposes, which no tree rendering prints.
func ExampleEngine_ExplainTable() {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	table, err := eng.ExplainTable("MATCH (n:Person) RETURN n", nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(table)
	// Output:
	// +----------------------------------+----------+------+
	// | Operator                         | Est.Rows | Vars |
	// +----------------------------------+----------+------+
	// | ProduceResults                   |        - | n    |
	// | └─ Projection                    |        - | n    |
	// |    └─ NodeByLabelScan [n:Person] |        0 | n    |
	// +----------------------------------+----------+------+
}

// ExampleEngine_ProfileTable executes the query and returns the measured
// physical plan as a columnar table. The measured times vary from run to run, so
// this example asserts the table's structure rather than embedding them.
//
// Note what the Total line means: its Rows cell sums every operator's emitted
// rows across the plan, so it is a cost measure and not the result's row count —
// the result's row count is the ROOT operator's Rows, on the first data line.
func ExampleEngine_ProfileTable() {
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

	table, err := eng.ProfileTable(context.Background(), "MATCH (n:Person) RETURN n", nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("has columns:", strings.Contains(table, "Rows") && strings.Contains(table, "DbHits"))
	fmt.Println("has scan:", strings.Contains(table, "NodeByLabelScan"))
	fmt.Println("scan read 3 records:", strings.Contains(table, "|    3 |      3 |"))
	fmt.Println("has total:", strings.Contains(table, "| Total"))
	// Output:
	// has columns: true
	// has scan: true
	// scan read 3 records: true
	// has total: true
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
