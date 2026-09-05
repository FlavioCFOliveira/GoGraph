package explain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// ─────────────────────────────────────────────────────────────────────────────
// OperatorStats
// ─────────────────────────────────────────────────────────────────────────────

// OperatorStats accumulates execution statistics for one operator.
//
// The instance a [ProfiledOperator] embeds is mutated in place by every
// [ProfiledOperator.Next] call, without synchronisation, so it is NOT safe for
// concurrent use while its pipeline is draining: reading it from another
// goroutine — including through [ProfiledOperator.Stats] — races the
// accumulation. [ProfiledOperator.Stats] returns a copy, so once the pipeline
// has been drained that snapshot is an ordinary value and may be shared and
// read freely.
type OperatorStats struct {
	// Name is the display name assigned when the operator was wrapped.
	Name string
	// Rows is the number of rows produced by successful Next calls.
	Rows uint64
	// DbHits is the number of logical storage accesses (see [DbHitsCounter]).
	// It is meaningful only when DbHitsKnown is true.
	DbHits uint64
	// ElapsedNs is the total nanoseconds spent inside Next across all calls.
	ElapsedNs int64
	// DbHitsKnown reports whether DbHits is a figure at all, mirroring
	// [exec.PlanNode.DbHitsKnown] on the node this row was flattened from.
	//
	// When it is false [FormatReport] prints [exec.DbHitsUnknown] in the cell
	// instead of a number, because an operator whose accesses nobody counted and
	// an operator that genuinely read nothing must not print the same 0
	// (rmp #2760).
	//
	// The zero value is therefore "unknown", which is deliberate: a report
	// assembled without considering the question should not silently assert that
	// every operator's storage cost was measured. A caller building a report by
	// hand from figures it does count sets this true.
	DbHitsKnown bool
	// RowsRemovedByFilter is the candidate rows this operator read and DISCARDED —
	// PostgreSQL's `Rows Removed by Filter`, mirroring
	// [exec.PlanNode.RowsRemovedByFilter] on the node this row was flattened from.
	// It is meaningful only when RowsRemovedByFilterKnown is true.
	RowsRemovedByFilter uint64
	// RowsRemovedByFilterKnown reports whether RowsRemovedByFilter is a figure at
	// all: true only for an operator that removes rows.
	//
	// It governs the report at TWO levels, which is what keeps the column honest
	// AND keeps it out of the way of every plan that has no filter in it:
	//
	//   - per CELL — a false leaves the cell BLANK rather than printing 0, because
	//     "this operator removes no rows" and "this filter removed none" are
	//     different facts and only the second is a measurement (rmp #2764); and
	//   - per COLUMN — [FormatReport] omits the whole Removed column when no
	//     operator in the report reports the figure, so a plan with no filter and
	//     no expansion renders exactly the four columns it always did. That is
	//     Neo4j's rule, which drops a column no plan node carries an argument for
	//     (renderAsTreeTable.scala, 5.26.16), rather than PostgreSQL's, which has no
	//     columns to drop.
	//
	// The zero value is "no figure", which is the correct default for a report
	// assembled by hand: a caller that has not thought about rejection should not
	// have the table assert that its operators rejected nothing.
	RowsRemovedByFilterKnown bool
}

// ─────────────────────────────────────────────────────────────────────────────
// ProfiledOperator
// ─────────────────────────────────────────────────────────────────────────────

// ProfiledOperator wraps an [exec.Operator] and records per-call statistics.
// It implements [exec.Operator].
//
// ProfiledOperator is NOT safe for concurrent use.
type ProfiledOperator struct {
	inner exec.Operator
	stats OperatorStats
}

// NewProfiledOperator wraps op, assigning it the display name given by name.
func NewProfiledOperator(op exec.Operator, name string) *ProfiledOperator {
	return &ProfiledOperator{
		inner: op,
		stats: OperatorStats{Name: name},
	}
}

// Init implements [exec.Operator]. It delegates to the inner operator.
func (p *ProfiledOperator) Init(ctx context.Context) error {
	return p.inner.Init(ctx)
}

// Next implements [exec.Operator]. It delegates to the inner operator,
// incrementing Rows on each (true, nil) return and accumulating elapsed time.
func (p *ProfiledOperator) Next(out *exec.Row) (bool, error) {
	start := time.Now()
	ok, err := p.inner.Next(out)
	elapsed := time.Since(start).Nanoseconds()
	p.stats.ElapsedNs += elapsed
	if ok && err == nil {
		p.stats.Rows++
	}
	return ok, err
}

// Close implements [exec.Operator]. It delegates to the inner operator.
func (p *ProfiledOperator) Close() error {
	return p.inner.Close()
}

// Stats returns the accumulated statistics for this operator.
func (p *ProfiledOperator) Stats() OperatorStats {
	return p.stats
}

