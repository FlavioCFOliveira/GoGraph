package explain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gograph/cypher/exec"
)

// ─────────────────────────────────────────────────────────────────────────────
// OperatorStats
// ─────────────────────────────────────────────────────────────────────────────

// OperatorStats accumulates execution statistics for one operator.
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

	wName := len(hdrName)
	wRows := len(hdrRows)
	wDbHits := len(hdrDbHits)
	wElapsed := len(hdrElapsed)
	for _, rr := range rows {
		if n := len(rr.name); n > wName {
			wName = n
		}
		if n := len(rr.rows); n > wRows {
			wRows = n
		}
		if n := len(rr.dbhits); n > wDbHits {
			wDbHits = n
		}
		if n := len(rr.elapsed); n > wElapsed {
			wElapsed = n
		}
	}
	// Also account for the total row.
	if n := len(totalRow.name); n > wName {
		wName = n
	}
	if n := len(totalRow.rows); n > wRows {
		wRows = n
	}
	if n := len(totalRow.dbhits); n > wDbHits {
		wDbHits = n
	}
	if n := len(totalRow.elapsed); n > wElapsed {
		wElapsed = n
	}

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
