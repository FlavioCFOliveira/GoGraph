package explain

// table_width_test.go — the column-width contract of the two table renderers
// ([FormatPlanTable] and [FormatReport]) and the operator naming of [TextTree]
// (rmp #2701).
//
// Every test here FAILS on the behaviour that shipped before #2701:
//
//   - the renderers measured column width with len(s) — BYTES — while their doc
//     comments promised runes, so any row carrying the multi-byte tree
//     connectors (└─, ├─, │) was padded short and the right-hand border walked
//     left as the tree got deeper. The godoc example's own three-line table was
//     already broken: 47, 43, 43 display columns.
//   - TextTree named operators by their Go type via reflection, so a plain
//     *ir.Apply rendered "Apply" where every other plan surface in the module
//     calls it "CartesianProduct".
//   - a node that DID implement the estimator interface and estimated zero rows
//     rendered "-", which is the marker for "no estimate at all".

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// assertRectangular fails when the rendered table's lines are not all the same
// DISPLAY width. Width is counted in runes, which is the display width of every
// character these renderers emit (ASCII plus the single-width box-drawing set).
//
// Counting bytes here would defeat the test: the defect is precisely that bytes
// and display columns diverge, and a byte-counted table IS rectangular in bytes
// while being visibly ragged on screen.
func assertRectangular(t *testing.T, what, out string) {
	t.Helper()
	if out == "" {
		t.Fatalf("%s: rendered nothing", what)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := utf8.RuneCountInString(lines[0])
	for i, l := range lines {
		if got := utf8.RuneCountInString(l); got != want {
			t.Errorf("%s: line %d is %d display columns, want %d (bytes=%d)\n%s",
				what, i, got, want, len(l), out)
		}
	}
}

// TestFormatPlanTable_RuneWidthAlignment pins the plan table's alignment across
// a tree deep enough to use every connector.
func TestFormatPlanTable_RuneWidthAlignment(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewSelection(
			"n.age > 18",
			ir.NewUnion(
				// The first branch must have a child of its own, or the "│  "
				// continuation is never emitted and the deepest multi-byte
				// prefix goes untested.
				ir.NewSelection("n.name IS NOT NULL", ir.NewNodeByLabelScan("n", "Person")),
				ir.NewNodeByLabelScan("m", "Animal"),
			),
		),
	)
	out := TextTree(plan)
	assertRectangular(t, "TextTree", out)

	// The connectors must actually be present, or the test would pass on a
	// renderer that emitted no multi-byte characters at all and proved nothing.
	for _, connector := range []string{"└─ ", "├─ ", "│  "} {
		if !strings.Contains(out, connector) {
			t.Fatalf("output contains no %q, so it does not exercise multi-byte padding:\n%s", connector, out)
		}
	}
	// At least one line must be wider in bytes than in runes — the condition
	// under which byte padding and rune padding differ at all.
	var sawMultiByte bool
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(l) > utf8.RuneCountInString(l) {
			sawMultiByte = true
			break
		}
	}
	if !sawMultiByte {
		t.Fatal("no line carries a multi-byte rune; the alignment assertion is vacuous")
	}
}

// TestFormatPlanTable_WidestCellSetsTheColumn checks the width rule itself: a
// column is as wide as its widest cell, measured in runes, and never narrower
// than its header.
func TestFormatPlanTable_WidestCellSetsTheColumn(t *testing.T) {
	out := FormatPlanTable([]PlanRow{
		{Operator: "└─ " + strings.Repeat("X", 30), EstRows: "1234567890", Vars: "a, b, c"},
		{Operator: "A", EstRows: "1", Vars: "a"},
	})
	assertRectangular(t, "FormatPlanTable", out)

	// Operator = 3 runes of connector + 30 X = 33; Est.Rows = 10 (the cell is
	// wider than its 8-rune header); Vars = 7 ("a, b, c", wider than "Vars").
	// The separator carries one dash per column rune plus the two padding
	// spaces each column is surrounded by.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	const wantDashes = (33 + 2) + (10 + 2) + (7 + 2)
	if got := strings.Count(lines[0], "-"); got != wantDashes {
		t.Errorf("separator has %d dashes, want %d:\n%s", got, wantDashes, out)
	}
}

// TestFormatReport_RuneWidthAlignment pins the same contract on the PROFILE
// table, whose Operator column carries tree connectors once a caller renders a
// plan tree into it (which cypher.Engine.ProfileTable does).
func TestFormatReport_RuneWidthAlignment(t *testing.T) {
	out := FormatReport(ProfileReport{
		Operators: []OperatorStats{
			{Name: "Project", Rows: 40},
			{Name: "└─ Filter", Rows: 40},
			{Name: "   └─ NodeByLabelScan [Person]", Rows: 40, DbHits: 40, ElapsedNs: 3000},
		},
		TotalRows:   120,
		TotalDbHits: 40,
		ElapsedMs:   0.087,
	})
	assertRectangular(t, "FormatReport", out)
}

