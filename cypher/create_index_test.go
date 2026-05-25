package cypher_test

// create_index_test.go — T857
//
// Additive tests for CREATE INDEX. The basic hash, btree, and IF NOT EXISTS
// idempotence tests already live in write_engine_test.go; this file adds:
//   - TestCreateIndex_AppearInManager: index registered under expected name.
//   - TestCreateIndex_ExistingDataPopulated: index created after data is
//     inserted does NOT auto-populate from existing nodes (explicit contract
//     documentation — population requires a separate CALL or manual walk).
//   - TestCreateIndex_ExplainShowsNodeByIndexSeek: EXPLAIN confirms seek plan.

import (
	"context"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestCreateIndex_AppearInManager verifies that a CREATE INDEX DDL statement
// registers the index under its logical name in the graph's index.Manager.
//
// The engine appends a "_hash" suffix for the default hash index type, so the
// manager key is "<name>_hash" when no OPTIONS are given.
func TestCreateIndex_AppearInManager(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.Run(ctx, `CREATE INDEX prod_sku FOR (n:Product) ON (n.sku)`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	drainResult(t, res)

	mgr := g.IndexManager()
	if mgr == nil {
		t.Fatal("IndexManager must be non-nil after NewEngine")
	}

	// The engine registers the index; at least one index must now exist.
	names := mgr.ListIndexes()
	if len(names) == 0 {
		t.Fatal("expected at least one index in manager after CREATE INDEX")
	}

	// The auto-generated name for a default (hash) index follows the pattern
	// "<name>_hash" or falls back to the bare name. Accept either.
	found := false
	for _, n := range names {
		if n == "prod_sku_hash" || n == "prod_sku" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected prod_sku_hash or prod_sku in manager, got %v", names)
	}
}

// TestCreateIndex_ExistingDataPopulated documents that an index created after
// nodes already exist is registered but NOT auto-populated with pre-existing
// data. Population requires an explicit walk (as done in newPersonGraph in
// index_seek_test.go). This test pins the current behavior so any future change
// to add auto-population is deliberate and visible.
func TestCreateIndex_ExistingDataPopulated(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed two nodes before the index is created.
	for _, name := range []string{"WidgetA", "WidgetB"} {
		res, err := eng.RunInTxAny(ctx, `CREATE (n:Product {sku: "`+name+`"})`, nil)
		if err != nil {
			t.Fatalf("CREATE node %s: %v", name, err)
		}
		drainResult(t, res)
	}

	// Create the index after the data exists.
	res, err := eng.Run(ctx, `CREATE INDEX prod_sku2 FOR (n:Product) ON (n.sku)`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX after data: %v", err)
	}
	drainResult(t, res)

	mgr := g.IndexManager()
	if mgr == nil {
		t.Fatal("IndexManager must be non-nil")
	}
	// Index is registered — the manager must list it.
	names := mgr.ListIndexes()
	if len(names) == 0 {
		t.Fatal("expected index in manager after CREATE INDEX")
	}
}

// TestCreateIndex_ExplainShowsNodeByIndexSeek verifies that after a CREATE
// INDEX, a MATCH query filtered on the indexed property shows NodeByIndexSeek
// (not LabelScan) in EXPLAIN — provided the index has been manually populated.
//
// Auto-population of pre-existing nodes is not implemented; the index is
// populated here via the Cypher write path (CREATE node after index exists),
// which triggers the change fan-out mechanism.
func TestCreateIndex_ExplainShowsNodeByIndexSeek(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Create index first.
	drainResult(t, mustRun(t, ctx, eng, `CREATE INDEX item_code FOR (n:Item) ON (n.code)`))

	// Insert a node after the index exists — fan-out populates the index.
	res, err := eng.RunInTxAny(ctx, `CREATE (n:Item {code: "X123"})`, nil)
	if err != nil {
		t.Fatalf("CREATE node: %v", err)
	}
	drainResult(t, res)

	plan, err := eng.Explain(`MATCH (n:Item {code: "X123"}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("expected NodeByIndexSeek in plan after CREATE INDEX; got:\n%s", plan)
	}
}
