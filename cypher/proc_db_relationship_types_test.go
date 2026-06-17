package cypher_test

// proc_db_relationship_types_test.go — tests for db.relationshipTypes()
// procedure (task-894, wired in task-1578).
//
// db.relationshipTypes() yields one row per distinct relationship type borne
// by an edge with both endpoints live, sourced from the engine graph via
// lpg.Graph.RelationshipTypesInUse. Order is unspecified.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// TestProcDbRelationshipTypes_Empty verifies that CALL db.relationshipTypes()
// returns zero rows on a graph with no edges.
func TestProcDbRelationshipTypes_Empty(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`CALL db.relationshipTypes() YIELD relationshipType`, nil)
	if err != nil {
		t.Fatalf("CALL db.relationshipTypes(): %v", err)
	}
	rows := collectProc(t, res)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty graph, got %d: %v", len(rows), rows)
	}
}

// TestProcDbRelationshipTypes_AfterCreatingEdges seeds typed edges into the
// engine's graph and verifies that CALL db.relationshipTypes() yields exactly
// the distinct relationship types in use, one per row in the single
// relationshipType column. Order is unspecified, so the result is asserted as
// a set. KNOWS is attached to two edges to confirm the result is de-duplicated.
func TestProcDbRelationshipTypes_AfterCreatingEdges(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g) // installs the index.Manager on g

	// Seed nodes and typed edges directly on the graph; db.relationshipTypes()
	// reads the engine graph's live edge-label shards via RelationshipTypesInUse.
	for _, n := range []string{"alice", "bob", "acme"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}
	for _, e := range [][2]string{{"alice", "bob"}, {"alice", "acme"}} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%q,%q): %v", e[0], e[1], err)
		}
	}
	// KNOWS is borne by two distinct edges to confirm de-duplication.
	g.SetEdgeLabel("alice", "bob", "KNOWS")
	g.SetEdgeLabel("alice", "bob", "LIKES")
	g.SetEdgeLabel("alice", "acme", "KNOWS")
	g.SetEdgeLabel("alice", "acme", "WORKS_WITH")

	res, err := eng.Run(context.Background(),
		`CALL db.relationshipTypes() YIELD relationshipType`, nil)
	if err != nil {
		t.Fatalf("CALL db.relationshipTypes(): %v", err)
	}
	rows := collectProc(t, res)

	got := make(map[string]int, len(rows))
	for i, row := range rows {
		v, ok := row["relationshipType"]
		if !ok {
			t.Errorf("row[%d] missing 'relationshipType' column", i)
			continue
		}
		got[v]++
	}
	want := []string{"KNOWS", "LIKES", "WORKS_WITH"}
	if len(rows) != len(want) {
		t.Fatalf("expected %d distinct relationship-type rows, got %d: %v", len(want), len(rows), rows)
	}
	for _, w := range want {
		if got[w] != 1 {
			t.Errorf("relationship type %q appeared %d times, want exactly 1", w, got[w])
		}
	}
}