// ─────────────────────────────────────────────────────────────────────────────
// ProfileReport
// ─────────────────────────────────────────────────────────────────────────────

// ProfileReport is the textual PROFILE output collected after draining a
// pipeline instrumented with [ProfiledOperator] wrappers.
//
// A report is assembled once, after the drain, and nothing here mutates it
// afterwards — [FormatReport] takes it by value and only reads it — so a
// finished report is safe for concurrent reads by any number of goroutines. The
// one caveat is Operators: every copy of the report shares that single backing
// array, so a caller must not append to it or overwrite its elements while
// another goroutine reads the report.
type ProfileReport struct {
	// Operators holds per-operator statistics in the order they were added.
	Operators []OperatorStats
	// TotalRows is the sum of all operator row counts.
	TotalRows uint64
	// TotalDbHits is the sum of the operator dbHits that were KNOWN. Operators
	// whose accesses nobody counted contribute nothing to it and set
	// TotalDbHitsUncertain instead.
	TotalDbHits uint64
	// TotalDbHitsUncertain reports whether the sum in TotalDbHits omitted at least
	// one operator, so the query's real cost is TotalDbHits plus an unknown
	// amount. [FormatReport] then renders the Total cell as "x + ?" rather than as
	// a plain number that would read as complete.
	//
	// The name and the four rendered cases are Neo4j's, transcribed from
	// InternalPlanDescription.TotalHits and renderSummary.scala (5.26.16), where
	// an operator carrying no DbHits argument contributes TotalHits(0,
	// uncertain = true) and the flag is OR-ed across the plan.
	//
	// Unlike [OperatorStats.DbHitsKnown] this field's zero value means CERTAIN,
	// because it describes a sum rather than a cell: a report that summed nothing
	// unknown has omitted nothing, and a hand-built report of known figures needs
	// no extra field set to render its total honestly.
	TotalDbHitsUncertain bool
	// ElapsedMs is the total wall-clock time in milliseconds.
	ElapsedMs float64
}

