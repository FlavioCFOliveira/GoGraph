package cypher_test

// proc_db_labels_test.go — engine-integration tests for db.labels() procedure
// (task-893, re-wired to live node labels in task-1580).
//
// db.labels() yields one row per distinct label currently attached to a live
// (non-tombstoned) node, sourced from the engine graph via
// lpg.Graph.NodeLabelsInUse. A label is listed regardless of whether an index
// exists for it, and is dropped once the last node bearing it is deleted. Order
// is unspecified.
//
// This replaces the previous index-derived behaviour, under which db.labels()
// listed only labels that had an explicitly registered label-kind index in the
// index.Manager — so a label attached to nodes without an index was invisible.
// The db.* introspection procedures are not covered by the openCypher TCK, so
// this is a deliberate, TCK-neutral behaviour change; see dbLabels in
// cypher/procs/builtin_db.go.
//
// Labels are seeded directly on the engine graph (the same approach the
// db.propertyKeys() integration test uses): the procedure reads the graph's
// live node-label state via NodeLabelsInUse, so a directly seeded graph
// exercises the same code path as Cypher writes.

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labelsInUse runs CALL db.labels() through the engine and returns the yielded
// labels as a set.
func labelsInUse(t *testing.T, eng *cypher.Engine) map[string]struct{} {
	t.Helper()
	res, err := eng.Run(context.Background(), `CALL db.labels() YIELD label`, nil)
	if err != nil {
		t.Fatalf("CALL db.labels(): %v", err)
	}
	rows := collectProc(t, res)
	set := make(map[string]struct{}, len(rows))
	for i, row := range rows {
		v, ok := row["label"]
		if !ok {
			t.Errorf("row[%d] missing 'label' column", i)
			continue
		}
		if _, dup := set[v]; dup {
			t.Errorf("duplicate label %q in result %v", v, rows)
		}
		set[v] = struct{}{}
	}
	return set
}

// TestProcDbLabels_EmptyGraph confirms zero rows when no nodes (and hence no
// labels) exist.
func TestProcDbLabels_EmptyGraph(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	rows := labelsInUse(t, eng)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty graph, got %d: %v", len(rows), rows)
	}
}

// TestProcDbLabels_AfterCreatingNodes is the key regression for task-1580:
// labels are listed even when no index is registered for them. Nodes are
// labelled directly on the engine graph (no index.Manager index is created),
// and db.labels() must still report every distinct label in use.
func TestProcDbLabels_AfterCreatingNodes(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g) // installs the index.Manager and wires Labels

	for _, n := range []string{"alice", "bob", "inception"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}
	// "Person" is borne by two nodes to confirm de-duplication.
	mustSetNodeLabel(t, g, "alice", "Person")
	mustSetNodeLabel(t, g, "bob", "Person")
	mustSetNodeLabel(t, g, "inception", "Movie")

	got := labelsInUse(t, eng)
	want := []string{"Person", "Movie"}
	if len(got) != len(want) {
		gotList := make([]string, 0, len(got))
		for l := range got {
			gotList = append(gotList, l)
		}
		sort.Strings(gotList)
		t.Fatalf("expected %d distinct labels, got %d: %v", len(want), len(got), gotList)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing expected label %q in %v", w, got)
		}
	}
}

// TestProcDbLabels_DroppedAfterDelete verifies the in-use semantics end-to-end:
// a label borne by exactly one node is no longer listed by db.labels() once
// that node is deleted (tombstoned). A label still borne by a surviving node
// ("Person") remains listed.
func TestProcDbLabels_DroppedAfterDelete(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	for _, n := range []string{"alice", "bob"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}
	// "Rare" is borne only by bob; "Person" is borne by both nodes.
	mustSetNodeLabel(t, g, "alice", "Person")
	mustSetNodeLabel(t, g, "bob", "Person")
	mustSetNodeLabel(t, g, "bob", "Rare")

	before := labelsInUse(t, eng)
	if _, ok := before["Rare"]; !ok {
		t.Fatalf("before delete: expected %q to be listed, got %v", "Rare", before)
	}
	if _, ok := before["Person"]; !ok {
		t.Fatalf("before delete: expected %q to be listed, got %v", "Person", before)
	}

	// Delete the only node bearing "Rare".
	g.RemoveNode("bob")

	after := labelsInUse(t, eng)
	if _, ok := after["Rare"]; ok {
		t.Errorf("after delete: %q must no longer be listed, got %v", "Rare", after)
	}
	if _, ok := after["Person"]; !ok {
		t.Errorf("after delete: %q must still be listed (borne by alice), got %v", "Person", after)
	}
}

// mustSetNodeLabel attaches a label to a node or fails the test.
func mustSetNodeLabel(t *testing.T, g *lpg.Graph[string, float64], n, label string) {
	t.Helper()
	if err := g.SetNodeLabel(n, label); err != nil {
		t.Fatalf("SetNodeLabel(%q, %q): %v", n, label, err)
	}
}