// TestFormatPlanTable_EmptyRows renders the header and nothing else rather than
// failing or producing a degenerate box.
func TestFormatPlanTable_EmptyRows(t *testing.T) {
	out := FormatPlanTable(nil)
	assertRectangular(t, "FormatPlanTable(nil)", out)
	if !strings.Contains(out, "Operator") || !strings.Contains(out, "Est.Rows") || !strings.Contains(out, "Vars") {
		t.Errorf("header row missing:\n%s", out)
	}
	if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")); n != 4 {
		t.Errorf("empty table has %d lines, want 4 (sep, header, sep, sep):\n%s", n, out)
	}
}

// TestTextTree_OperatorNamesMatchIR proves the table names operators exactly as
// the rest of the module does. Before #2701 this test failed on both rows: a
// plain Apply rendered as "Apply" and an Expand with a bound destination as
// "Expand", because the label came from reflect.TypeOf rather than from
// [ir.OperatorName].
func TestTextTree_OperatorNamesMatchIR(t *testing.T) {
	t.Run("Apply renders as CartesianProduct", func(t *testing.T) {
		plan := ir.NewApply(
			ir.NewNodeByLabelScan("a", "Person"),
			ir.NewNodeByLabelScan("b", "City"),
		)
		out := TextTree(plan)
		if !strings.Contains(out, "CartesianProduct") {
			t.Errorf("output does not name the Apply CartesianProduct:\n%s", out)
		}
		// "Apply" must not appear as a standalone operator name. It is a
		// substring of nothing else here, so a bare occurrence is the defect.
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "| Apply") {
				t.Errorf("operator still named by its Go type:\n%s", out)
			}
		}
	})

	t.Run("bound-destination Expand renders as ExpandInto", func(t *testing.T) {
		exp := ir.NewExpand("a", "r", []string{"KNOWS"}, ir.DirectionOutgoing, "b",
			ir.NewNodeByLabelScan("a", "Person"))
		exp.IntoVar = "b"
		out := TextTree(exp)
		if !strings.Contains(out, "ExpandInto") {
			t.Errorf("a bound-destination Expand is not named ExpandInto:\n%s", out)
		}
	})
}

// zeroEstimatePlan is a plan node that implements the estimator interface and
// estimates ZERO rows — the case the renderer used to collapse into "-", the
// marker reserved for "this node offers no estimate at all".
type zeroEstimatePlan struct{}

func (zeroEstimatePlan) Children() []ir.LogicalPlan { return nil }
func (zeroEstimatePlan) Vars() []string             { return []string{"n"} }
func (zeroEstimatePlan) EstimatedRows() int64       { return 0 }

// nonZeroEstimatePlan is the control: a node estimating a non-zero count, whose
// rendering was already correct.
type nonZeroEstimatePlan struct{}

func (nonZeroEstimatePlan) Children() []ir.LogicalPlan { return nil }
func (nonZeroEstimatePlan) Vars() []string             { return []string{"n"} }
func (nonZeroEstimatePlan) EstimatedRows() int64       { return 7 }

// TestTextTree_ZeroEstimateIsNotUnknown distinguishes "estimated zero rows" from
// "no estimate". They are different facts, and an operator the planner expects
// to produce nothing is usually the one a reader is looking for.
func TestTextTree_ZeroEstimateIsNotUnknown(t *testing.T) {
	zero := TextTree(zeroEstimatePlan{})
	if !strings.Contains(zero, "| 0 |") && !strings.Contains(zero, "        0 |") {
		t.Errorf("an estimate of zero rows is not rendered as 0:\n%s", zero)
	}
	if strings.Contains(zero, "- |") {
		t.Errorf("an estimate of zero rows is still rendered as the unknown marker:\n%s", zero)
	}

	seven := TextTree(nonZeroEstimatePlan{})
	if !strings.Contains(seven, "7") {
		t.Errorf("control: a non-zero estimate is missing:\n%s", seven)
	}

	// A node that implements nothing keeps the unknown marker.
	unknown := TextTree(ir.NewAllNodesScan("n"))
	if !strings.Contains(unknown, "- |") {
		t.Errorf("a node with no estimator must render the unknown marker:\n%s", unknown)
	}
}

// TestTextTree_UnknownNodeTypeKeepsAnIdentifyingName covers the fallback: an
// out-of-tree implementation of ir.LogicalPlan is not enumerated by
// [ir.OperatorName], which answers "Unknown" for it. Rendering that would throw
// away the only identifying information the node has, so the renderer falls back
// to the concrete type name.
func TestTextTree_UnknownNodeTypeKeepsAnIdentifyingName(t *testing.T) {
	out := TextTree(zeroEstimatePlan{})
	if !strings.Contains(out, "zeroEstimatePlan") {
		t.Errorf("an unrecognised node type lost its identifying name:\n%s", out)
	}
	if strings.Contains(out, "| Unknown") {
		t.Errorf("an unrecognised node type rendered as \"Unknown\":\n%s", out)
	}
}