// FormatReport formats r as a Neo4j-style table:
//
//	+--------------------------+--------+---------+-----------+
//	| Operator                 |   Rows | DbHits  | Time (ms) |
//	+--------------------------+--------+---------+-----------+
//	| NodeByLabelScan          |    100 |     100 |     0.012 |
//	| ProduceResults           |    100 |       0 |     0.001 |
//	+--------------------------+--------+---------+-----------+
//	| Total                    |    200 |     100 |     0.013 |
//	+--------------------------+--------+---------+-----------+
//
// A DbHits cell whose figure was never counted
// ([OperatorStats.DbHitsKnown] false) renders as [exec.DbHitsUnknown] — "?" —
// and the Total then renders as "x + ?", so a reader can see that the sum is a
// floor and not the whole cost (rmp #2760):
//
//	+--------------------------+--------+---------+-----------+
//	| NodeByLabelScan          |    100 |     100 |     0.012 |
//	| └─ Filter                |     42 |       ? |     0.004 |
//	+--------------------------+--------+---------+-----------+
//	| Total                    |    142 | 100 + ? |     0.016 |
//	+--------------------------+--------+---------+-----------+
//
// Both forms come from [exec.DbHitsCell] and [exec.DbHitsTotalCell], which the
// indented tree renderer uses too, so the two renderings of one run cannot
// disagree about which figures exist.
//
// # The Removed column, present only when something removes rows
//
// When at least one operator reports the rows it discarded
// ([OperatorStats.RowsRemovedByFilterKnown]), a fifth column appears between
// DbHits and Time — PostgreSQL's `Rows Removed by Filter`, abbreviated to fit a
// fixed-width table (rmp #2764):
//
//	+--------------------------+------+--------+---------+-----------+
//	| Operator                 | Rows | DbHits | Removed | Time (ms) |
//	+--------------------------+------+--------+---------+-----------+
//	| Filter                   |    3 |      ? |     997 |     0.412 |
//	| └─ NodeByLabelScan [P]   | 1000 |   1000 |         |     0.203 |
//	+--------------------------+------+--------+---------+-----------+
//	| Total                    | 1003 | 1000 + ? |       |     0.412 |
//	+--------------------------+------+--------+---------+-----------+
//
// Two omissions in that table are deliberate and mean different things:
//
//   - the SCAN's cell is blank because a scan removes no rows — there is no figure,
//     as against the "?" one column left, which admits a figure exists and was not
//     counted; and
//   - the TOTAL's cell is blank because no plan-wide figure is claimed. Db-hits can
//     be totalled because every operator is classified, so the sum is either
//     complete or explicitly incomplete. Rejection is reported by three operator
//     families only, and others discard rows for reasons this figure would
//     misdescribe (SemiApply on an inner plan's emptiness, LIMIT on a count), so a
//     summed cell would be a floor presented as a total. See exec's
//     rowsRemovedCounter.
//
// When NO operator reports the figure the column is not rendered at all and the
// table is byte-identical to the four-column form above. That is Neo4j's rule for
// an argument no plan node carries (renderAsTreeTable.scala, 5.26.16).
func FormatReport(r ProfileReport) string {
	type row struct {
		name    string
		rows    string
		dbhits  string
		removed string
		elapsed string
	}

	const (
		hdrName    = "Operator"
		hdrRows    = "Rows"
		hdrDbHits  = "DbHits"
		hdrRemoved = "Removed"
		hdrElapsed = "Time (ms)"
	)

	// The column exists only if something in this plan removes rows. Decided over
	// the whole report before any cell is rendered, so the header, the separators
	// and every row agree by construction rather than by three parallel checks.
	showRemoved := false
	for _, op := range r.Operators {
		if op.RowsRemovedByFilterKnown {
			showRemoved = true
			break
		}
	}

	rows := make([]row, len(r.Operators))
	for i, op := range r.Operators {
		rows[i] = row{
			name:    op.Name,
			rows:    fmt.Sprintf("%d", op.Rows),
			dbhits:  exec.DbHitsCell(int64(op.DbHits), op.DbHitsKnown),
			removed: exec.RowsRemovedCell(int64(op.RowsRemovedByFilter), op.RowsRemovedByFilterKnown),
			elapsed: fmt.Sprintf("%.3f", float64(op.ElapsedNs)/1e6),
		}
	}
	totalRow := row{
		name:    "Total",
		rows:    fmt.Sprintf("%d", r.TotalRows),
		dbhits:  exec.DbHitsTotalCell(int64(r.TotalDbHits), r.TotalDbHitsUncertain),
		removed: "", // no plan-wide total is claimed; see the doc comment above
		elapsed: fmt.Sprintf("%.3f", r.ElapsedMs),
	}

	// Widths are measured in RUNES, not bytes: an Operator cell may carry the
	// multi-byte tree connectors (└─, ├─, │) when the caller renders a plan tree
	// into the column, and a byte measurement pads those rows short so the
	// right-hand border walks left with the tree depth.
	wName := maxWidth(0, hdrName)
	wRows := maxWidth(0, hdrRows)
	wDbHits := maxWidth(0, hdrDbHits)
	wRemoved := 0
	if showRemoved {
		wRemoved = maxWidth(0, hdrRemoved)
	}
	wElapsed := maxWidth(0, hdrElapsed)
	for _, rr := range rows {
		wName = maxWidth(wName, rr.name)
		wRows = maxWidth(wRows, rr.rows)
		wDbHits = maxWidth(wDbHits, rr.dbhits)
		if showRemoved {
			wRemoved = maxWidth(wRemoved, rr.removed)
		}
		wElapsed = maxWidth(wElapsed, rr.elapsed)
	}
	// Also account for the total row.
	wName = maxWidth(wName, totalRow.name)
	wRows = maxWidth(wRows, totalRow.rows)
	wDbHits = maxWidth(wDbHits, totalRow.dbhits)
	wElapsed = maxWidth(wElapsed, totalRow.elapsed)

	sep := fmt.Sprintf("+-%s-+-%s-+-%s-+",
		strings.Repeat("-", wName),
		strings.Repeat("-", wRows),
		strings.Repeat("-", wDbHits),
	)
	if showRemoved {
		sep += fmt.Sprintf("-%s-+", strings.Repeat("-", wRemoved))
	}
	sep += fmt.Sprintf("-%s-+", strings.Repeat("-", wElapsed))

	var b strings.Builder

	writeLine := func(rr row) {
		b.WriteString("| ")
		b.WriteString(padRight(rr.name, wName))
		b.WriteString(" | ")
		b.WriteString(padLeft(rr.rows, wRows))
		b.WriteString(" | ")
		b.WriteString(padLeft(rr.dbhits, wDbHits))
		if showRemoved {
			b.WriteString(" | ")
			b.WriteString(padLeft(rr.removed, wRemoved))
		}
		b.WriteString(" | ")
		b.WriteString(padLeft(rr.elapsed, wElapsed))
		b.WriteString(" |\n")
	}

	b.WriteString(sep)
	b.WriteByte('\n')
	writeLine(row{name: hdrName, rows: hdrRows, dbhits: hdrDbHits, removed: hdrRemoved, elapsed: hdrElapsed})
	b.WriteString(sep)
	b.WriteByte('\n')
	for _, rr := range rows {
		writeLine(rr)
	}
	b.WriteString(sep)
	b.WriteByte('\n')
	writeLine(totalRow)
	b.WriteString(sep)
	b.WriteByte('\n')

	return b.String()
}
