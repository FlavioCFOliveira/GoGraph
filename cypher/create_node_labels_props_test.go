package cypher_test

// create_node_labels_props_test.go — T761
//
// Additive coverage for CREATE with multiple labels and all supported property
// types. Complements TestRunInTx_CreateNode which covers the single-label,
// single-string-property baseline.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestCreate_MultipleLabels verifies that CREATE with two colon-separated
// labels registers both in the registry and places the node in both label
// indexes.
func TestCreate_MultipleLabels(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (n:Person:Employee {name: "Alice"})`, nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	_ = drainRecords(t, res)

	for _, label := range []string{"Person", "Employee"} {
		lid, ok := g.Registry().Lookup(label)
		if !ok {
			t.Errorf("label %q not registered after CREATE", label)
			continue
		}
		bm := g.NodeIndex().Intersect(uint32(lid))
		if bm.IsEmpty() {
			t.Errorf("no node with label %q in node index after CREATE", label)
		}
	}
}

// TestCreate_AllPropertyTypes verifies that CREATE correctly persists nodes
// with string, int64, float64, and bool property values.
func TestCreate_AllPropertyTypes(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx,
		`CREATE (n:Item {name: "widget", qty: 42, price: 9.99, active: true})`, nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	_ = drainRecords(t, res)

	// Walk all nodes and locate the one with name "widget".
	type propSnapshot struct {
		name   string
		qty    int64
		price  float64
		active bool
		found  bool
	}
	var snap propSnapshot
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		props := g.NodeProperties(key)
		ns, ok := props["name"]
		if !ok {
			return true
		}
		sv, ok := ns.String()
		if !ok || sv != "widget" {
			return true
		}
		snap.found = true
		snap.name = sv

		if qv, ok2 := props["qty"]; ok2 {
			snap.qty, _ = qv.Int64()
		}
		if pv, ok2 := props["price"]; ok2 {
			snap.price, _ = pv.Float64()
		}
		if av, ok2 := props["active"]; ok2 {
			snap.active, _ = av.Bool()
		}
		return false
	})
	if !snap.found {
		t.Fatal("node with name=widget not found after CREATE")
	}
	if snap.qty != 42 {
		t.Errorf("qty = %d, want 42", snap.qty)
	}
	if snap.price != 9.99 {
		t.Errorf("price = %f, want 9.99", snap.price)
	}
	if !snap.active {
		t.Errorf("active = %v, want true", snap.active)
	}
}

// TestCreate_ThenMatchVerifies verifies that after CREATE the node is visible
// through a subsequent MATCH query and that the name property round-trips.
func TestCreate_ThenMatchVerifies(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (n:City {name: "Lisbon"})`, nil)
	if err != nil {
		t.Fatalf("CREATE RunInTx: %v", err)
	}
	_ = drainRecords(t, res)

	// MATCH by label and return the property.
	res2, err := eng.Run(ctx, `MATCH (n:City) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("MATCH Run: %v", err)
	}
	rows := collectRecords(t, res2)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	raw := rows[0]["n.name"]
	if raw == nil {
		t.Fatal("n.name is nil in result")
	}
	if got := fmtAny(raw); got != `"Lisbon"` {
		t.Errorf("n.name = %s, want \"Lisbon\"", got)
	}
}
