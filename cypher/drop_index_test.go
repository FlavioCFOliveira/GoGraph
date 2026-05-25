package cypher_test

// drop_index_test.go — T859
//
// Additive tests for DROP INDEX. The basic drop and IF EXISTS idempotence
// tests already live in write_engine_test.go; this file adds:
//   - TestDropIndex_ThenReCreate: DROP then CREATE same name succeeds.
//   - TestDropIndex_ExplainShowsLabelScan: EXPLAIN reverts to LabelScan after drop.

import (
	"context"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestDropIndex_ThenReCreate verifies that after DROP INDEX the name is freed
// and a subsequent CREATE INDEX with the same name succeeds.
func TestDropIndex_ThenReCreate(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Create the index.
	drainResult(t, mustRun(t, ctx, eng, `CREATE INDEX recyclable FOR (n:T) ON (n.x)`))

	// Verify it exists.
	if _, err := g.IndexManager().GetIndex("recyclable_hash"); err != nil {
		// Try bare name fallback.
		if _, err2 := g.IndexManager().GetIndex("recyclable"); err2 != nil {
			t.Fatalf("expected index in manager after CREATE: %v / %v", err, err2)
		}
	}

	// Drop it.
	drainResult(t, mustRun(t, ctx, eng, `DROP INDEX recyclable`))

	// Manager must not contain the index any more.
	mgr := g.IndexManager()
	for _, n := range mgr.ListIndexes() {
		if n == "recyclable" || n == "recyclable_hash" {
			t.Errorf("index %q still present after DROP INDEX", n)
		}
	}

	// Re-create with the same name — must succeed.
	res, err := eng.Run(ctx, `CREATE INDEX recyclable FOR (n:T) ON (n.x)`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX after DROP: %v", err)
	}
	drainResult(t, res)

	// Index must be back.
	names := mgr.ListIndexes()
	found := false
	for _, n := range names {
		if n == "recyclable" || n == "recyclable_hash" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected index to reappear after re-create, got %v", names)
	}
}

// TestDropIndex_ExplainShowsLabelScan verifies that after DROP INDEX, EXPLAIN
// for a filtered MATCH no longer shows NodeByIndexSeek and reverts to either
// LabelScan or Selection, because the planner re-probes the index manager on
// every Explain call.
func TestDropIndex_ExplainShowsLabelScan(t *testing.T) {
	t.Parallel()

	// Use the newPersonGraph helper from index_seek_test.go: it builds a graph
	// with a populated hash index on Person.name.
	g, eng := newPersonGraph(5, true /* withIndex */)

	// Before drop: NodeByIndexSeek expected.
	planBefore, err := eng.Explain(`MATCH (n:Person {name: "Alice"}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain before drop: %v", err)
	}
	if !strings.Contains(planBefore, "NodeByIndexSeek") {
		t.Fatalf("expected NodeByIndexSeek before drop; got:\n%s", planBefore)
	}

	// Drop the index directly via the manager.
	if err := g.IndexManager().DropIndex("person_name_hash"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}

	// After drop: plan must revert to LabelScan or Selection.
	planAfter, err := eng.Explain(`MATCH (n:Person {name: "Alice"}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain after drop: %v", err)
	}
	if strings.Contains(planAfter, "NodeByIndexSeek") {
		t.Errorf("NodeByIndexSeek still present after drop; plan:\n%s", planAfter)
	}
	if !strings.Contains(planAfter, "LabelScan") && !strings.Contains(planAfter, "Selection") {
		t.Errorf("expected LabelScan or Selection after drop; got:\n%s", planAfter)
	}
}

// TestDropIndex_NonExistentWithoutIfExists verifies that DROP INDEX on a
// non-existent index (without IF EXISTS) returns an error. This documents the
// strict error contract so callers know they must use IF EXISTS for idempotent
// operations.
func TestDropIndex_NonExistentWithoutIfExists(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	_ = g // suppress unused warning

	_, err := eng.Run(ctx, `DROP INDEX ghost_index`, nil)
	if err == nil {
		t.Fatal("expected error when dropping non-existent index without IF EXISTS")
	}
}
