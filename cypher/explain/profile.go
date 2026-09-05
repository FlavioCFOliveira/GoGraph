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
	DbHits uint64
	// ElapsedNs is the total nanoseconds spent inside Next across all calls.
	ElapsedNs int64
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
	// TotalDbHits is the sum of all operator dbHits.
	TotalDbHits uint64
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
func FormatReport(r ProfileReport) string {
	type row struct {
		name    string
		rows    string
		dbhits  string
		elapsed string
	}

	const (
		hdrName    = "Operator"
		hdrRows    = "Rows"
		hdrDbHits  = "DbHits"
		hdrElapsed = "Time (ms)"
	)

	rows := make([]row, len(r.Operators))
	for i, op := range r.Operators {
		rows[i] = row{
			name:    op.Name,
			rows:    fmt.Sprintf("%d", op.Rows),
			dbhits:  fmt.Sprintf("%d", op.DbHits),
			elapsed: fmt.Sprintf("%.3f", float64(op.ElapsedNs)/1e6),
		}
	}
	totalRow := row{
		name:    "Total",
		rows:    fmt.Sprintf("%d", r.TotalRows),
		dbhits:  fmt.Sprintf("%d", r.TotalDbHits),
		elapsed: fmt.Sprintf("%.3f", r.ElapsedMs),
	}

	// Widths are measured in RUNES, not bytes: an Operator cell may carry the
	// multi-byte tree connectors (└─, ├─, │) when the caller renders a plan tree
	// into the column, and a byte measurement pads those rows short so the
	// right-hand border walks left with the tree depth.
	wName := maxWidth(0, hdrName)
	wRows := maxWidth(0, hdrRows)
	wDbHits := maxWidth(0, hdrDbHits)
	wElapsed := maxWidth(0, hdrElapsed)
	for _, rr := range rows {
		wName = maxWidth(wName, rr.name)
		wRows = maxWidth(wRows, rr.rows)
		wDbHits = maxWidth(wDbHits, rr.dbhits)
		wElapsed = maxWidth(wElapsed, rr.elapsed)
	}
	// Also account for the total row.
	wName = maxWidth(wName, totalRow.name)
	wRows = maxWidth(wRows, totalRow.rows)
	wDbHits = maxWidth(wDbHits, totalRow.dbhits)
	wElapsed = maxWidth(wElapsed, totalRow.elapsed)

	sep := fmt.Sprintf("+-%s-+-%s-+-%s-+-%s-+",
		strings.Repeat("-", wName),
		strings.Repeat("-", wRows),
		strings.Repeat("-", wDbHits),
		strings.Repeat("-", wElapsed),
	)

	var b strings.Builder

	writeLine := func(name, rowsStr, dbhitsStr, elapsedStr string) {
		b.WriteString("| ")
		b.WriteString(padRight(name, wName))
		b.WriteString(" | ")
		b.WriteString(padLeft(rowsStr, wRows))
		b.WriteString(" | ")
		b.WriteString(padLeft(dbhitsStr, wDbHits))
		b.WriteString(" | ")
		b.WriteString(padLeft(elapsedStr, wElapsed))
		b.WriteString(" |\n")
	}

	b.WriteString(sep)
	b.WriteByte('\n')
	writeLine(hdrName, hdrRows, hdrDbHits, hdrElapsed)
	b.WriteString(sep)
	b.WriteByte('\n')
	for _, rr := range rows {
		writeLine(rr.name, rr.rows, rr.dbhits, rr.elapsed)
	}
	b.WriteString(sep)
	b.WriteByte('\n')
	writeLine(totalRow.name, totalRow.rows, totalRow.dbhits, totalRow.elapsed)
	b.WriteString(sep)
	b.WriteByte('\n')

	return b.String()
}
