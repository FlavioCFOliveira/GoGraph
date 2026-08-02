package cypher_test

// writer_rows_test.go — a writing statement observes its OWN work (rmp #2299),
// row by row within a statement and statement by statement within an explicit
// transaction.
//
// The write path used to read the PRESENT — `ReadAt(nil)`, "the current stored
// value, no version walk" — which gave it read-your-own-writes for free, and
// gave it another writer's uncommitted work for free too. It now reads through
// the writing transaction's own snapshot: as of the instant it began, plus its
// own versions, via the ts == txID branch of mvcc.Visible.
//
// That substitution is only safe if it preserves the eager row-by-row apply:
// later rows of a statement MUST still observe earlier rows (cypher/undo.go:5-8).
// These tests are what verifies that rather than assuming it, across the write
// clauses whose correctness depends on it.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// writeScalar runs a writing statement and returns its single integer column.
func writeScalar(t *testing.T, eng *cypher.Engine, q string) int64 {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx %q: %v", q, err)
	}
	defer func() {
		if err := res.Close(); err != nil {
			t.Fatalf("close %q: %v", q, err)
		}
	}()
	if !res.Next() {
		t.Fatalf("%q returned no row (err=%v)", q, res.Err())
	}
	n, ok := res.ValueAt(0).(expr.IntegerValue)
	if !ok {
		t.Fatalf("%q returned %T, want IntegerValue", q, res.ValueAt(0))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%q drain: %v", q, err)
	}
	return int64(n)
}

// TestWriteRows_LaterRowsObserveEarlierRows is acceptance criterion 3's
// within-a-statement half. Each case is a shape whose correct answer depends on
// a later row seeing what an earlier row of the SAME statement did.
func TestWriteRows_LaterRowsObserveEarlierRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup []string
		query string
		want  int64
		why   string
	}{
		{
			name:  "MERGE deduplicates within one statement",
			query: "UNWIND [1, 1, 1, 1] AS k MERGE (p:M {k: k}) WITH count(p) AS c MATCH (n:M) RETURN count(n) AS n",
			want:  1,
			why:   "rows 2-4 must MATCH the node row 1 created, not create three more",
		},
		{
			name:  "MERGE distinguishes distinct keys within one statement",
			query: "UNWIND [1, 2, 1, 2, 3] AS k MERGE (p:M2 {k: k}) WITH count(p) AS c MATCH (n:M2) RETURN count(n) AS n",
			want:  3,
			why:   "a repeated key must match and a new one must create",
		},
		{
			name:  "CREATE is visible to a later MATCH in the same statement",
			query: "CREATE (:C {v: 1}) WITH 1 AS _ MATCH (n:C) RETURN count(n) AS n",
			want:  1,
			why:   "the MATCH runs after the CREATE in the same statement",
		},
		{
			name:  "SET is visible to a later MATCH in the same statement",
			setup: []string{"CREATE (:S {v: 1})"},
			query: "MATCH (n:S) SET n.v = 99 WITH n MATCH (m:S) WHERE m.v = 99 RETURN count(m) AS n",
			want:  1,
			why:   "the second MATCH must see the value the SET wrote",
		},
		{
			// 3, not 4, and the arithmetic is the property. The statement
			// streams row by row: k=1 MERGEs :O{1} and its MATCH then sees ONE
			// :O; k=2 MERGEs :O{2} and its MATCH sees TWO. 1+2 = 3. A count of
			// 4 would mean both MERGEs had completed before either MATCH ran,
			// and a count of 2 would mean neither row saw its own work.
			name:  "OPTIONAL MATCH correlates against work done earlier in the statement",
			query: "UNWIND [1, 2] AS k MERGE (a:O {k: k}) WITH a OPTIONAL MATCH (a)-[:E]->(b) WITH a, b MATCH (n:O) RETURN count(n) AS n",
			want:  3,
			why:   "the correlated apply re-runs per outer row, and row N sees rows 1..N's work",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, _ := storelessEngineWithGraph(t)
			for _, s := range tc.setup {
				autocommit(t, eng, s)
			}
			if got := writeScalar(t, eng, tc.query); got != tc.want {
				t.Fatalf("got %d, want %d — %s.\nThe writing statement is not observing its own "+
					"earlier rows, which is what the ts == txID branch of mvcc.Visible supplies now "+
					"that the write path reads through a snapshot instead of the present (rmp #2299).",
					got, tc.want, tc.why)
			}
		})
	}
}

