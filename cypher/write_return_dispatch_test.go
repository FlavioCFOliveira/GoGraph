package cypher_test

// write_return_dispatch_test.go — T874
//
// Regression coverage for the CREATE/SET/MERGE + RETURN dispatch fix.
// Before the fix, "CREATE … RETURN n.x" built a ProduceResults→Projection→
// CreateNode plan that fell through to buildOperator's default branch and
// returned "cypher: build plan: cypher: unsupported IR node *ir.CreateNode".
//
// The basic regressions (CREATE+RETURN, SET+RETURN, DELETE+RETURN) are already
// covered in write_with_return_test.go. This file adds the cases that are NOT
// yet tested there:
//
//   - TestCreateReturn_TwoNodes: CREATE (n),(m) RETURN n, m → 1 row, 2 columns.
//   - TestSetReturn_TwoProperties: SET + RETURN both property columns in one row.
//   - TestCreateReturn_CountAfter: verify CREATE+RETURN does not leave phantom
//     state — a subsequent count must match.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestCreateReturn_TwoNodes verifies that creating two nodes in a single
// CREATE clause and returning both in the same RETURN yields exactly one row
// with two non-nil node columns.
func TestCreateReturn_TwoNodes(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	_ = g

	res, err := eng.RunInTx(ctx, `CREATE (n:A {v: 1}), (m:B {v: 2}) RETURN n, m`, nil)
	if err != nil {
		t.Fatalf("RunInTx CREATE two nodes + RETURN: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	if rows[0]["n"] == nil {
		t.Error("column n is nil")
	}
	if rows[0]["m"] == nil {
		t.Error("column m is nil")
	}
}

// TestSetReturn_TwoProperties verifies that SET with a RETURN clause containing
// two property projections emits one row with both columns populated.
func TestSetReturn_TwoProperties(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	_ = g

	// Seed.
	res0, err := eng.RunInTx(ctx, `CREATE (n:Widget {code: "W1"})`, nil)
	if err != nil {
		t.Fatalf("seed CREATE: %v", err)
	}
	drainResult(t, res0)

	// SET two properties, RETURN both.
	res, err := eng.RunInTx(ctx,
		`MATCH (n:Widget {code: "W1"}) SET n.active = true, n.score = 99 RETURN n.active AS active, n.score AS score`,
		nil)
	if err != nil {
		t.Fatalf("RunInTx SET+RETURN two props: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0]["active"] == nil {
		t.Error("active column is nil")
	}
	if rows[0]["score"] == nil {
		t.Error("score column is nil")
	}
}

// TestCreateReturn_CountAfter verifies that CREATE … RETURN does not create
// phantom nodes: a MATCH count immediately after must equal the number of
// nodes explicitly created.
func TestCreateReturn_CountAfter(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	_ = g

	// Create three nodes with RETURN.
	for i := 0; i < 3; i++ {
		res, err := eng.RunInTx(ctx, `CREATE (n:Counter) RETURN n`, nil)
		if err != nil {
			t.Fatalf("CREATE %d: %v", i, err)
		}
		drainResult(t, res)
	}

	// Count must be exactly 3.
	countRes, err := eng.Run(ctx, `MATCH (n:Counter) RETURN count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("MATCH count: %v", err)
	}
	rows := drainRecords(t, countRes)
	if len(rows) != 1 {
		t.Fatalf("count query returned %d rows, want 1", len(rows))
	}
	if got := fmtAny(rows[0]["c"]); got != "3" {
		t.Errorf("count after 3 CREATE+RETURN = %s, want 3", got)
	}
}
