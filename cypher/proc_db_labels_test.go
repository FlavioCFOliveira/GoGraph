package cypher_test

// proc_db_labels_test.go — additive tests for db.labels() procedure (task-893).
//
// db.labels() returns one row per index registered in the index.Manager with
// Kind() == "label". Labels attached to nodes via SetNodeLabel or Cypher
// CREATE use the LPG's internal nodeIdx, which is NOT the same index.Manager
// used by db.labels(). Consequently, db.labels() returns rows only when a
// label.Index has been explicitly registered in the index.Manager under the
// label name.

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	indexlabel "github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// TestProcDbLabels_AfterCreatingNodes verifies that db.labels() returns the
// distinct label names registered as label-kind indexes in the index.Manager.
//
// NOTE: Labels set via g.SetNodeLabel or Cypher CREATE are stored in the
// LPG's internal nodeIdx and are invisible to db.labels(). Only indexes
// registered in the index.Manager with Kind()=="label" are reported.
func TestProcDbLabels_AfterCreatingNodes(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g) // installs the index.Manager on g
	mgr := g.IndexManager()

	// Register three label-kind indexes directly into the index.Manager.
	// This is the only mechanism through which db.labels() can observe labels.
	for _, name := range []string{"Person", "Movie", "Admin"} {
		if err := mgr.CreateIndex(name, indexlabel.NewIndex()); err != nil {
			t.Fatalf("CreateIndex(%q): %v", name, err)
		}
	}

	res, err := eng.Run(context.Background(), `CALL db.labels() YIELD label`, nil)
	if err != nil {
		t.Fatalf("CALL db.labels(): %v", err)
	}
	rows := collectProc(t, res)

	if len(rows) != 3 {
		t.Fatalf("expected 3 label rows, got %d: %v", len(rows), rows)
	}

	got := make([]string, 0, len(rows))
	for _, row := range rows {
		lbl, ok := row["label"]
		if !ok {
			t.Errorf("row missing 'label' column: %v", row)
			continue
		}
		got = append(got, lbl)
	}
	sort.Strings(got)
	want := []string{"Admin", "Movie", "Person"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("labels[%d]: got %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

// TestProcDbLabels_OrderedOutput verifies that combining CALL db.labels() with
// YIELD and ORDER BY returns labels in alphabetical order. The ordering is
// performed by the Cypher ORDER BY clause, not by db.labels() itself (which
// returns results in unspecified order).
func TestProcDbLabels_OrderedOutput(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)
	mgr := g.IndexManager()

	for _, name := range []string{"Zebra", "Apple", "Mango"} {
		if err := mgr.CreateIndex(name, indexlabel.NewIndex()); err != nil {
			t.Fatalf("CreateIndex(%q): %v", name, err)
		}
	}

	res, err := eng.Run(context.Background(),
		`CALL db.labels() YIELD label RETURN label ORDER BY label`, nil)
	if err != nil {
		t.Fatalf("CALL db.labels() YIELD label RETURN label ORDER BY label: %v", err)
	}
	rows := collectProc(t, res)

	if len(rows) != 3 {
		t.Fatalf("expected 3 ordered label rows, got %d: %v", len(rows), rows)
	}
	want := []string{"Apple", "Mango", "Zebra"}
	for i, w := range want {
		got := rows[i]["label"]
		if got != w {
			t.Errorf("labels[%d]: got %q, want %q", i, got, w)
		}
	}
}

// TestProcDbLabels_EmptyGraph confirms zero rows when no label-kind indexes are
// registered. Additive to TestProcsEngine_DbLabels_Empty in
// procs_engine_test.go; kept as a regression guard for changes introduced by
// the other tests in this file.
func TestProcDbLabels_EmptyGraph(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(), `CALL db.labels() YIELD label`, nil)
	if err != nil {
		t.Fatalf("CALL db.labels(): %v", err)
	}
	rows := collectProc(t, res)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty graph, got %d: %v", len(rows), rows)
	}
}