// TestWriteRows_LaterStatementsObserveEarlierStatements is criterion 3's
// across-a-transaction half. Every statement of one explicit write transaction
// shares one stamp window and therefore one transaction id (lpg.Graph.LockBarrier
// opens it, UnlockBarrier closes it), so statement N must see statements 1..N-1
// even though none of them has committed.
func TestWriteRows_LaterStatementsObserveEarlierStatements(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	execTx := func(q string) {
		t.Helper()
		res, err := tx.Exec(q, nil)
		if err != nil {
			t.Fatalf("Exec %q: %v", q, err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Exec %q drain: %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Exec %q close: %v", q, err)
		}
	}
	scalarTx := func(q string) int64 {
		t.Helper()
		res, err := tx.Exec(q, nil)
		if err != nil {
			t.Fatalf("Exec %q: %v", q, err)
		}
		defer func() { _ = res.Close() }()
		if !res.Next() {
			t.Fatalf("%q returned no row (err=%v)", q, res.Err())
		}
		n, ok := res.ValueAt(0).(expr.IntegerValue)
		if !ok {
			t.Fatalf("%q returned %T, want IntegerValue", q, res.ValueAt(0))
		}
		return int64(n)
	}

	execTx("CREATE (:T {v: 1})")
	if got := scalarTx("MATCH (n:T) RETURN count(n) AS n"); got != 1 {
		t.Fatalf("statement 2 saw %d :T nodes, want 1: it cannot see what statement 1 created, "+
			"even though both share one transaction id", got)
	}
	execTx("MATCH (n:T) SET n.v = 2")
	if got := scalarTx("MATCH (n:T) WHERE n.v = 2 RETURN count(n) AS n"); got != 1 {
		t.Fatalf("statement 4 saw %d nodes with v=2, want 1: it cannot see statement 3's SET", got)
	}
	execTx("MATCH (n:T) DELETE n")
	if got := scalarTx("MATCH (n:T) RETURN count(n) AS n"); got != 0 {
		t.Fatalf("statement 6 saw %d :T nodes, want 0: it cannot see statement 5's DELETE", got)
	}

	// Nothing of this is visible outside the transaction until it commits.
	execTx("CREATE (:T2)")
	outside, err := eng.Run(ctx, "MATCH (n:T2) RETURN count(n) AS n", nil)
	if err != nil {
		t.Fatalf("outside read: %v", err)
	}
	if !outside.Next() {
		t.Fatalf("outside read returned no row: %v", outside.Err())
	}
	if n, _ := outside.ValueAt(0).(expr.IntegerValue); int64(n) != 0 {
		t.Fatalf("a reader outside the transaction saw %d :T2 nodes, want 0: uncommitted work is "+
			"leaking out of the write transaction", int64(n))
	}
	if err := outside.Close(); err != nil {
		t.Fatalf("outside close: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	after, err := eng.Run(ctx, "MATCH (n:T2) RETURN count(n) AS n", nil)
	if err != nil {
		t.Fatalf("read after commit: %v", err)
	}
	if !after.Next() {
		t.Fatalf("read after commit returned no row: %v", after.Err())
	}
	if n, _ := after.ValueAt(0).(expr.IntegerValue); int64(n) != 1 {
		t.Fatalf("after commit a reader saw %d :T2 nodes, want 1", int64(n))
	}
	if err := after.Close(); err != nil {
		t.Fatalf("close after commit: %v", err)
	}
}

// TestWriteRows_StructuralChangesAreNotVisibleToALaterClause records a
// pre-existing INCONSISTENCY, with its evidence, so it is not mistaken for
// something rmp #2299 introduced or something rmp #2299 fixed.
//
// A node CREATE and a SET are visible to a later clause of the same statement
// (asserted in TestWriteRows_LaterRowsObserveEarlierRows). An edge CREATE and a
// node DELETE are NOT. Measured 2026-08-02, and measured IDENTICALLY with the
// write path reading the present (ReadAt(nil)) and reading the writing
// transaction's snapshot, so the substitution rmp #2299 made is
// behaviour-preserving here — which is the only claim this file is entitled to
// make about it.
//
// Both writes DO land: the assertions below check the committed state after the
// statement, and it is correct in both cases. What differs is only what a later
// clause of the SAME statement observes.
//
// Whether that is openCypher's Eager semantics working as specified or a
// candidate-set gap is NOT settled here — it needs the specification and the
// TCK, not an argument, and it is out of scope for a task about writer
// identity. This test asserts the observed behaviour so a change to it is
// noticed, and deliberately does not call it correct.
func TestWriteRows_StructuralChangesAreNotVisibleToALaterClause(t *testing.T) {
	t.Run("edge CREATE", func(t *testing.T) {
		t.Parallel()
		eng, _ := storelessEngineWithGraph(t)
		autocommit(t, eng, "CREATE (:R1 {id: 1}), (:R2 {id: 2})")
		got := writeScalar(t, eng,
			"MATCH (a:R1), (b:R2) CREATE (a)-[:LINK]->(b) WITH 1 AS _ MATCH (:R1)-[:LINK]->(x:R2) RETURN count(x) AS n")
		if got != 0 {
			t.Fatalf("the traversal saw %d edges in the same statement, recorded behaviour is 0", got)
		}
		// The edge is really there once the statement finishes.
		if after := readScalar(t, eng, "MATCH (:R1)-[:LINK]->(x:R2) RETURN count(x) AS n"); after != 1 {
			t.Fatalf("after the statement the graph holds %d LINK edges, want 1: the CREATE itself failed", after)
		}
	})
	t.Run("node DELETE", func(t *testing.T) {
		t.Parallel()
		eng, _ := storelessEngineWithGraph(t)
		autocommit(t, eng, "CREATE (:D), (:D), (:D)")
		got := writeScalar(t, eng,
			"MATCH (n:D) WITH n LIMIT 2 DELETE n WITH 1 AS _ MATCH (m:D) RETURN count(m) AS n")
		if got != 3 {
			t.Fatalf("the second MATCH saw %d :D nodes in the same statement, recorded behaviour is 3", got)
		}
		if after := readScalar(t, eng, "MATCH (m:D) RETURN count(m) AS n"); after != 1 {
			t.Fatalf("after the statement the graph holds %d :D nodes, want 1: the DELETE itself failed", after)
		}
	})
}

// readScalar runs a read-only statement and returns its single integer column.
func readScalar(t *testing.T, eng *cypher.Engine, q string) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("%q returned no row (err=%v)", q, res.Err())
	}
	n, ok := res.ValueAt(0).(expr.IntegerValue)
	if !ok {
		t.Fatalf("%q returned %T, want IntegerValue", q, res.ValueAt(0))
	}
	return int64(n)
}
