package cypher_test

// relationship_identity_test.go — rmp #2317 acceptance criterion 3 and rmp #2334.
//
// Layer: short.
//
// # One identity, so one answer
//
// A relationship's emitted identity is its stable HANDLE. It used to be the
// absolute position of the edge in a rebuilt CSR, and that had two consequences,
// one latent and one live.
//
// The latent one: a position indexes ONE adjacency, and since rmp #2317 the
// adjacency is resolved per Init rather than once at plan-build time, so a position
// emitted by one Init can name a different edge in the next.
//
// The live one is what these tests gate. A relationship has two property stores — a
// per-pair store and a per-instance by-handle store — and which one was written, and
// which one was read, both depended on whether the row still carried an edge
// POSITION. Straight off Expand it did, so a write resolved the position to a handle
// and went by-handle. After a WITH the projection replaces the triplet with a
// self-describing RelationshipValue that carried no position, so the write fell back
// to the per-pair store, which the direct read path never consults.
//
// The same edge then reported two different values depending on the shape of the
// query that asked. Measured before the fix, after `WITH r SET r.k = 2`:
//
//	MATCH (:A)-[r:R]->(:B) RETURN r.k              -> 1   stale
//	MATCH (:A)-[r:R]->(:B) WITH r RETURN r.k       -> 2   the write is here
//	MATCH (:A)-[r:R]->(:B) RETURN properties(r).k  -> 1   stale
//
// No error was raised on any of it.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// multigraphEngine builds an engine over a MULTIGRAPH, which parallel relationships
// require: the default fixture is a simple graph and rejects a second edge between
// the same pair.
func multigraphEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	return cypher.NewEngine(g)
}

// TestRelationshipIdentity_EveryWriteShapeAgreesWithEveryReadShape is the
// cross-product gate: whichever shape writes a relationship property, every shape
// that reads it must see the same value.
//
// The cross-product is the point. Testing one write against one read would have
// passed throughout the defect — the two stores each answered consistently for the
// shape that wrote them, and only the pairing disagreed.
func TestRelationshipIdentity_EveryWriteShapeAgreesWithEveryReadShape(t *testing.T) {
	t.Parallel()
	writes := []struct{ name, stmt string }{
		{"no barrier", "MATCH (a:A)-[r:R]->(b:B) SET r.k = 2"},
		{"WITH r", "MATCH (a:A)-[r:R]->(b:B) WITH r SET r.k = 2"},
		{"WITH *", "MATCH (a:A)-[r:R]->(b:B) WITH * SET r.k = 2"},
		{"WITH r AS q", "MATCH (a:A)-[r:R]->(b:B) WITH r AS q SET q.k = 2"},
		{"WITH a, r, b", "MATCH (a:A)-[r:R]->(b:B) WITH a, r, b SET r.k = 2"},
	}
	reads := []struct{ name, q string }{
		{"direct", "MATCH (:A)-[r:R]->(:B) RETURN r.k AS out"},
		{"after a barrier", "MATCH (:A)-[r:R]->(:B) WITH r RETURN r.k AS out"},
		{"properties()", "MATCH (:A)-[r:R]->(:B) RETURN properties(r).k AS out"},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			eng, _ := storelessEngineWithGraph(t)
			autocommit(t, eng, "CREATE (:A)-[:R {k: 1}]->(:B)")
			autocommit(t, eng, w.stmt)
			for _, r := range reads {
				if got := readScalar(t, eng, r.q); got != 2 {
					t.Errorf("write %q then read %q: got %d, want 2. A relationship has one "+
						"identity and therefore one property value; a disagreement between two "+
						"read shapes means the write and the read reached different stores",
						w.name, r.name, got)
				}
			}
		})
	}
}

