package cypher_test

// empty_reltype_test.go — regression gate for #1878. An empty backtick-quoted
// relationship type (MATCH ()-[:``]->()) previously collided with the exec
// "no type filter" sentinel and matched EVERY edge (and, as the first
// alternative of a union, poisoned the whole filter); an empty node label and
// the CREATE ()-[:``]->() form were likewise mishandled. openCypher's data
// model gives every relationship a non-empty type and every label a non-empty
// name, so these are now rejected at semantic analysis. Real (non-empty) type
// and label filters are unaffected.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestEngine_EmptyRelationshipTypeAndLabel_Rejected(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	drainRunInTx(t, eng, `CREATE (:A)-[:KNOWS]->(:B)`)
	drainRunInTx(t, eng, `CREATE (:A)-[:LIKES]->(:B)`)

	ctx := context.Background()
	// Backticks are literal characters inside a Go double-quoted string, so
	// "``" below is an empty backtick-quoted identifier in the Cypher text.
	// Read-side forms go through the read-only Run path. This covers the clause
	// patterns AND the expression-position sites that bypass the
	// MATCH/CREATE/MERGE clause walkers (pattern comprehension, bare-WHERE
	// pattern predicate, and the pattern forms of EXISTS {} / COUNT {}) — each
	// previously reproduced the match-everything defect at an uncovered site
	// (#1878 completeness).
	rejectedRead := []struct {
		name string
		q    string
	}{
		{"empty rel type", "MATCH ()-[r:``]->() RETURN count(r) AS c"},
		{"empty first alternative", "MATCH ()-[r:``|KNOWS]->() RETURN count(r) AS c"},
		{"empty rel type varlength", "MATCH ()-[r:``*1..2]->() RETURN count(r) AS c"},
		{"empty node label", "MATCH (n:``) RETURN count(n) AS c"},
		{"pattern comprehension empty type", "MATCH (a) RETURN size([(a)-[r:``]->(b) | b]) AS n"},
		{"pattern comprehension empty node label", "MATCH (a) RETURN size([(a)-[r]->(b:``) | b]) AS n"},
		{"bare WHERE pattern empty type", "MATCH (a) WHERE (a)-[:``]->() RETURN count(a) AS c"},
		{"EXISTS pattern-form empty type", "MATCH (a) WHERE EXISTS { (a)-[:``]->() } RETURN count(a) AS c"},
		{"COUNT pattern-form empty type", "MATCH (a) RETURN COUNT { (a)-[:``]->() } AS c"},
	}
	for _, tc := range rejectedRead {
		if _, err := eng.Run(ctx, tc.q, nil); err == nil {
			t.Errorf("%s: %q was accepted; want a semantic rejection", tc.name, tc.q)
		}
	}

	// Write-side forms go through RunInTx (which permits writes), so a
	// rejection here is the empty-name semantic error, not the read-only guard.
	rejectedWrite := []struct {
		name string
		q    string
	}{
		{"create empty rel type", "CREATE ()-[:``]->()"},
		{"create empty node label", "CREATE (:``)"},
		{"merge empty rel type", "MERGE ()-[:``]->()"},
		{"set empty label", "MATCH (n:A) SET n:``"},
		{"remove empty label", "MATCH (n:A) REMOVE n:``"},
	}
	for _, tc := range rejectedWrite {
		if _, err := eng.RunInTx(ctx, tc.q, nil); err == nil {
			t.Errorf("%s: %q was accepted; want a semantic rejection", tc.name, tc.q)
		}
	}

	// The rejection carries a clear, dedicated message (not the reused
	// "expects Boolean operands" text).
	if _, err := eng.Run(ctx, "MATCH ()-[r:``]->() RETURN r", nil); err == nil {
		t.Error("empty rel type must be rejected")
	} else if !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("error message = %q, want it to mention \"must not be empty\"", err.Error())
	}

	// Controls: real (non-empty) type/label filters and the unfiltered match
	// are unaffected by the empty-name rejection.
	if got := countScalar(t, eng, "MATCH ()-[r]->() RETURN count(r) AS c"); got != 2 {
		t.Errorf("unfiltered edge count = %d, want 2", got)
	}
	if got := countScalar(t, eng, "MATCH ()-[r:KNOWS]->() RETURN count(r) AS c"); got != 1 {
		t.Errorf("KNOWS edge count = %d, want 1", got)
	}
	if got := countScalar(t, eng, "MATCH (n:A) RETURN count(n) AS c"); got != 2 {
		t.Errorf("A-label node count = %d, want 2", got)
	}
}
