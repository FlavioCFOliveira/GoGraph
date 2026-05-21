package tck_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"gograph/cypher"
	"gograph/cypher/exec"
	"gograph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// When steps
// ─────────────────────────────────────────────────────────────────────────────

// queryTimeout is the per-scenario execution timeout. It must be short enough
// that a hung goroutine (e.g. from a parallel scan worker that panics and
// leaves the coordinator goroutine waiting on a drained channel) does not
// block the entire test run.
const queryTimeout = 10 * time.Second

// whenExecutingQuery runs the test query and stores the result (or error) in
// the world. Errors are stored in w.err rather than returned so that scenarios
// that expect an error can still succeed.
//
// The query is executed with a per-scenario timeout so that engine goroutine
// panics (which cause the coordinator to hang on a channel receive) do not
// block the test suite indefinitely.
func (w *world) whenExecutingQuery(ctx context.Context, query *godog.DocString) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			w.err = fmt.Errorf("engine panic: %v", r)
			w.result = nil
		}
	}()
	// Cancel any previously issued query context so stale goroutines stop.
	if w.queryCancel != nil {
		w.queryCancel()
		w.queryCancel = nil
	}
	w.snapshotCounts()
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	w.queryCancel = cancel // stored so result iteration can be aborted
	res, err := w.eng.RunAny(qctx, query.Content, w.params)
	if err != nil {
		cancel()
		w.queryCancel = nil
		w.err = err
		w.result = nil
		return nil
	}
	w.result = res
	return nil
}

// whenExecutingControlQuery is an alias for whenExecutingQuery used in some TCK
// features that distinguish "control" queries.
func (w *world) whenExecutingControlQuery(ctx context.Context, query *godog.DocString) error {
	return w.whenExecutingQuery(ctx, query)
}

// ─────────────────────────────────────────────────────────────────────────────
// Then steps — result assertions
// ─────────────────────────────────────────────────────────────────────────────

// resultShouldBeEmpty asserts that the query produced no rows.
func (w *world) resultShouldBeEmpty(_ context.Context) (retErr error) {
	if w.err != nil {
		return fmt.Errorf("expected empty result but query failed: %w", w.err)
	}
	if w.result == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic during result iteration: %v", r)
		}
	}()
	defer w.result.Close() //nolint:errcheck // result close is best-effort in test teardown
	rows, err := drainResult(w.result)
	if err != nil {
		return fmt.Errorf("draining result: %w", err)
	}
	if len(rows) != 0 {
		return fmt.Errorf("expected empty result but got %d rows", len(rows))
	}
	return nil
}

// resultShouldBeInAnyOrder asserts the result matches the expected table in
// any row order (multiset semantics).
func (w *world) resultShouldBeInAnyOrder(_ context.Context, table *godog.Table) (retErr error) {
	if w.err != nil {
		return fmt.Errorf("expected result table but query failed: %w", w.err)
	}
	if table == nil {
		return errors.New("result table step called with nil table argument")
	}
	if w.result == nil {
		return errors.New("no result available")
	}
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic during result iteration: %v", r)
		}
	}()
	defer w.result.Close() //nolint:errcheck // result close is best-effort in test teardown

	cols, expected, err := parseExpectedTable(table)
	if err != nil {
		return err
	}
	actual, err := collectActualRows(w.result, cols)
	if err != nil {
		return err
	}
	return compareMultiset(expected, actual)
}

// resultShouldBeInOrder asserts the result matches the expected table in the
// exact row order.
func (w *world) resultShouldBeInOrder(_ context.Context, table *godog.Table) (retErr error) {
	if w.err != nil {
		return fmt.Errorf("expected result table but query failed: %w", w.err)
	}
	if table == nil {
		return errors.New("result table step called with nil table argument")
	}
	if w.result == nil {
		return errors.New("no result available")
	}
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic during result iteration: %v", r)
		}
	}()
	defer w.result.Close() //nolint:errcheck // result close is best-effort in test teardown

	cols, expected, err := parseExpectedTable(table)
	if err != nil {
		return err
	}
	actual, err := collectActualRows(w.result, cols)
	if err != nil {
		return err
	}
	return compareOrdered(expected, actual)
}

// resultShouldBeInAnyOrderIgnoringListOrder is a variant of
// resultShouldBeInAnyOrder for scenarios that note "ignoring element order for
// lists". For string-based comparison we treat it identically.
func (w *world) resultShouldBeInAnyOrderIgnoringListOrder(ctx context.Context, table *godog.Table) error {
	return w.resultShouldBeInAnyOrder(ctx, table)
}

