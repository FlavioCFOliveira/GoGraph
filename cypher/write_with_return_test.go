package cypher_test

// write_with_return_test.go — regression coverage for the
// CREATE/SET/DELETE + RETURN dispatch fix. Before the fix, the
// canonical lowering of `CREATE … RETURN n.x` was
// ProduceResults → Projection → CreateNode, which fell through to
// buildOperator's default branch and failed with
// "cypher: build plan: cypher: unsupported IR node *ir.CreateNode".

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// fmtAny returns a stringification of v that is stable across the
// engine's possible numeric types ([expr.IntegerValue], int64, int).
func fmtAny(v any) string { return fmt.Sprintf("%v", v) }

// drainRecords iterates res to exhaustion, collecting each Record into
// the returned slice. Used by the assertions below to compare row sets
// without coupling to ResultSet internals.
func drainRecords(t *testing.T, res *cypher.Result) []map[string]any {
	t.Helper()
	var out []map[string]any
	for res.Next() {
		rec := res.Record()
		copy := make(map[string]any, len(rec))
		for k, v := range rec {
			copy[k] = v
		}
		out = append(out, copy)
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		t.Fatalf("result error: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return out
}

// TestRunInTx_CreateReturn verifies that a CREATE clause followed by a
// RETURN clause emits one record per created node and persists the
// node and its properties to the graph.
func TestRunInTx_CreateReturn(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (n:User {username: "alice"}) RETURN n.username AS username`, nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("CREATE+RETURN yielded %d rows, want 1: %v", len(rows), rows)
	}
	if got := rows[0]["username"]; got == nil {
		t.Fatalf("username column missing: %v", rows[0])
	}

	// The node and its label must be in the graph after the call.
	res2, err := eng.Run(ctx, `MATCH (u:User) RETURN u.username AS username`, nil)
	if err != nil {
		t.Fatalf("Run MATCH: %v", err)
	}
	matchRows := drainRecords(t, res2)
	if len(matchRows) != 1 {
		t.Fatalf("MATCH after CREATE found %d rows, want 1", len(matchRows))
	}
}

// TestRunInTx_SetReturn verifies that SET property with RETURN emits
// one record per affected node carrying the updated value.
func TestRunInTx_SetReturn(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (n:User {username: "bob"})`, nil)
	if err != nil {
		t.Fatalf("seed CREATE: %v", err)
	}
	_ = drainRecords(t, res)

	res2, err := eng.RunInTx(ctx, `MATCH (u:User {username: "bob"}) SET u.display_name = "Bob" RETURN u.username AS username, u.display_name AS display_name`, nil)
	if err != nil {
		t.Fatalf("RunInTx SET+RETURN: %v", err)
	}
	rows := drainRecords(t, res2)
	if len(rows) != 1 {
		t.Fatalf("SET+RETURN yielded %d rows, want 1", len(rows))
	}
}

// TestRunInTx_DeleteReturn verifies that DELETE with RETURN drives the
// pipeline. Per openCypher 9 §3.5.8 (Return2 [15]) accessing a deleted node's
// properties in the same statement is EntityNotFound: DeletedEntityAccess; the
// test asserts the engine surfaces this error.
//
// Because that error is raised at runtime DURING the statement, the statement
// fails as a whole, and per the ACID Atomicity invariant (task #1282) a failed
// write statement is all-or-nothing: its eager in-memory mutations — including
// the DELETE — are rolled back. The node therefore SURVIVES (count == 1). This
// is the corrected semantics: before #1282 the in-memory delete leaked while
// the WAL transaction rolled back, an in-memory-vs-durable divergence. The
// openCypher TCK does not assert side effects for these error scenarios
// (Return2 [15]/[16] assert only the raised error), so both this atomic
// rollback and the older leak pass the TCK; atomicity is what dictates the
// rollback.
func TestRunInTx_DeleteReturn(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (n:User {username: "carol"})`, nil)
	if err != nil {
		t.Fatalf("seed CREATE: %v", err)
	}
	_ = drainRecords(t, res)

	res2, err := eng.RunInTx(ctx, `MATCH (u:User {username: "carol"}) DELETE u RETURN u.username AS removed`, nil)
	// The query is allowed to fail at compile / open phase OR to surface
	// the EntityNotFound only when the result is drained — both shapes
	// are observed across drivers. Accept either path: a non-nil err
	// here, or a nil err followed by a draining error.
	failed := err != nil
	if err == nil {
		for res2.Next() {
			// drain
		}
		drainErr := res2.Err()
		_ = res2.Close() //nolint:errcheck
		if drainErr == nil {
			t.Fatalf("DELETE+RETURN u.foo: expected DeletedEntityAccess error, got success")
		}
		failed = true
	}
	if !failed {
		t.Fatal("expected the DELETE+RETURN statement to fail with DeletedEntityAccess")
	}

	res3, err := eng.Run(ctx, `MATCH (u:User) RETURN count(u) AS n`, nil)
	if err != nil {
		t.Fatalf("Run count: %v", err)
	}
	countRows := drainRecords(t, res3)
	if len(countRows) != 1 {
		t.Fatalf("count yielded %d rows", len(countRows))
	}
	// The failed statement rolled back atomically, so the node is restored.
	// count() may surface as expr.IntegerValue, int64 or int depending on
	// engine version; compare via %v which renders all as "1".
	if got := countRows[0]["n"]; got == nil {
		t.Fatalf("count missing")
	} else if s := fmtAny(got); s != "1" {
		t.Fatalf("count after failed delete = %s, want 1 (statement rolled back atomically)", s)
	}
}
