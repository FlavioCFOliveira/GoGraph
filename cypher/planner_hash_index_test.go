package cypher_test

// planner_hash_index_test.go — T916: planner picks NodeByIndexSeek (hash index
// seek) for equality predicates on an indexed property.
//
// AC1: EXPLAIN output contains "NodeByIndexSeek" for an equality predicate
//
//	when a hash index exists on the property.
//
// AC2: race-clean (t.Parallel on all sub-tests).
// AC3: goleak-clean (enforced by TestMain in testmain_test.go).

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestPlannerHashIndex_EqualityUsesIndexSeek verifies that an equality
// predicate on a property covered by a hash index produces a NodeByIndexSeek
// plan leaf (not NodeByLabelScan or AllNodesScan).
func TestPlannerHashIndex_EqualityUsesIndexSeek(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(100, true /* withIndex */)

	plan, err := eng.Explain(`MATCH (n:Person {name: "Alice"}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("expected NodeByIndexSeek for equality predicate with hash index; got:\n%s", plan)
	}
}

// TestPlannerHashIndex_NoIndexKeepsLabelScan verifies the control case:
// without a hash index the planner falls back to NodeByLabelScan (or
// Selection over it). This ensures the positive assertion is meaningful.
func TestPlannerHashIndex_NoIndexKeepsLabelScan(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(100, false /* withIndex */)

	plan, err := eng.Explain(`MATCH (n:Person {name: "Alice"}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("unexpected NodeByIndexSeek without hash index; got:\n%s", plan)
	}
	if !strings.Contains(plan, "LabelScan") && !strings.Contains(plan, "Selection") {
		t.Errorf("expected NodeByLabelScan or Selection without index; got:\n%s", plan)
	}
}

// TestPlannerHashIndex_EqualityReturnsCorrectRows verifies end-to-end
// correctness: the seek plan not only appears in EXPLAIN but also returns the
// right node data.
func TestPlannerHashIndex_EqualityReturnsCorrectRows(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(100, true /* withIndex */)

	res, err := eng.Run(context.Background(),
		`MATCH (n:Person {name: "Alice"}) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	v, ok := rows[0]["n.name"].(expr.StringValue)
	if !ok {
		t.Fatalf("n.name: expected StringValue, got %T (%v)", rows[0]["n.name"], rows[0]["n.name"])
	}
	if string(v) != "Alice" {
		t.Errorf("n.name: want Alice, got %q", string(v))
	}
}

// TestPlannerHashIndex_SameEngineMultipleQueries verifies that the same engine
// instance correctly selects NodeByIndexSeek for repeated equality queries on
// the indexed property, exercising plan-cache interactions.
func TestPlannerHashIndex_SameEngineMultipleQueries(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(50, true /* withIndex */)

	for _, name := range []string{"Alice", "Person0", "Person49"} {
		q := `MATCH (n:Person {name: "` + name + `"}) RETURN n`
		plan, err := eng.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain %q: %v", name, err)
		}
		if !strings.Contains(plan, "NodeByIndexSeek") {
			t.Errorf("name=%q: expected NodeByIndexSeek; got:\n%s", name, plan)
		}
	}
}

// TestPlannerHashIndex_DDLCreatedIndex verifies that an index created via DDL
// (CREATE INDEX … FOR (n:Label) ON (n.prop)) is also selected by the planner
// for equality predicates once a node is inserted after the index exists.
func TestPlannerHashIndex_DDLCreatedIndex(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Create the index before inserting data so that fan-out populates it.
	drainResult(t, mustRun(t, ctx, eng,
		`CREATE INDEX widget_code FOR (n:Widget) ON (n.code)`))

	// Insert a node after the index exists — the change fan-out populates it.
	res, err := eng.RunInTxAny(ctx, `CREATE (n:Widget {code: "W42"})`, nil)
	if err != nil {
		t.Fatalf("CREATE node: %v", err)
	}
	drainResult(t, res)

	plan, err := eng.Explain(`MATCH (n:Widget {code: "W42"}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("expected NodeByIndexSeek after DDL CREATE INDEX; got:\n%s", plan)
	}
}
