package explain

import (
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// estimator is an optional interface that logical plan nodes may implement to
// expose their estimated row count to the EXPLAIN renderer. When a node does
// not implement estimator its estimated rows are rendered as "-"; when it does,
// the value is rendered as written, INCLUDING zero — an operator estimated to
// produce no rows is a real and useful estimate, not a missing one.
//
// No node in [github.com/FlavioCFOliveira/GoGraph/cypher/ir] implements it: the
// engine's cardinality estimates are derived from the live count store and
// statistics collector rather than stored on the plan, and reach the table
// through the EstRows cell of a [PlanRow] filled by the cypher package's
// Engine.ExplainTable. The interface is therefore an extension point for a caller
// that implements [ir.LogicalPlan] itself — the interface has only exported
// methods, so an out-of-tree plan node is possible — and [TextTree] on an
// in-tree plan renders "-" in every Est.Rows cell.
type estimator interface {
	EstimatedRows() int64
}

// PlanRow is one operator line of a rendered plan table.
//
// Operator already carries the tree indentation and connector for the node's
// depth, so a row is rendered verbatim into the Operator column; EstRows and
// Vars are the remaining two cells. A caller building rows by hand is free to
// leave EstRows empty, which renders as an empty cell rather than "-": "-" is
// the renderer's marker for "this node offers no estimate", and inventing it
// for a caller who simply did not fill the field would be a fabrication.
type PlanRow struct {
	// Operator is the operator's display name, prefixed with its tree indent.
	Operator string
	// EstRows is the estimated row count cell, right-aligned when rendered.
	EstRows string
	// Vars is the comma-joined list of variables the operator exposes.
	Vars string
}

// Column headers of the plan table. They are the minimum width of their column.
const (
	hdrOperator = "Operator"
	hdrEstRows  = "Est.Rows"
	hdrVars     = "Vars"
)

// FormatPlanTable renders rows as a Neo4j-style columnar plan table:
//
//	+-----------------------+----------+----------+
//	| Operator              | Est.Rows | Vars     |
//	+-----------------------+----------+----------+
//	| ProduceResults        |      100 | n        |
//	| └─ NodeByLabelScan    |      100 | n:Person |
//	+-----------------------+----------+----------+
//
// Each column is as wide as its widest cell (never narrower than its header),
// measured in RUNES rather than bytes: the tree connectors (└─, ├─, │) are
// multi-byte UTF-8, so a byte-width measurement pads a deeply indented row
// short and the right-hand border walks left as the tree descends. The
// measurement is rune count, which is the correct width for every character
// this renderer emits (ASCII plus the box-drawing set, all single-width); a
// caller who puts a double-width character — CJK, an emoji — into a cell will
// still see that cell render narrow, because no terminal-width table can be
// correct for every font without measuring the terminal.
//
// The Operator and Vars columns are left-aligned, Est.Rows right-aligned.
// Output ends with a trailing newline. Rendering is a pure function of rows, so
// it is safe for concurrent use and deterministic.
func FormatPlanTable(rows []PlanRow) string {
	wOp := utf8.RuneCountInString(hdrOperator)
	wRows := utf8.RuneCountInString(hdrEstRows)
	wVars := utf8.RuneCountInString(hdrVars)
	for i := range rows {
		wOp = maxWidth(wOp, rows[i].Operator)
		wRows = maxWidth(wRows, rows[i].EstRows)
		wVars = maxWidth(wVars, rows[i].Vars)
	}

	var b strings.Builder
	sep := "+-" + strings.Repeat("-", wOp) +
		"-+-" + strings.Repeat("-", wRows) +
		"-+-" + strings.Repeat("-", wVars) + "-+\n"

	writeLine := func(op, est, vars string) {
		b.WriteString("| ")
		b.WriteString(padRight(op, wOp))
		b.WriteString(" | ")
		b.WriteString(padLeft(est, wRows))
		b.WriteString(" | ")
		b.WriteString(padRight(vars, wVars))
		b.WriteString(" |\n")
	}

	b.WriteString(sep)
	writeLine(hdrOperator, hdrEstRows, hdrVars)
	b.WriteString(sep)
	for i := range rows {
		writeLine(rows[i].Operator, rows[i].EstRows, rows[i].Vars)
	}
	b.WriteString(sep)
	return b.String()
}

// TextTree renders a logical plan in Neo4j-style columnar text:
//
//	+-------------------------+----------+----------+
//	| Operator                | Est.Rows | Vars     |
//	+-------------------------+----------+----------+
//	| ProduceResults          |        - | n        |
//	| └─ NodeByLabelScan      |        - | n        |
//	+-------------------------+----------+----------+
//
// It walks plan depth-first into [PlanRow] values and hands them to
// [FormatPlanTable]. Operator names come from [ir.OperatorName], the same
// naming the engine's own plan renderers use, so a plain Apply reads as
// CartesianProduct and an Expand with a bound destination as ExpandInto exactly
// as they do in EXPLAIN. A node type [ir.OperatorName] does not know falls back
// to its concrete Go type name rather than to "Unknown", which keeps an
// out-of-tree plan node legible.
//
// Est.Rows comes from the optional [estimator] interface. No in-tree plan node
// implements it, so every cell of a plan built by this module's planner renders
// "-"; the engine's cardinality estimates reach a plan table through
// [FormatPlanTable] instead. Vars lists the variables returned by
// [ir.LogicalPlan.Vars].
//
// Output is stable across runs: no map iteration order is relied upon.
// Children appear in [ir.LogicalPlan.Children] order.
func TextTree(plan ir.LogicalPlan) string {
	var rows []PlanRow
	var collect func(p ir.LogicalPlan, prefix string, isRoot, isLast bool)
	collect = func(p ir.LogicalPlan, prefix string, isRoot, isLast bool) {
		var connector, childCont string
		switch {
		case isRoot:
			connector, childCont = "", ""
		case isLast:
			connector, childCont = "└─ ", "   "
		default:
			connector, childCont = "├─ ", "│  "
		}

		estStr := "-"
		if est, ok := p.(estimator); ok {
			estStr = strconv.FormatInt(est.EstimatedRows(), 10)
		}

		rows = append(rows, PlanRow{
			Operator: prefix + connector + operatorLabel(p),
			EstRows:  estStr,
			Vars:     strings.Join(p.Vars(), ", "),
		})

		children := p.Children()
		nextPrefix := prefix + childCont
		for i, child := range children {
			collect(child, nextPrefix, false, i == len(children)-1)
		}
	}
	collect(plan, "", true, true)

	return FormatPlanTable(rows)
}

// operatorLabel returns the display name for a logical plan node.
//
// It asks [ir.OperatorName] first so the table names operators exactly as the
// engine's EXPLAIN does — the two used to disagree, because this renderer named
// nodes by their Go type and ir names a plain *ir.Apply "CartesianProduct" and
// an *ir.Expand with a bound destination "ExpandInto". ir returns "Unknown" for
// a type it does not enumerate, which is every out-of-tree implementation of
// [ir.LogicalPlan]; those fall back to the concrete type name, which is at
// least identifying.
func operatorLabel(p ir.LogicalPlan) string {
	if name := ir.OperatorName(p); name != "" && name != "Unknown" {
		return name
	}
	t := reflect.TypeOf(p)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Name() == "" {
		return "Unknown"
	}
	return t.Name()
}

// maxWidth returns the larger of w and the rune width of s.
func maxWidth(w int, s string) int {
	if n := utf8.RuneCountInString(s); n > w {
		return n
	}
	return w
}

// padRight returns s padded with trailing spaces to exactly width runes.
// A string already at or beyond width is returned unchanged.
func padRight(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// padLeft returns s padded with leading spaces to exactly width runes.
// A string already at or beyond width is returned unchanged.
func padLeft(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return strings.Repeat(" ", width-n) + s
}
