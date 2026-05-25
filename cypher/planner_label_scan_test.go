package cypher_test

// planner_label_scan_test.go — T919: planner picks NodeByLabelScan (not
// AllNodesScan) when a label filter is present and no property index exists.
//
// AC1: EXPLAIN for "MATCH (n:Person) RETURN n" contains "NodeByLabelScan"
//
//	(not "AllNodesScan").
//
// AC2: "AllNodesScan" must NOT appear in the plan for a labelled query.
// AC3: race-clean (t.Parallel on all sub-tests).
// AC4: goleak-clean (enforced by TestMain in testmain_test.go).

import (
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestPlannerLabelScan_LabeledQueryUsesLabelScan verifies that a MATCH with a
// label filter uses NodeByLabelScan rather than the more expensive AllNodesScan.
func TestPlannerLabelScan_LabeledQueryUsesLabelScan(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(20, false /* no property index */)

	plan, err := eng.Explain(`MATCH (n:Person) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByLabelScan") {
		t.Errorf("expected NodeByLabelScan for labeled MATCH; got:\n%s", plan)
	}
}

// TestPlannerLabelScan_NotAllNodesScan asserts the complementary side: the
// planner must not degrade to AllNodesScan when a label is specified.
func TestPlannerLabelScan_NotAllNodesScan(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(20, false /* no property index */)

	plan, err := eng.Explain(`MATCH (n:Person) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if strings.Contains(plan, "AllNodesScan") {
		t.Errorf("planner must not use AllNodesScan when label is present; got:\n%s", plan)
	}
}

// TestPlannerLabelScan_UnlabeledQueryUsesAllNodesScan verifies the dual: a
// MATCH with no label filter (and no index) must use AllNodesScan, not
// NodeByLabelScan. This ensures the positive assertion above is non-trivial.
func TestPlannerLabelScan_UnlabeledQueryUsesAllNodesScan(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	// Add a bare node with no label so the planner cannot use a label scan.
	if err := g.AddNode("n1"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	plan, err := eng.Explain(`MATCH (n) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "AllNodesScan") {
		t.Errorf("expected AllNodesScan for unlabeled MATCH; got:\n%s", plan)
	}
}

// TestPlannerLabelScan_MultiLabelEachUsesLabelScan verifies that multiple
// independent labelled queries each resolve to NodeByLabelScan when the two
// labels are disjoint and no property index exists for either.
func TestPlannerLabelScan_MultiLabelEachUsesLabelScan(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	// Seed both labels via direct LPG API.
	for i, pair := range []struct{ key, label string }{
		{"p0", "Person"}, {"p1", "Person"},
		{"c0", "Car"}, {"c1", "Car"},
	} {
		key := pair.key
		_ = i
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, pair.label); err != nil {
			t.Fatalf("SetNodeLabel %s/%s: %v", key, pair.label, err)
		}
	}

	for _, label := range []string{"Person", "Car"} {
		q := "MATCH (n:" + label + ") RETURN n"
		plan, err := eng.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain %s: %v", label, err)
		}
		if !strings.Contains(plan, "NodeByLabelScan") {
			t.Errorf("label %s: expected NodeByLabelScan; got:\n%s", label, plan)
		}
		if strings.Contains(plan, "AllNodesScan") {
			t.Errorf("label %s: must not use AllNodesScan; got:\n%s", label, plan)
		}
	}
}

// TestPlannerLabelScan_LabelScanWithPropertyIndexPresent verifies that when a
// hash index is present on a property but the MATCH has no property predicate,
// the planner still uses NodeByLabelScan (not NodeByIndexSeek), because there
// is no equality filter to drive an index seek.
func TestPlannerLabelScan_LabelScanWithPropertyIndexPresent(t *testing.T) {
	t.Parallel()

	// Graph with property index installed but no equality filter in the query.
	_, eng := newPersonGraph(20, true /* withIndex */)

	plan, err := eng.Explain(`MATCH (n:Person) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	// No equality predicate means no NodeByIndexSeek.
	if strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("NodeByIndexSeek must not appear without equality predicate; got:\n%s", plan)
	}
	if !strings.Contains(plan, "NodeByLabelScan") {
		t.Errorf("expected NodeByLabelScan for MATCH with label only; got:\n%s", plan)
	}
}
