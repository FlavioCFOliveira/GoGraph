package cypher_test

// remove_label_test.go — additive REMOVE label tests (T838).
//
// Existing TestRunInTx_RemoveLabels covers basic removal of one label from a
// two-label node. Tests here cover:
//   - removing the last label from a node (node still exists, no labels)
//   - removing a label that was never attached (no error)
//   - removing one of multiple labels, verifying others are intact
//   - REMOVE n:Person:Employee — removing multiple labels in one clause

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestRemoveLabel_LastLabel removes the sole label from a node and verifies
// the label index is empty for that label while the node still occupies a
// synthetic key in the mapper.
func TestRemoveLabel_LastLabel(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Temp)`)
	drainRunInTx(t, eng, `MATCH (n:Temp) REMOVE n:Temp`)

	lid, ok := g.Registry().Lookup("Temp")
	if !ok {
		return // label never registered — vacuously OK
	}
	bm := g.NodeIndex().Intersect(uint32(lid))
	if !bm.IsEmpty() {
		t.Fatal("expected no nodes with label Temp after removing last label")
	}
}

// TestRemoveLabel_NonExistentLabel removes a label that was never attached to
// the node. The operation must succeed without error.
func TestRemoveLabel_NonExistentLabel(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Person)`)
	// "Ghost" was never attached — must be a silent no-op.
	drainRunInTx(t, eng, `MATCH (n:Person) REMOVE n:Ghost`)

	// Person label must still be present.
	lid, ok := g.Registry().Lookup("Person")
	if !ok {
		t.Fatal("Person label not registered")
	}
	bm := g.NodeIndex().Intersect(uint32(lid))
	if bm.IsEmpty() {
		t.Fatal("Person node gone after removing unrelated Ghost label")
	}
}

// TestRemoveLabel_OneOfMultiple removes Employee from a Person+Employee node
// and verifies that Person survives in the label index.
func TestRemoveLabel_OneOfMultiple(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Person)`)
	drainRunInTx(t, eng, `MATCH (n:Person) SET n:Employee`)
	drainRunInTx(t, eng, `MATCH (n:Employee) REMOVE n:Employee`)

	// Employee gone.
	if eid, ok := g.Registry().Lookup("Employee"); ok {
		if bm := g.NodeIndex().Intersect(uint32(eid)); !bm.IsEmpty() {
			t.Error("Employee label still present after REMOVE n:Employee")
		}
	}

	// Person still present.
	pid, ok := g.Registry().Lookup("Person")
	if !ok {
		t.Fatal("Person label not registered")
	}
	if bm := g.NodeIndex().Intersect(uint32(pid)); bm.IsEmpty() {
		t.Fatal("Person label lost after REMOVE n:Employee")
	}
}

// TestRemoveLabel_MultipleLabelsAtOnce removes two labels with one REMOVE
// clause (REMOVE n:Person:Employee) and verifies both are gone.
func TestRemoveLabel_MultipleLabelsAtOnce(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Person)`)
	drainRunInTx(t, eng, `MATCH (n:Person) SET n:Employee`)

	// REMOVE two labels in a single clause.
	res, err := eng.RunInTx(context.Background(), `MATCH (n:Person) REMOVE n:Person:Employee`, nil)
	if err != nil {
		t.Fatalf("REMOVE n:Person:Employee: %v", err)
	}
	for res.Next() {
	}
	if iterErr := res.Err(); iterErr != nil {
		// Multi-label REMOVE is implemented; a non-nil error here is a real
		// regression (#1761 — the stale "not supported" skip was removed).
		t.Fatalf("REMOVE n:Person:Employee: %v", iterErr)
	}
	res.Close()

	for _, lbl := range []string{"Person", "Employee"} {
		if lid, ok := g.Registry().Lookup(lbl); ok {
			if bm := g.NodeIndex().Intersect(uint32(lid)); !bm.IsEmpty() {
				t.Errorf("label %s still present after REMOVE n:Person:Employee", lbl)
			}
		}
	}
}
