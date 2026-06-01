package explain

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// estimator is an optional interface that logical plan nodes may implement to
// expose their estimated row count to the EXPLAIN renderer. When a node does
// not implement estimator its estimated rows are rendered as "-".
type estimator interface {
	EstimatedRows() int64
}

// TextTree renders a physical plan in Neo4j-style columnar text:
//
//	+-------------------------+----------+----------+
//	| Operator                | Est.Rows | Vars     |
//	+-------------------------+----------+----------+
//	| ProduceResults          |      100 | n        |
//	| └─ NodeByLabelScan      |      100 | n:Person |
//	+-------------------------+----------+----------+
//
// The Operator column is padded to the widest (name + tree-indent). Est.Rows
// comes from the [estimator] interface; nodes that do not implement it show "-".
// Vars lists the variables returned by [ir.LogicalPlan.Vars].
//
// Output is stable across runs: no map iteration order is relied upon.
// Children appear in [ir.LogicalPlan.Children] order.
func TextTree(plan ir.LogicalPlan) string {
	// Collect rows in DFS order.
	type row struct {
		label   string // indented operator name
		estRows string // estimated rows or "-"
		vars    string // comma-joined vars
	}

	var rows []row
	var collect func(p ir.LogicalPlan, prefix string, isRoot, isLast bool)
	collect = func(p ir.LogicalPlan, prefix string, isRoot, isLast bool) {
		var connector, childCont string
		if isRoot {
			connector = ""
			childCont = ""
		} else if isLast {
			connector = "└─ " // └─
			childCont = "   "
		} else {
			connector = "├─ " // ├─
			childCont = "│  " // │
		}

		label := prefix + connector + operatorLabel(p)

		var estStr string
		if est, ok := p.(estimator); ok {
			n := est.EstimatedRows()
			if n == 0 {
				estStr = "-"
			} else {
				estStr = fmt.Sprintf("%d", n)
			}
		} else {
			estStr = "-"
		}

		rows = append(rows, row{
			label:   label,
			estRows: estStr,
			vars:    strings.Join(p.Vars(), ", "),
		})

		children := p.Children()
		nextPrefix := prefix + childCont
		for i, child := range children {
			collect(child, nextPrefix, false, i == len(children)-1)
		}
	}
	collect(plan, "", true, true)

	// Compute column widths.
	const (
		hdrOp   = "Operator"
		hdrRows = "Est.Rows"
		hdrVars = "Vars"
	)
	wOp := len(hdrOp)
	wRows := len(hdrRows)
	wVars := len(hdrVars)
	for _, r := range rows {
		if n := len(r.label); n > wOp {
			wOp = n
		}
		if n := len(r.estRows); n > wRows {
			wRows = n
		}
		if n := len(r.vars); n > wVars {
			wVars = n
		}
	}

	// Build output.
	var b strings.Builder

	sep := fmt.Sprintf("+-%s-+-%s-+-%s-+",
		strings.Repeat("-", wOp),
		strings.Repeat("-", wRows),
		strings.Repeat("-", wVars),
	)

	writeLine := func(op, rows, vars string) {
		b.WriteString("| ")
		b.WriteString(padRight(op, wOp))
		b.WriteString(" | ")
		b.WriteString(padLeft(rows, wRows))
		b.WriteString(" | ")
		b.WriteString(padRight(vars, wVars))
		b.WriteString(" |\n")
	}

	b.WriteString(sep)
	b.WriteByte('\n')
	writeLine(hdrOp, hdrRows, hdrVars)
	b.WriteString(sep)
	b.WriteByte('\n')
	for _, r := range rows {
		writeLine(r.label, r.estRows, r.vars)
	}
	b.WriteString(sep)
	b.WriteByte('\n')

	return b.String()
}

// operatorLabel returns the display name for a logical plan node using the
// concrete type name. This avoids importing ir's internal operatorName function
// and works correctly for any node type (including future additions).
func operatorLabel(p ir.LogicalPlan) string {
	t := reflect.TypeOf(p)
	if t.Kind() == reflect.Pointer {
		return t.Elem().Name()
	}
	return t.Name()
}

// padRight returns s padded with trailing spaces to exactly width runes.
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// padLeft returns s padded with leading spaces to exactly width runes.
func padLeft(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}
