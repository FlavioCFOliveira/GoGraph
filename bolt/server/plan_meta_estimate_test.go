package server

// plan_meta_estimate_test.go — the planner's cardinality estimate on the Bolt wire
// (rmp #2765, closing audit divergence D9).
//
// Before this, a driver's `ResultSummary.Plan()` received operator names and
// details and NO NUMBERS AT ALL: an EXPLAIN publishes no measurements by
// construction, so with no estimate published there was nothing quantitative in
// the message. The estimate is what an un-run plan has to say, and it is what a
// driver needs in order to show why a plan was chosen.
//
// It is asserted against the metadata MAP rather than through the driver, for the
// same reason the `dbHits` omission is (plan_meta_dbhits_test.go): the driver reads
// `args` wholesale into ProfiledPlan.Arguments, so it can show a key is delivered
// but not that an absent one is absent. The presence/absence distinction lives in
// the map this server builds.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// planMetaArgs returns the `args` sub-map of a rendered plan node, or nil when the
// node published none.
func planMetaArgs(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	raw, present := m[planKeyArgs]
	if !present {
		return nil
	}
	args, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want a map", planKeyArgs, raw)
	}
	return args
}

// TestPlanNodeMetadata_PublishesTheEstimateAndOmitsItWhereThereIsNone is the
// acceptance gate for D9: an operator the planner estimated publishes the figure
// and its provenance; one it did not publishes NEITHER key.
//
// Both directions are asserted. A server that never sent the keys would satisfy
// the omission alone while delivering none of the feature, and a server that sent
// a 0 for every un-estimated operator would satisfy the presence alone while
// making every driver believe the planner predicted no rows — which is the exact
// failure mode rmp #2760 removed from `dbHits` and Neo4j removes by dropping the
// argument when it holds its NO_DATA sentinel.
func TestPlanNodeMetadata_PublishesTheEstimateAndOmitsItWhereThereIsNone(t *testing.T) {
	t.Parallel()

	tree := exec.PlanNode{
		Name: "Project",
		// No logical node the planner estimates lowers to a final projection.
		Children: []exec.PlanNode{{
			Name: "NodeByLabelScan", Detail: "Person",
			Est: exec.PlanEstimate{Rows: 2000, Source: exec.EstimateExact},
		}},
	}

	m := planNodeMetadata(&tree, false)

	if args := planMetaArgs(t, m); args != nil {
		if v, present := args[planArgEstimatedRows]; present {
			t.Errorf("the un-estimated Project published %s=%v. No logical node the "+
				"planner estimates lowers to it, so there is no figure — and a 0 here would "+
				"tell every driver the planner predicted no rows, which it never did.",
				planArgEstimatedRows, v)
		}
		if v, present := args[planArgEstimatedRowsSource]; present {
			t.Errorf("the un-estimated Project published %s=%v; the provenance key must "+
				"never appear without a figure to qualify.", planArgEstimatedRowsSource, v)
		}
	}

	kids, ok := m[planKeyChildren].([]any)
	if !ok || len(kids) != 1 {
		t.Fatalf("%s = %#v, want one child map", planKeyChildren, m[planKeyChildren])
	}
	child, ok := kids[0].(map[string]any)
	if !ok {
		t.Fatalf("child metadata is %T, want a map", kids[0])
	}
	args := planMetaArgs(t, child)
	if args == nil {
		t.Fatalf("the estimated scan published no %s map at all: %#v", planKeyArgs, child)
	}
	// The value type matters as much as the key. Every integer the driver's hydrator
	// decodes arrives as an int64 (neo4j-go-driver v5.28.4,
	// neo4j/internal/bolt/hydrator.go, unp.Int), so an int here would be delivered
	// as something a caller asserting int64 could not read.
	got, present := args[planArgEstimatedRows]
	if !present {
		t.Fatalf("the estimated scan published no %s key: %#v", planArgEstimatedRows, args)
	}
	if n, isInt64 := got.(int64); !isInt64 || n != 2000 {
		t.Errorf("%s = %#v (%T), want int64(2000)", planArgEstimatedRows, got, got)
	}
	if src, _ := args[planArgEstimatedRowsSource].(string); src != "exact" {
		t.Errorf("%s = %#v, want \"exact\". A bare number gives a driver no way to tell a "+
			"maintained exact count from a heuristic guess, and marking that difference is "+
			"the one place GoGraph's plan output leads all three reference implementations.",
			planArgEstimatedRowsSource, args[planArgEstimatedRowsSource])
	}
}

// TestPlanNodeMetadata_EstimateIsPublishedForAnExplainAsWellAsAProfile holds the
// property that makes the estimate worth publishing at all.
//
// Every other figure on this message is gated on `profiled`, because an EXPLAIN ran
// nothing and a measurement it did not take must not appear. The estimate is not a
// measurement: the planner derived it BEFORE anything ran, so an EXPLAIN has it and
// a PROFILE has the same one. Gating it would leave `ResultSummary.Plan()` exactly
// as empty of numbers as it was before this task.
func TestPlanNodeMetadata_EstimateIsPublishedForAnExplainAsWellAsAProfile(t *testing.T) {
	t.Parallel()

	node := exec.PlanNode{
		Name: "Filter",
		Est:  exec.PlanEstimate{Rows: 17, Source: exec.EstimateStats},
	}

	for _, arm := range []struct {
		name     string
		profiled bool
	}{{"explain", false}, {"profile", true}} {
		t.Run(arm.name, func(t *testing.T) {
			args := planMetaArgs(t, planNodeMetadata(&node, arm.profiled))
			if args == nil {
				t.Fatalf("%s published no %s map", arm.name, planKeyArgs)
			}
			if n, _ := args[planArgEstimatedRows].(int64); n != 17 {
				t.Errorf("%s published %s=%#v, want int64(17)", arm.name,
					planArgEstimatedRows, args[planArgEstimatedRows])
			}
			if src, _ := args[planArgEstimatedRowsSource].(string); src != "stats" {
				t.Errorf("%s published %s=%#v, want \"stats\"", arm.name,
					planArgEstimatedRowsSource, args[planArgEstimatedRowsSource])
			}
		})
	}

	// And the measured keys stay gated: an EXPLAIN publishes the estimate WITHOUT
	// publishing rows, or the ungated estimate would have smuggled the measured
	// group in with it.
	explained := planNodeMetadata(&node, false)
	if _, present := explained[planKeyRows]; present {
		t.Errorf("an EXPLAIN published %s; it executed nothing and has no row count to "+
			"report", planKeyRows)
	}
}
