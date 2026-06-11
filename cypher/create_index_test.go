package cypher_test

// create_index_test.go — T857
//
// Additive tests for CREATE INDEX. The basic hash, btree, and IF NOT EXISTS
// idempotence tests already live in write_engine_test.go; this file adds:
//   - TestCreateIndex_AppearInManager: index registered under expected name.
//   - TestCreateIndex_ExistingDataPopulated: index created after data is
//     inserted IS backfilled from the existing nodes (task #1340); the full
//     backfill battery lives in create_index_backfill_test.go.
//   - TestCreateIndex_ExplainShowsNodeByIndexSeek: EXPLAIN confirms seek plan.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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

// TestCreateIndex_ExistingDataPopulated verifies that an index created after
// nodes already exist is registered AND backfilled from the pre-existing data
// (task #1340): a query on the indexed predicate finds the seeded nodes. The
// full backfill battery (updates, deletes, label changes, wrong-label
// fallback, concurrency) lives in create_index_backfill_test.go.
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

	// The pre-existing nodes are backfilled: the indexed predicate finds them.
	for _, name := range []string{"WidgetA", "WidgetB"} {
		qres, err := eng.Run(ctx, `MATCH (n:Product {sku: "`+name+`"}) RETURN n.sku`, nil)
		if err != nil {
			t.Fatalf("MATCH %s: %v", name, err)
		}
		rows := collectRecords(t, qres)
		_ = qres.Close()
		if len(rows) != 1 {
			t.Fatalf("backfilled node %s: want 1 row, got %d", name, len(rows))
		}
	}
}

// TestCreateIndex_ExplainShowsNodeByIndexSeek verifies that after a CREATE
// INDEX, a MATCH query filtered on the indexed property shows NodeByIndexSeek
// (not LabelScan) in EXPLAIN. The node is inserted after the index exists, so
// the change fan-out (the bound index's Apply, task #1340) populates it.
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