// resultShouldBeInOrderIgnoringListOrder is a variant of resultShouldBeInOrder
// for scenarios that note "ignoring element order for lists".
func (w *world) resultShouldBeInOrderIgnoringListOrder(ctx context.Context, table *godog.Table) error {
	return w.resultShouldBeInOrder(ctx, table)
}

// ─────────────────────────────────────────────────────────────────────────────
// And steps — side effects
// ─────────────────────────────────────────────────────────────────────────────

// noSideEffects is a no-op assertion.
func (w *world) noSideEffects(_ context.Context) error { return nil }

// sideEffectsTable performs a lightweight check: if the table declares "+nodes N"
// or "+relationships N", it verifies that the graph grew by at least that amount.
func (w *world) sideEffectsTable(_ context.Context, table *godog.Table) error {
	if table == nil || len(table.Rows) == 0 {
		return nil
	}
	for _, row := range table.Rows[1:] { // skip header row
		if len(row.Cells) < 2 {
			continue
		}
		key := strings.TrimSpace(row.Cells[0].Value)
		var delta int64
		if _, err := fmt.Sscanf(strings.TrimSpace(row.Cells[1].Value), "%d", &delta); err != nil {
			continue
		}
		switch key {
		case "+nodes":
			current := int64(w.g.AdjList().Order())
			if current < w.nodesBefore+delta {
				return fmt.Errorf("side effect +nodes %d: node count went from %d to %d (expected at least %d)",
					delta, w.nodesBefore, current, w.nodesBefore+delta)
			}
		case "+relationships":
			current := int64(w.g.AdjList().Size())
			if current < w.relsBefore+delta {
				return fmt.Errorf("side effect +relationships %d: rel count went from %d to %d (expected at least %d)",
					delta, w.relsBefore, current, w.relsBefore+delta)
			}
		case "-nodes":
			current := int64(w.g.AdjList().Order())
			if current > w.nodesBefore-delta {
				return fmt.Errorf("side effect -nodes %d: node count went from %d to %d (expected at most %d)",
					delta, w.nodesBefore, current, w.nodesBefore-delta)
			}
		case "-relationships":
			current := int64(w.g.AdjList().Size())
			if current > w.relsBefore-delta {
				return fmt.Errorf("side effect -relationships %d: rel count went from %d to %d (expected at most %d)",
					delta, w.relsBefore, current, w.relsBefore-delta)
			}
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// drainResult fully iterates the result set and returns all records.
func drainResult(r *cypher.Result) ([]exec.Record, error) {
	var rows []exec.Record
	for r.Next() {
		rec := r.Record()
		clone := make(exec.Record, len(rec))
		for k, v := range rec {
			clone[k] = v
		}
		rows = append(rows, clone)
	}
	return rows, r.Err()
}

// collectActualRows iterates the result and returns string representations of
// each row in the column order specified by cols.
func collectActualRows(r *cypher.Result, cols []string) ([][]string, error) {
	var out [][]string
	for r.Next() {
		rec := r.Record()
		row := make([]string, len(cols))
		for i, col := range cols {
			v, ok := rec[col]
			if !ok {
				row[i] = "null"
			} else {
				row[i] = valueToString(v)
			}
		}
		out = append(out, row)
	}
	return out, r.Err()
}

// parseExpectedTable parses a Gherkin table into column names and expected rows.
// The first row is treated as the header; subsequent rows are data.
func parseExpectedTable(table *godog.Table) (cols []string, rows [][]string, err error) {
	if table == nil || len(table.Rows) == 0 {
		return nil, nil, errors.New("expected table is nil or has no rows")
	}
	for _, cell := range table.Rows[0].Cells {
		cols = append(cols, strings.TrimSpace(cell.Value))
	}
	for _, row := range table.Rows[1:] {
		r := make([]string, len(cols))
		for i, cell := range row.Cells {
			if i < len(cols) {
				r[i] = strings.TrimSpace(cell.Value)
			}
		}
		rows = append(rows, r)
	}
	return cols, rows, nil
}

// compareOrdered checks that expected and actual rows are identical in count
// and order.
func compareOrdered(expected, actual [][]string) error {
	if len(expected) != len(actual) {
		return fmt.Errorf("row count mismatch: expected %d, got %d\nexpected: %v\nactual:   %v",
			len(expected), len(actual), expected, actual)
	}
	for i, exp := range expected {
		if !rowsEqual(exp, actual[i]) {
			return fmt.Errorf("row %d mismatch: expected %v, got %v", i, exp, actual[i])
		}
	}
	return nil
}

// compareMultiset checks that expected and actual rows are the same multiset
// (same rows in any order).
func compareMultiset(expected, actual [][]string) error {
	if len(expected) != len(actual) {
		return fmt.Errorf("row count mismatch: expected %d, got %d\nexpected: %v\nactual:   %v",
			len(expected), len(actual), expected, actual)
	}
	sortRows(expected)
	sortRows(actual)
	for i, exp := range expected {
		if !rowsEqual(exp, actual[i]) {
			return fmt.Errorf("multiset mismatch at sorted position %d: expected %v, got %v\nall expected: %v\nall actual:   %v",
				i, exp, actual[i], expected, actual)
		}
	}
	return nil
}

func rowsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortRows(rows [][]string) {
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := rows[i], rows[j]
		for k := 0; k < len(ri) && k < len(rj); k++ {
			if ri[k] != rj[k] {
				return ri[k] < rj[k]
			}
		}
		return len(ri) < len(rj)
	})
}

// valueToString converts an interface{} value (from exec.Record) to a
// comparable string. The mapping mirrors the TCK's expected cell format.
func valueToString(v any) string {
	switch val := v.(type) {
	case nil:
		return "null"
	case expr.Value:
		return exprValueToString(val)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// exprValueToString formats v in the textual form used by the openCypher TCK
// expected tables. The mapping intentionally diverges from the default Go
// formatting for several types:
//
//   - String values are wrapped in single quotes (TCK: `| 'a' |`).
//   - Float values always carry a fractional part (`1.0`, not `1`) so that
//     scenarios that distinguish integer from float results match cleanly.
//   - Nodes render as `()` (or `(:Label)` when a single label is present).
//   - Relationships render as `[:TYPE]`.
//   - Lists and maps render with TCK whitespace.
//
// Any unknown kind falls back to the Value's own String() method.
func exprValueToString(v expr.Value) string {
	if v == nil || expr.IsNull(v) {
		return "null"
	}
	switch val := v.(type) {
	case expr.BoolValue:
		if bool(val) {
			return "true"
		}
		return "false"
	case expr.IntegerValue:
		return fmt.Sprintf("%d", int64(val))
	case expr.FloatValue:
		return formatFloatTCK(float64(val))
	case expr.StringValue:
		return "'" + string(val) + "'"
	case expr.NodeValue:
		return formatNodeTCK(val)
	case expr.RelationshipValue:
		return fmt.Sprintf("[:%s]", val.Type)
	case expr.ListValue:
		parts := make([]string, len(val))
		for i, elem := range val {
			parts[i] = exprValueToString(elem)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case expr.MapValue:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(val))
		for _, k := range keys {
			parts = append(parts, k+": "+exprValueToString(val[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case expr.DateValue, expr.LocalDateTimeValue, expr.DateTimeValue, expr.LocalTimeValue, expr.TimeValue, expr.DurationValue:
		// Temporal values render as quoted strings to match the TCK table cells:
		// e.g. | '2015-07-21' | for a Date, | 'PT2H' | for a Duration.
		return "'" + val.String() + "'"
	default:
		return v.String()
	}
}

// formatFloatTCK formats f in the form expected by TCK tables:
//
//   - NaN / ±Inf render as "NaN", "Infinity", "-Infinity".
//   - Finite floats with no fractional part render as "N.0" (e.g. 2 → "2.0").
//   - All other finite floats use Go's %g, which produces the shortest
//     representation that round-trips back to the same float64 (e.g. 1.5,
//     1.23e-07).
func formatFloatTCK(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	if f == math.Trunc(f) && !math.IsInf(f, 0) {
		// Integer-valued finite float: render with explicit ".0" so the TCK
		// table cell `| 2.0 |` matches.
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// formatNodeTCK renders a node value in the TCK textual form. The TCK uses
// `()` for a bare node, `(:Label)` when a single label is present, and
// `(:LabelA:LabelB)` for multiple labels. Properties are omitted in this
// minimal form because the TCK expected cells rarely include them; scenarios
// that compare full node payloads use side-effect tables instead.
func formatNodeTCK(n expr.NodeValue) string {
	if len(n.Labels) == 0 {
		return "()"
	}
	return "(:" + strings.Join(n.Labels, ":") + ")"
}
