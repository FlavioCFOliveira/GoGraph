package cypher_test

// same_statement_edge_visibility_test.go — rmp #2317, acceptance criterion 5.
//
// Layer: short.
//
// # The composition the openCypher TCK does not cover
//
// The suite has exactly two scenarios composing an updating clause with a later
// READING clause in one query, and neither involves relationships:
//
//	Create3 [3] MATCH-CREATE-WITH-CREATE
//	  Given two nodes, MATCH () CREATE () WITH * MATCH () CREATE ()
//	  expects +nodes 10.
//
// That number is only reachable if the second MATCH observes the first CREATE:
// the first MATCH yields 2 rows and creates 2 nodes (4 total); the second MATCH
// must then see 4 nodes, giving 2x4 = 8 rows and 8 more nodes, for 2+8 = 10. A
// second MATCH reading a frozen topology would see the original 2 and produce
// 2+4 = 6.
//
// GoGraph passed that scenario throughout, because node reads always went to live
// stores. Relationship traversal did not: it expanded over a CSR materialised at
// plan-build time, so the relationship half of the same composition was broken and
// no upstream scenario could detect it. This file supplies the missing half, in
// the same shape and with the same style of arithmetic — the expected counts are
// derived here from the semantics, not observed from the implementation.
//
// Being a conformance-shaped gate rather than a vendored scenario, it lives beside
// the engine's own tests: cypher/tck/features/ is the upstream suite verbatim and
// its scenario count is the regression baseline.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// sideEffects reports the node and relationship counts, the two quantities the
// TCK's side-effect table tracks for these scenarios.
func sideEffects(t *testing.T, eng *cypher.Engine) (nodes, rels int64) {
	t.Helper()
	return readScalar(t, eng, "MATCH (n) RETURN count(n) AS n"),
		readScalar(t, eng, "MATCH ()-[r]->() RETURN count(r) AS n")
}

// TestTCKShaped_MatchCreateWithMatchCreate_Relationships is the relationship
// analogue of Create3 [3].
//
// Seed: one relationship a-[:R]->b, so two nodes and one relationship.
//
//	MATCH ()-[:R]->()
//	CREATE ()-[:R]->()
//	WITH *
//	MATCH ()-[:R]->()
//	CREATE ()-[:R]->()
//
// Derivation. The first MATCH yields 1 row and its CREATE adds 2 nodes and 1
// relationship, so the graph holds 4 nodes and 2 relationships. WITH * carries
// that 1 row forward. The second MATCH must therefore see 2 relationships and
// yield 2 rows, whose CREATE adds 4 nodes and 2 relationships.
//
//	+nodes         = 2 + 4 = 6   (total 8)
//	+relationships = 1 + 2 = 3   (total 4)
//
// A second MATCH reading a frozen topology sees the seed relationship only,
// yields 1 row, and produces +nodes 4 (total 6) and +relationships 2 (total 3) —
// so both counts discriminate, in the same way Create3 [3]'s 10-vs-6 does.
func TestTCKShaped_MatchCreateWithMatchCreate_Relationships(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	autocommit(t, eng, "CREATE (:A)-[:R]->(:B)")

	if n, r := sideEffects(t, eng); n != 2 || r != 1 {
		t.Fatalf("seed graph has %d nodes / %d relationships, want 2 / 1", n, r)
	}

	autocommit(t, eng, `
		MATCH ()-[:R]->()
		CREATE ()-[:R]->()
		WITH *
		MATCH ()-[:R]->()
		CREATE ()-[:R]->()`)

	nodes, rels := sideEffects(t, eng)
	if nodes != 8 {
		t.Errorf("total nodes = %d, want 8 (+6). Reaching only 6 (+4) means the second MATCH "+
			"did not observe the relationship the first CREATE made", nodes)
	}
	if rels != 4 {
		t.Errorf("total relationships = %d, want 4 (+3). Reaching only 3 (+2) means the second "+
			"MATCH expanded over a topology frozen before the statement ran", rels)
	}
}

// TestTCKShaped_MatchDeleteWithMatch_Relationships is the DELETE direction, which
// Create3 has no analogue for and which fails in the opposite way: a frozen
// topology makes a deleted relationship still traversable.
//
// Seed: two relationships. Deleting one and then matching must see one.
func TestTCKShaped_MatchDeleteWithMatch_Relationships(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	autocommit(t, eng, "CREATE (:A)-[:R]->(:B) CREATE (:C)-[:R]->(:D)")

	if _, r := sideEffects(t, eng); r != 2 {
		t.Fatalf("seed graph has %d relationships, want 2", r)
	}

	got := writeScalar(t, eng, `
		MATCH ()-[r:R]->()
		WITH r LIMIT 1
		DELETE r
		WITH count(*) AS _
		MATCH ()-[s:R]->()
		RETURN count(s) AS n`)
	if got != 1 {
		t.Errorf("the later clause traversed %d relationships, want 1: the one this statement "+
			"deleted must not still be reachable", got)
	}
	if _, r := sideEffects(t, eng); r != 1 {
		t.Errorf("after the statement the graph holds %d relationships, want 1: the DELETE "+
			"itself did not take effect", r)
	}
}
