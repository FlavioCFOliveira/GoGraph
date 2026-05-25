package cypher_test

// set_property_test.go — T822
//
// Additive tests for SET on existing nodes. Complements
// TestRunInTx_SetProperty_SingleKey which verifies a basic name-overwrite.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestSet_NonExistentPropertyIsCreated verifies that SET creates a property
// that did not exist on the node before.
func TestSet_NonExistentPropertyIsCreated(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Node created without an email property.
	drainRunInTx(t, eng, `CREATE (n:Person {name: "Dave"})`)
	// SET adds the new property.
	drainRunInTx(t, eng, `MATCH (n:Person) SET n.email = "dave@example.com"`)

	// Verify via graph walk.
	found := false
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		props := g.NodeProperties(key)
		if ev, ok := props["email"]; ok {
			if sv, ok2 := ev.String(); ok2 && sv == "dave@example.com" {
				found = true
				return false
			}
		}
		return true
	})
	if !found {
		t.Fatal("expected email=dave@example.com after SET on node that lacked email")
	}
}

// TestSet_FoundViaLabelScan verifies that a node found by a label scan can
// have its properties updated via SET.
func TestSet_FoundViaLabelScan(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Sensor {id: 1, value: 0})`)
	drainRunInTx(t, eng, `MATCH (n:Sensor) SET n.value = 42`)

	// Verify via RETURN.
	res, err := eng.Run(ctx, `MATCH (n:Sensor) RETURN n.value`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if got := fmtAny(rows[0]["n.value"]); got != "42" {
		t.Errorf("n.value = %s, want 42", got)
	}
}

// TestSet_PropertyVisibleViaReturn verifies that a property written by SET is
// immediately visible in a subsequent RETURN within the same query.
func TestSet_PropertyVisibleViaReturn(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Counter {count: 0})`)

	res, err := eng.RunInTx(ctx,
		`MATCH (n:Counter) SET n.count = 7 RETURN n.count`, nil)
	if err != nil {
		t.Fatalf("RunInTx SET+RETURN: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row from SET+RETURN, got %d", len(rows))
	}
	if got := fmtAny(rows[0]["n.count"]); got != "7" {
		t.Errorf("n.count in RETURN = %s, want 7", got)
	}
}