// TestRelationshipIdentity_RemoveAndDeleteAfterABarrier covers the two operations
// that take the same resolution path as SET and were untested for the same reason.
func TestRelationshipIdentity_RemoveAndDeleteAfterABarrier(t *testing.T) {
	t.Parallel()
	t.Run("REMOVE after a barrier", func(t *testing.T) {
		t.Parallel()
		eng, _ := storelessEngineWithGraph(t)
		autocommit(t, eng, "CREATE (:A)-[:R {k: 1}]->(:B)")
		autocommit(t, eng, "MATCH (a:A)-[r:R]->(b:B) WITH r REMOVE r.k")
		for _, q := range []string{
			"MATCH (:A)-[r:R]->(:B) WHERE r.k IS NULL RETURN count(r) AS n",
			"MATCH (:A)-[r:R]->(:B) WITH r WHERE r.k IS NULL RETURN count(r) AS n",
		} {
			if got := readScalar(t, eng, q); got != 1 {
				t.Errorf("%q: got %d, want 1 — the REMOVE must be visible to every read shape", q, got)
			}
		}
	})
	t.Run("DELETE after a barrier", func(t *testing.T) {
		t.Parallel()
		eng, _ := storelessEngineWithGraph(t)
		autocommit(t, eng, "CREATE (:A)-[:R]->(:B)")
		autocommit(t, eng, "MATCH (a:A)-[r:R]->(b:B) WITH r DELETE r")
		if got := readScalar(t, eng, "MATCH (:A)-[r:R]->(:B) RETURN count(r) AS n"); got != 0 {
			t.Errorf("the relationship survived a DELETE issued after a WITH barrier: %d remain", got)
		}
	})
}

// TestRelationshipIdentity_SurvivesATopologyChangeInTheSameStatement is criterion
// 3's other half: the identity a row carries must keep naming the same edge after
// the statement changes the topology under it.
//
// A CSR position could not do this once the adjacency became per-Init, because a
// rebuild shifts positions; a handle is minted per slot and carried verbatim across
// the compaction that shifts them.
func TestRelationshipIdentity_SurvivesATopologyChangeInTheSameStatement(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	autocommit(t, eng, "CREATE (:A)-[:R {k: 1}]->(:B)")

	// Bind r, change the topology, then write THROUGH the binding.
	res, err := eng.RunInTx(context.Background(), `
		MATCH (a:A)-[r:R]->(b:B)
		CREATE (:X)-[:R]->(:Y)
		SET r.k = 2
		RETURN count(*) AS n`, nil)
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	for res.Next() { // intentional full drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := readScalar(t, eng, "MATCH (:A)-[r:R]->(:B) WHERE r.k = 2 RETURN count(r) AS n"); got != 1 {
		t.Errorf("the bound relationship no longer names the edge it was bound to: %d edges "+
			"carry the write, want 1", got)
	}
	// The edge the statement CREATED must not have been written through — that is
	// what a stale position does when the rebuild shifts it onto a different slot.
	if got := readScalar(t, eng, "MATCH (:X)-[r:R]->(:Y) WHERE r.k = 2 RETURN count(r) AS n"); got != 0 {
		t.Errorf("%d newly created edges were written through the stale binding, want 0", got)
	}
}

// TestRelationshipIdentity_IsStableAcrossParallelEdges pins that the identity
// distinguishes parallel instances, which is the property the per-pair store cannot
// express and the reason the by-handle store exists at all.
func TestRelationshipIdentity_IsStableAcrossParallelEdges(t *testing.T) {
	t.Parallel()
	eng := multigraphEngine(t)
	autocommit(t, eng, "CREATE (a:A), (b:B)")
	for i := 1; i <= 3; i++ {
		autocommit(t, eng, fmt.Sprintf("MATCH (a:A), (b:B) CREATE (a)-[:R {n: %d}]->(b)", i))
	}
	// Write through a barrier on the instance with n = 2 only.
	autocommit(t, eng, "MATCH (:A)-[r:R]->(:B) WHERE r.n = 2 WITH r SET r.tag = 9")

	if got := readScalar(t, eng, "MATCH (:A)-[r:R]->(:B) WHERE r.tag = 9 RETURN count(r) AS n"); got != 1 {
		t.Errorf("%d parallel instances carry the tag, want exactly 1: the write reached the "+
			"pair rather than the instance", got)
	}
	if got := readScalar(t, eng, "MATCH (:A)-[r:R]->(:B) WHERE r.tag = 9 AND r.n = 2 RETURN count(r) AS n"); got != 1 {
		t.Errorf("the tagged instance is not the one that was bound (n = 2): count %d", got)
	}
}
