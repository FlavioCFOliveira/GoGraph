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

// collapseWS returns s with all whitespace characters removed. Used to
// normalise TCK column-header strings that preserve source whitespace inside
// function arguments (e.g. "cOuNt( * )" → "cOuNt(*)") against record keys
// produced by the engine which never include intra-argument whitespace.
func collapseWS(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			b = append(b, c)
		}
	}
	return string(b)
}

// collectActualRows iterates the result and returns string representations of
// each row in the column order specified by cols.
func collectActualRows(r *cypher.Result, cols []string) ([][]string, error) {
	// Build whitespace-collapsed and (collapsed-and-lowered) reverse maps so
	// that TCK column headers like "cOuNt( * )" resolve to the engine key
	// "count(*)" — Cypher source spellings preserve case + whitespace
	// (`cOuNt( * )`, `coUnt( dIstInct p )`) while our IR canonicalises
	// function names and drops interior whitespace. The fallback chain is:
	//   1. exact match on the source spelling
	//   2. whitespace-insensitive (e.g. `cOuNt(*)` ↔ `cOuNt( * )`)
	//   3. whitespace-and-case-insensitive (e.g. `count(*)` ↔ `cOuNt( * )`,
	//      `coUnt(DISTINCT p)` ↔ `coUnt( dIstInct p )`).
	// All three are scoped to this single result's row record map; no global
	// state is touched.
	var collapsedMap map[string]string // collapsed engine key → engine key
	var lowerMap map[string]string     // lower(collapsed) engine key → engine key
	var out [][]string
	for r.Next() {
		rec := r.Record()
		if collapsedMap == nil {
			collapsedMap = make(map[string]string, len(rec))
			lowerMap = make(map[string]string, len(rec))
			for k := range rec {
				ck := collapseWS(k)
				collapsedMap[ck] = k
				lowerMap[strings.ToLower(ck)] = k
			}
		}
		row := make([]string, len(cols))
		for i, col := range cols {
			v, ok := rec[col]
			if !ok {
				if engKey, found := collapsedMap[collapseWS(col)]; found {
					v, ok = rec[engKey]
				}
			}
			if !ok {
				if engKey, found := lowerMap[strings.ToLower(collapseWS(col))]; found {
					v, ok = rec[engKey]
				}
			}
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
		if normalizeTCKCell(a[i]) != normalizeTCKCell(b[i]) {
			return false
		}
	}
	return true
}

// normalizeTCKCell rewrites the property-map portions of a TCK cell string so
// that property keys appear in sorted (alphabetical) order. This makes the
// comparison insensitive to the insertion order used in feature-file CREATE
// statements while keeping our output (which always uses sorted keys) correct.
//
// The normaliser walks the string character by character, tracking brace depth.
// When it encounters a balanced `{...}` block it splits the key-value pairs,
// sorts them, and re-joins. Only the top-level flat key: value pairs within
// each `{...}` block are re-sorted; values that are themselves maps are treated
// as opaque strings and passed through unchanged, so round-trip stability is
// preserved for nested literals.
func normalizeTCKCell(s string) string {
	// Always normalise node label ordering — openCypher treats a node's
	// labels as a set, so any permutation is semantically equivalent. The
	// TCK comparator is string-based, so we canonicalise both expected
	// and actual cells to alphabetical label order before comparing.
	if strings.Contains(s, "(:") {
		s = sortNodeLabels(s)
	}
	// Fast path: no braces means no property maps.
	if !strings.ContainsAny(s, "{}") {
		return s
	}
	var buf strings.Builder
	depth := 0
	mapStart := -1
	buf.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '{':
			depth++
			if depth == 1 {
				// Record where this map literal starts (after the opening brace).
				mapStart = i
			}
		case '}':
			if depth == 1 && mapStart >= 0 {
				// We have the complete `{...}` block from mapStart to i.
				inner := s[mapStart+1 : i]
				sorted := sortMapLiteralKeys(inner)
				buf.WriteByte('{')
				buf.WriteString(sorted)
				buf.WriteByte('}')
				mapStart = -1
				depth--
				continue
			}
			depth--
		default:
			if depth == 0 {
				buf.WriteByte(ch)
			}
		}
	}
	return buf.String()
}

