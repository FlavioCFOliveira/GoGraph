package server

// plan_meta_rows_removed_test.go — the `RowsRemovedByFilter` argument on the Bolt
// wire (rmp #2764).
//
// The figure is published as an ARG rather than as a top-level key, which is a
// wire-protocol fact and not a preference: the driver's parseProfile reads a fixed
// set of top-level keys — dbHits, rows, time and the three pageCache ones — and
// discards every other, while `args` is read wholesale into
// ProfiledPlan.Arguments (neo4j-go-driver v5.28.4, neo4j/internal/bolt/hydrator.go,
// parsePlanOpIdArgsChildren). A new top-level key would therefore reach no caller
// at all.
//
// This file is the map-level complement of the e2e gates in
// e2e_plan_prefix_test.go, and it exists for what the driver cannot show: whether
// the key is ABSENT or present-and-zero. Both arrive at a Go caller as a missing
// map entry versus an int64(0), and only a test that reads the map this server
// builds can tell them apart.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// planArgsOf returns the `args` map of one rendered plan node, and whether it has
// one at all.
func planArgsOf(t *testing.T, m map[string]packstream.Value) (map[string]packstream.Value, bool) {
	t.Helper()
	raw, present := m[planKeyArgs]
	if !present {
		return nil, false
	}
	args, ok := raw.(map[string]packstream.Value)
	if !ok {
		t.Fatalf("%s is %T, want map[string]packstream.Value — the driver reads it as "+
			"a MAP (hydrator.go, parsePlanOpIdArgsChildren)", planKeyArgs, raw)
	}
	return args, true
}

// TestPlanNodeMetadata_PublishesRowsRemovedOnlyWhereItIsAFigure is the acceptance
// gate. An operator that removes rows must publish the count; one that does not
// must publish NO key for it.
//
// Both directions are asserted, because a server that never sent the key would
// satisfy the second alone and would publish nothing at all, and a server that
// always sent it would satisfy the first alone and would assert a zero for every
// scan in every plan.
func TestPlanNodeMetadata_PublishesRowsRemovedOnlyWhereItIsAFigure(t *testing.T) {
	t.Parallel()

	tree := exec.PlanNode{
		Name:     "Filter",
		Profiled: true, Rows: 3, DbHits: 0, DbHitsKnown: false,
		RowsRemovedByFilter: 997, RowsRemovedByFilterKnown: true,
		Children: []exec.PlanNode{{
			Name: "NodeByLabelScan", Detail: "P",
			Profiled: true, Rows: 1000, DbHits: 1000, DbHitsKnown: true,
		}},
	}

	m := planNodeMetadata(&tree, true)

	args, hasArgs := planArgsOf(t, m)
	if !hasArgs {
		t.Fatalf("the Filter published no %s map at all, so the figure reaches no "+
			"driver", planKeyArgs)
	}
	got, present := args[planArgRowsRemoved]
	if !present {
		t.Fatalf("the Filter published no %s argument. It removed 997 rows and that "+
			"is the figure rmp #2764 exists to publish; args carries %v",
			planArgRowsRemoved, args)
	}
	if v, ok := got.(int64); !ok || v != 997 {
		t.Errorf("%s = %v (%T), want int64(997). The driver surfaces args verbatim, "+
			"so the value has to be the count and its Go type has to survive packing",
			planArgRowsRemoved, got, got)
	}

	// The child removes no rows, so it must publish nothing for it — not a zero.
	if len(m[planKeyChildren].([]packstream.Value)) != 1 {
		t.Fatalf("the Filter published %d children, want 1", len(m[planKeyChildren].([]packstream.Value)))
	}
	child, ok := m[planKeyChildren].([]packstream.Value)[0].(map[string]packstream.Value)
	if !ok {
		t.Fatalf("the child node is %T, want a map", m[planKeyChildren].([]packstream.Value)[0])
	}
	childArgs, _ := planArgsOf(t, child)
	if v, present := childArgs[planArgRowsRemoved]; present {
		t.Errorf("the NodeByLabelScan published %s=%v. A scan removes no rows, so it "+
			"has no figure and a 0 on this key would be a measurement claim — the same "+
			"reasoning that omits dbHits for an uncounted operator and omits the "+
			"page-cache keys entirely.", planArgRowsRemoved, v)
	}
	// … while still publishing the Details arg it does have, so the guard above is
	// about this key and not about args as a whole.
	if _, present := childArgs[planArgDetails]; !present {
		t.Errorf("the NodeByLabelScan published no %s argument, so the previous "+
			"assertion could have passed on a node with no args map at all",
			planArgDetails)
	}
}

// TestPlanNodeMetadata_ExplainPublishesNoRowsRemoved keeps the figure out of an
// EXPLAIN, which executed nothing and therefore measured nothing.
//
// The production guard is `profiled && n.RowsRemovedByFilterKnown`, and the flag
// alone would in practice be enough — an EXPLAIN never runs an operator, so no node
// it captures ever claims the figure. The mode is checked as well so the two cannot
// disagree, and this gate is what holds that: it hands the EXPLAIN path a node
// whose flag IS set, which only a mode check can reject.
func TestPlanNodeMetadata_ExplainPublishesNoRowsRemoved(t *testing.T) {
	t.Parallel()

	tree := exec.PlanNode{
		Name:                "Filter",
		RowsRemovedByFilter: 997, RowsRemovedByFilterKnown: true,
	}
	m := planNodeMetadata(&tree, false)

	args, hasArgs := planArgsOf(t, m)
	if hasArgs {
		if v, present := args[planArgRowsRemoved]; present {
			t.Errorf("an EXPLAIN published %s=%v. EXPLAIN executes nothing, so every "+
				"measured field is omitted rather than zeroed — a number here would "+
				"describe a run that never happened.", planArgRowsRemoved, v)
		}
	}
}
