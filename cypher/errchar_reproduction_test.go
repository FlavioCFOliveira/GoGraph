package cypher_test

// errchar_reproduction_test.go — end-to-end fence for the round-3 audit's
// correctness finding C1 (rmp #2167), reproduced through the public engine on
// the exact graph the audit used: 2 `:A` nodes and 3 `:B` nodes.
//
// The lexer's catch-all rule routed any unrecognised character to a hidden
// channel, so the character was deleted and the rest of the query executed as
// if it had never been typed. Measured on this graph before the fix:
//
//	MATCH (n) WHERE n.v <> 2 RETURN n   → 4 rows  (correct)
//	MATCH (n) WHERE n.v != 2 RETURN n   → 1 row   — the row where v = 2
//	MATCH (n:!A) RETURN n               → 2 rows  — exactly the :A nodes
//
// Each wrong result was returned with no error. `:!A` is valid Neo4j 5 syntax,
// so a query ported from Neo4j returned the precise complement of what it asked
// for. Neither case is visible to the openCypher TCK, which only executes
// syntactically valid queries — this test is the guard.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newErrCharFixture builds the audit's graph: 2 `:A` nodes and 3 `:B` nodes,
// each carrying an integer property v.
func newErrCharFixture(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	const seed = `CREATE (:A {v: 1}), (:A {v: 2}), (:B {v: 3}), (:B {v: 4}), (:B {v: 5})`
	res, err := eng.RunAny(context.Background(), seed, nil)
	if err != nil {
		t.Fatalf("seed CREATE failed: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("seed CREATE result error: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("seed CREATE close error: %v", err)
	}
	return eng
}

// rowCount runs query and returns the number of rows, or an error.
func rowCount(eng *cypher.Engine, query string) (int, error) {
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		return 0, err
	}
	defer res.Close()
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// TestErrChar_AuditReproductionRejected proves the two queries the audit
// reproduced are now rejected rather than answered wrongly.
func TestErrChar_AuditReproductionRejected(t *testing.T) {
	eng := newErrCharFixture(t)

	tests := []struct {
		name  string
		query string
		// wrongRows is what the query returned before the fix, recorded so the
		// failure message states what the regression would be.
		wrongRows int
	}{
		{"bang_equal", "MATCH (n) WHERE n.v != 2 RETURN n", 1},
		{"label_negation", "MATCH (n:!A) RETURN n", 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rowCount(eng, tc.query)
			if err == nil {
				t.Fatalf("%q returned %d rows and no error; before the fix it returned %d "+
					"rows by silently discarding the '!'", tc.query, got, tc.wrongRows)
			}
			if !strings.Contains(err.Error(), "unrecognised character") {
				t.Fatalf("%q error = %v, want an unrecognised-character syntax error", tc.query, err)
			}
		})
	}
}

// TestErrChar_ConformantEquivalentsUnchanged proves the fix rejected only the
// non-conformant spellings: the openCypher forms the rejected queries were
// meant to express still execute, and still return the right rows.
func TestErrChar_ConformantEquivalentsUnchanged(t *testing.T) {
	eng := newErrCharFixture(t)

	tests := []struct {
		name  string
		query string
		want  int
	}{
		// `<>` is openCypher's inequality operator; 4 of the 5 nodes have v <> 2.
		{"not_equal_conformant", "MATCH (n) WHERE n.v <> 2 RETURN n", 4},
		// The label set `:!A` was meant to express is the 3 :B nodes.
		{"label_complement_conformant", "MATCH (n) WHERE NOT n:A RETURN n", 3},
		// Plain equality and plain label matching are untouched.
		{"equality", "MATCH (n) WHERE n.v = 2 RETURN n", 1},
		{"label_a", "MATCH (n:A) RETURN n", 2},
		{"label_b", "MATCH (n:B) RETURN n", 3},
		{"all", "MATCH (n) RETURN n", 5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rowCount(eng, tc.query)
			if err != nil {
				t.Fatalf("%q returned error: %v", tc.query, err)
			}
			if got != tc.want {
				t.Fatalf("%q returned %d rows, want %d", tc.query, got, tc.want)
			}
		})
	}
}