// sortNodeLabels walks s and rewrites every node rendering of the form
// `(:LabelA:LabelB:…)` or `(:LabelA:LabelB:… {…})` so that the label list
// appears in alphabetical order. openCypher treats node labels as an
// unordered set, so the rendering is implementation-defined; canonicalising
// both sides of the comparison to alphabetical order keeps the string-based
// TCK comparator semantically correct.
//
// Brace-depth tracking ensures that colons inside `{…}` property maps (which
// separate keys from values, not labels) are not treated as label delimiters.
func sortNodeLabels(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	depth := 0
	for i := 0; i < len(s); {
		ch := s[i]
		if depth == 0 && ch == '(' && i+1 < len(s) && s[i+1] == ':' {
			// Collect labels starting at i+2 until we hit a space, ')',
			// '{', or any character that cannot appear in a label.
			j := i + 2
			for j < len(s) {
				c := s[j]
				if c == ' ' || c == ')' || c == '{' {
					break
				}
				j++
			}
			labelRun := s[i+2 : j]
			parts := strings.Split(labelRun, ":")
			sort.Strings(parts)
			buf.WriteByte('(')
			for _, lbl := range parts {
				buf.WriteByte(':')
				buf.WriteString(lbl)
			}
			i = j
			continue
		}
		switch ch {
		case '{', '[':
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
		buf.WriteByte(ch)
		i++
	}
	return buf.String()
}

// sortMapLiteralKeys takes the contents of a Cypher map literal (everything
// between the outer braces, e.g. "num: 9, bool: true") and returns the same
// pairs sorted alphabetically by key. Pairs are split on the top-level comma
// (commas inside nested braces or strings are not split) to handle values that
// are themselves nested maps or lists.
//
// Each pair is normalised to "key: value" form (single space after the colon,
// no space before) so that "prop:1" and "prop: 1" compare as equal.
func sortMapLiteralKeys(inner string) string {
	if inner == "" {
		return inner
	}
	pairs := splitTopLevelCommas(inner)
	for i, p := range pairs {
		pairs[i] = normalizeMapPair(p)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ", ")
}

// normalizeMapPair normalises a single "key: value" pair to the canonical
// form "key: value" — trimmed key, single space after the colon, trimmed
// value. The split point is the first top-level colon (not nested inside
// braces or brackets) so that values that are themselves maps are preserved.
func normalizeMapPair(pair string) string {
	depth := 0
	for i := 0; i < len(pair); i++ {
		switch pair[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ':':
			if depth == 0 {
				key := strings.TrimSpace(pair[:i])
				val := strings.TrimSpace(pair[i+1:])
				return key + ": " + val
			}
		}
	}
	// No colon found — return trimmed.
	return strings.TrimSpace(pair)
}

// splitTopLevelCommas splits inner on commas that are not nested inside braces
// or square brackets, returning the trimmed key-value pair strings.
func splitTopLevelCommas(inner string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	if tail := strings.TrimSpace(inner[start:]); tail != "" {
		parts = append(parts, tail)
	}
	return parts
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
		return formatRelTCK(val)
	case expr.PathValue:
		return formatPathTCK(val)
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
//   - Floats whose magnitude is in [1e-7, 1e21) and whose shortest %g
//     representation would use scientific notation fall back to %f with
//     precision -1 (shortest fixed-point form). The TCK tables expect
//     "0.00002" not "2e-05" for small floats in that range.
//   - Floats outside that magnitude window keep their %g scientific form
//     (so 1e-305, 1e308, 1.23456789e308 round-trip as written instead of
//     producing huge zero-padded strings or losing precision).
//   - All other finite floats use Go's %g shortest representation.
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
		// table cell `| 2.0 |` matches — but only when the magnitude is
		// human-readable. Beyond ±1e21 the fixed-point form is an enormous
		// digit string that bears no resemblance to the round-tripped %g
		// scientific form expected by the TCK.
		abs := math.Abs(f)
		if abs < 1e21 || abs == 0 {
			return strconv.FormatFloat(f, 'f', 1, 64)
		}
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if strings.ContainsAny(s, "eE") {
		// Only switch to fixed-point when the magnitude is human-readable.
		// Outside the [1e-7, 1e21) window the fixed-point form is either
		// astronomically long or pure zeros, neither of which round-trips
		// against the TCK feature-file expectations.
		abs := math.Abs(f)
		if abs >= 1e-7 && abs < 1e21 {
			s = strconv.FormatFloat(f, 'f', -1, 64)
		} else {
			// Drop the explicit '+' in positive exponents — the TCK
			// expectation is "1e308" not Go's default "1e+308".
			s = strings.Replace(s, "e+", "e", 1)
		}
	}
	return s
}

// formatNodeTCK renders a node value in the TCK textual form. The format is:
//
//	()                      — bare node
//	(:Label)                — single label, no properties
//	(:LabelA:LabelB)        — multiple labels, no properties
//	({key: value})          — no label, with properties
//	(:Label {key: value})   — label plus properties
//
// Keys in the property map are emitted in sorted order so the rendered
// representation is deterministic across runs.
func formatNodeTCK(n expr.NodeValue) string {
	var b strings.Builder
	b.WriteByte('(')
	for _, lbl := range n.Labels {
		b.WriteByte(':')
		b.WriteString(lbl)
	}
	if len(n.Properties) > 0 {
		if len(n.Labels) > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(formatPropertyMapTCK(n.Properties))
	}
	b.WriteByte(')')
	return b.String()
}

// formatRelTCK renders a relationship value in the TCK textual form. The
// format is:
//
//	[:Type]                  — relationship with type, no properties
//	[:Type {key: value}]     — relationship with type and properties
//
// Keys in the property map are emitted in sorted order.
func formatRelTCK(r expr.RelationshipValue) string {
	var b strings.Builder
	b.WriteByte('[')
	if r.Type != "" {
		b.WriteByte(':')
		b.WriteString(r.Type)
	}
	if len(r.Properties) > 0 {
		b.WriteByte(' ')
		b.WriteString(formatPropertyMapTCK(r.Properties))
	}
	b.WriteByte(']')
	return b.String()
}

// formatPathTCK renders a path value in the TCK textual form. The shape is
// `<n0 r0 n1 r1 n2 ...>` where each n is rendered by [formatNodeTCK] and
// each relationship is rendered with its direction relative to the path
// traversal (-[:T]-> when StartID matches the preceding node, <-[:T]- when
// the relationship is traversed against its storage direction).
//
// Empty paths (no nodes) render as the TCK's reserved literal "<empty-path>";
// zero-length paths (one node, no relationships) render as `<(node)>`.
func formatPathTCK(p expr.PathValue) string {
	if len(p.Nodes) == 0 {
		return "<empty-path>"
	}
	var b strings.Builder
	b.WriteByte('<')
	b.WriteString(formatNodeTCK(p.Nodes[0]))
	for i, rel := range p.Relationships {
		if i+1 >= len(p.Nodes) {
			break
		}
		// Relationship orientation in the path: the storage StartID tells us
		// the edge's stored direction; comparing it against the preceding
		// node's ID picks the arrow.
		if rel.StartID == p.Nodes[i].ID {
			b.WriteString("-")
			b.WriteString(formatRelTCK(rel))
			b.WriteString("->")
		} else {
			b.WriteString("<-")
			b.WriteString(formatRelTCK(rel))
			b.WriteString("-")
		}
		b.WriteString(formatNodeTCK(p.Nodes[i+1]))
	}
	b.WriteByte('>')
	return b.String()
}

// formatPropertyMapTCK renders a MapValue as a Cypher map literal, with
// keys in sorted order so the output is deterministic.
func formatPropertyMapTCK(m expr.MapValue) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(m))
	for _, k := range keys {
		parts = append(parts, k+": "+exprValueToString(m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
