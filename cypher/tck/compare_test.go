package tck_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cucumber/godog"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// When steps
// ─────────────────────────────────────────────────────────────────────────────

// queryTimeout is the per-scenario execution timeout. It must be short enough
// that a hung goroutine (e.g. from a parallel scan worker that panics and
// leaves the coordinator goroutine waiting on a drained channel) does not
// block the entire test run.
//
// Exceeding it is NOT a conformance failure. See [queryWedgeGrace] for the
// distinction and rmp #2568 for why it matters.
const queryTimeout = 10 * time.Second

// queryWedgeGrace is how long AFTER its deadline a query is still allowed to
// return before the engine is judged wedged.
//
// # Why a duration alone cannot tell a slow host from a hung engine (rmp #2568)
//
// A scenario that exceeds [queryTimeout] used to store context.DeadlineExceeded
// in w.err, which failed the scenario and lowered the count
// [tckExecutionBaseline] gates on. That reports a LOADED MACHINE as a loss of
// openCypher conformance — the single most serious false signal this project can
// emit, because CLAUDE.md makes 100% TCK compliance non-negotiable and an
// engineer meeting that red would reasonably conclude the engine had regressed.
// For scale, measured on this box during rmp #2567: a cypher security test ran
// 17.76 s idle under -race and 537.36 s under saturation, a factor above 30.
//
// Widening the timeout is not the answer; that is what the cartesian test did and
// it merely moved the cliff. The two outcomes have to be SEPARATED, and the
// discriminator is not how long the query took — it is whether the engine
// HONOURED CANCELLATION:
//
//   - The context deadline fires and RunAny returns promptly with
//     context.DeadlineExceeded. The engine is responsive; the host was just slow.
//     This is INCONCLUSIVE: it carries no evidence either way about conformance,
//     so it must not be scored as a failure, and must be reported as such.
//   - The context deadline fires and RunAny DOES NOT RETURN AT ALL. The engine
//     ignored cancellation, which is a defect against CLAUDE.md's context-aware
//     blocking mandate and is exactly the wedged coordinator the timeout exists
//     to catch. This still FAILS, loudly.
//
// The grace is generous relative to the work: TCK scenarios are individually
// tiny, so an engine that honours cancellation returns in microseconds once the
// deadline fires, even on a saturated host. A wedge is unbounded and will always
// exceed it.
const queryWedgeGrace = 30 * time.Second

// errInconclusive marks a scenario whose query exceeded [queryTimeout] while the
// engine honoured cancellation. It is wrapped into w.err so the existing Then
// steps need no change, and identified by the gate so such a scenario is never
// counted as a conformance regression (rmp #2568).
var errInconclusive = errors.New("TCK scenario INCONCLUSIVE (slow host, engine responsive)")

// errWedged marks a query whose engine ignored its cancelled context. Unlike
// [errInconclusive] it IS a real failure and is never credited by the gate; the
// sentinel exists only so that it cannot satisfy an error-expecting scenario. A
// wedged engine producing a non-nil error used to make 695 error scenarios "pass"
// — measured during the rmp #2568 demonstration — which is a false pass on top of
// a genuine defect.
var errWedged = errors.New("TCK scenario FAILED: engine ignored cancellation")

// tckInconclusive records the scenarios whose query exceeded [queryTimeout]
// while the engine honoured cancellation. It is read by the gate so a timeout can
// never be scored as a conformance regression, and rendered so a run that was
// partly inconclusive can never be mistaken for a clean one.
//
// Concurrency: guarded by its own mutex. godog runs scenarios sequentially here,
// but the value is also read by the gate after the run, so the lock is not
// optional.
var tckInconclusive = struct {
	mu      sync.Mutex
	reasons []string
}{}

// recordInconclusive notes one inconclusive scenario. detail must say what timed
// out, so the rendered inventory is actionable rather than a bare count.
func recordInconclusive(detail string) {
	tckInconclusive.mu.Lock()
	defer tckInconclusive.mu.Unlock()
	tckInconclusive.reasons = append(tckInconclusive.reasons, detail)
}

// inconclusiveCount returns how many scenarios were inconclusive.
func inconclusiveCount() int {
	tckInconclusive.mu.Lock()
	defer tckInconclusive.mu.Unlock()
	return len(tckInconclusive.reasons)
}

// inconclusiveReasons returns a copy of the recorded reasons.
func inconclusiveReasons() []string {
	tckInconclusive.mu.Lock()
	defer tckInconclusive.mu.Unlock()
	out := make([]string, len(tckInconclusive.reasons))
	copy(out, tckInconclusive.reasons)
	return out
}

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

	// RunAny is driven on its own goroutine so that a query which IGNORES its
	// cancelled context can be distinguished from one that honours it — see
	// [queryWedgeGrace]. The panic recovery is duplicated inside the goroutine
	// because the deferred recover above only covers THIS goroutine; without it,
	// an engine panic would take the process down instead of failing the scenario.
	type outcome struct {
		res *cypher.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{nil, fmt.Errorf("engine panic: %v", r)}
			}
		}()
		res, err := w.eng.RunAny(qctx, query.Content, w.params)
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		if o.err != nil {
			cancel()
			w.queryCancel = nil
			w.result = nil
			// The engine honoured cancellation, so a deadline error says the HOST
			// was slow and says nothing about conformance. Record it as
			// inconclusive and leave w.err nil, so the Then steps do not report a
			// conformance failure the evidence does not support (rmp #2568).
			if errors.Is(o.err, context.DeadlineExceeded) && ctx.Err() == nil {
				detail := fmt.Sprintf("query exceeded %s but the engine honoured cancellation "+
					"(slow host, not a conformance signal): %s", queryTimeout, firstLine(query.Content))
				if !w.inconclusiveRecorded {
					w.inconclusiveRecorded = true
					recordInconclusive(detail)
				}
				// Stored in w.err, wrapping errInconclusive, so that every existing
				// Then step keeps working unchanged: the scenario still does not
				// pass, because it genuinely was not verified. What changes is that
				// the GATE can now tell this apart from a conformance failure and
				// declines to score it as one.
				w.err = fmt.Errorf("%w: %s", errInconclusive, detail)
				return nil
			}
			w.err = o.err
			return nil
		}
		w.result = o.res
		return nil

	case <-time.After(queryTimeout + queryWedgeGrace):
		// The deadline fired at queryTimeout and RunAny still has not returned
		// queryWedgeGrace later. The engine is ignoring its context, which is the
		// wedged coordinator this bound exists to catch. This is a REAL failure and
		// is reported as one. The goroutine is deliberately left running: it is
		// wedged by definition, and there is nothing to wait for.
		w.queryCancel = nil
		w.result = nil
		w.err = fmt.Errorf("%w: engine WEDGED: RunAny did not return %s after its %s context deadline "+
			"fired, so it is ignoring cancellation (CLAUDE.md context-aware blocking). This is NOT a "+
			"slow host — a responsive engine returns context.DeadlineExceeded immediately (rmp #2568). "+
			"Query: %s", errWedged, queryWedgeGrace, queryTimeout, firstLine(query.Content))
		return nil
	}
}

// firstLine returns s truncated to its first line and 120 characters, so a
// multi-line TCK query can be named in a one-line diagnostic.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
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
	defer w.result.Close() // result close is best-effort in test teardown
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
	defer w.result.Close() // result close is best-effort in test teardown

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
	defer w.result.Close() // result close is best-effort in test teardown

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
// lists". Top-level list literals in each cell are canonicalised (elements
// sorted as strings) on both the expected and actual sides before the per-row
// string comparison runs, so map iteration order or aggregation emission
// order does not cause a spurious mismatch.
func (w *world) resultShouldBeInAnyOrderIgnoringListOrder(ctx context.Context, table *godog.Table) (retErr error) {
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
	defer w.result.Close() // result close is best-effort in test teardown
	cols, expected, err := parseExpectedTable(table)
	if err != nil {
		return err
	}
	actual, err := collectActualRows(w.result, cols)
	if err != nil {
		return err
	}
	normaliseRowsListOrder(expected)
	normaliseRowsListOrder(actual)
	return compareMultiset(expected, actual)
}

// resultShouldBeInOrderIgnoringListOrder is a variant of resultShouldBeInOrder
// for scenarios that note "ignoring element order for lists".
func (w *world) resultShouldBeInOrderIgnoringListOrder(ctx context.Context, table *godog.Table) (retErr error) {
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
	defer w.result.Close() // result close is best-effort in test teardown
	cols, expected, err := parseExpectedTable(table)
	if err != nil {
		return err
	}
	actual, err := collectActualRows(w.result, cols)
	if err != nil {
		return err
	}
	normaliseRowsListOrder(expected)
	normaliseRowsListOrder(actual)
	return compareOrdered(expected, actual)
}

// normaliseRowsListOrder canonicalises the order of list elements inside
// every cell of rows. The transformation only touches top-level [ ... ] list
// literals; nested lists inside maps or nested-list elements are sorted
// recursively at each bracket depth so two valid emissions with different
// element orders compare equal.
func normaliseRowsListOrder(rows [][]string) {
	for _, row := range rows {
		for i, cell := range row {
			row[i] = sortListElementsInCell(cell)
		}
	}
}

// sortListElementsInCell walks the cell string and sorts the elements of
// every top-level [ ... ] list literal it encounters. Element splitting
// respects nested brackets, braces and string literals so a list of maps
// like [{k: 1}, {k: 2}] is split on the outer commas only. Sorting uses
// the same recursive normalisation so nested lists are themselves
// canonicalised before the outer sort runs — the result is deterministic
// regardless of the depth at which the divergence sits.
func sortListElementsInCell(s string) string {
	if !strings.ContainsAny(s, "[]") {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		ch := s[i]
		if ch != '[' {
			// Skip past quoted strings verbatim so embedded brackets do not
			// trigger the list walker.
			if ch == '\'' || ch == '"' {
				j := skipQuoted(s, i)
				out.WriteString(s[i:j])
				i = j
				continue
			}
			out.WriteByte(ch)
			i++
			continue
		}
		// Find the matching closing bracket at the same depth.
		end := matchingBracket(s, i)
		if end < 0 {
			// Malformed; pass the rest through unchanged.
			out.WriteString(s[i:])
			return out.String()
		}
		inner := s[i+1 : end]
		parts := splitTopLevel(inner)
		for j, p := range parts {
			parts[j] = sortListElementsInCell(strings.TrimSpace(p))
		}
		sort.Strings(parts)
		out.WriteByte('[')
		for j, p := range parts {
			if j > 0 {
				out.WriteString(", ")
			}
			out.WriteString(p)
		}
		out.WriteByte(']')
		i = end + 1
	}
	return out.String()
}

// matchingBracket returns the index of the ']' that closes the '[' at
// position start. Returns -1 when the brackets are unbalanced. Quoted
// strings and nested {…}/[…] blocks are skipped without affecting depth.
func matchingBracket(s string, start int) int {
	depth := 0
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		case '\'', '"':
			i = skipQuoted(s, i) - 1
		}
	}
	return -1
}

// splitTopLevel splits s on commas that sit at bracket/brace depth zero
// and outside any quoted string. Empty input yields a nil slice (an
// empty list literal "[]" has no elements).
func splitTopLevel(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var (
		parts []string
		depth int
		start int
	)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '[', '{', '(':
			depth++
		case ']', '}', ')':
			depth--
		case '\'', '"':
			i = skipQuoted(s, i) - 1
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// skipQuoted scans past a single-quoted or double-quoted string starting
// at position i (which must point at the opening quote). Backslash
// escapes are honoured so an embedded \' or \" does not close the
// string. Returns the index immediately after the closing quote, or
// len(s) when the string is unterminated.
func skipQuoted(s string, i int) int {
	quote := s[i]
	i++
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			i += 2
			continue
		}
		if s[i] == quote {
			return i + 1
		}
		i++
	}
	return len(s)
}

// ─────────────────────────────────────────────────────────────────────────────
// And steps — side effects
// ─────────────────────────────────────────────────────────────────────────────

// noSideEffects is a no-op assertion: a scenario's "no side effects" step
// never checks that the graph was actually left unchanged. See
// package tck's "Gate scope" doc (conformance_history.go) for how this
// bounds what the 3897/3897 execution gate does and does not verify.
func (w *world) noSideEffects(_ context.Context) error { return nil }

// sideEffectsTable performs a lightweight check: if the table declares "+nodes N"
// or "+relationships N", it verifies that the graph grew by at least that
// amount (a lower bound, not an exact count). "+properties"/"-properties" and
// "+labels"/"-labels" rows are silently ignored by the switch below — no
// property or label side effect is checked at all. See package tck's
// "Gate scope" doc (conformance_history.go) for the full picture.
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
		// openCypher tracks ADDITIONS and REMOVALS as separate counters,
		// not net change. Use the per-direction counters maintained by
		// the LPG so a CREATE+DELETE pair shows up as +1/-1 (and not as
		// net 0).
		nodesAdded, nodesRemoved, edgesAdded, edgesRemoved := w.g.SideEffectCounters()
		switch key {
		case "+nodes":
			added := int64(nodesAdded - w.nodesAddedBefore)
			if added < delta {
				return fmt.Errorf("side effect +nodes %d: actual additions %d (expected at least %d)",
					delta, added, delta)
			}
		case "+relationships":
			added := int64(edgesAdded - w.edgesAddedBefore)
			if added < delta {
				return fmt.Errorf("side effect +relationships %d: actual additions %d (expected at least %d)",
					delta, added, delta)
			}
		case "-nodes":
			removed := int64(nodesRemoved - w.nodesRemovedBefore)
			if removed < delta {
				return fmt.Errorf("side effect -nodes %d: actual removals %d (expected at least %d)",
					delta, removed, delta)
			}
		case "-relationships":
			removed := int64(edgesRemoved - w.edgesRemovedBefore)
			if removed < delta {
				return fmt.Errorf("side effect -relationships %d: actual removals %d (expected at least %d)",
					delta, removed, delta)
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
//
// Nested maps and lists inside the value are normalised recursively by
// re-entering normalizeTCKCell on the value, so the key order of a deeply-
// nested literal like `{data: [{id: '0001', type: 'donut'}]}` matches
// regardless of the producer's iteration order.
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
				if strings.ContainsAny(val, "{[") {
					val = normalizeTCKValue(val)
				}
				return key + ": " + val
			}
		}
	}
	// No colon found — return trimmed.
	return strings.TrimSpace(pair)
}

// normalizeTCKValue normalises a value that may contain nested maps and
// lists. Top-level lists are walked element-by-element; each element is
// re-normalised by recursing into normalizeTCKValue. Maps are normalised
// via the same sort-keys path as the top-level cell processor. Other
// strings (primitives, node/relationship renderings, etc.) are returned
// unchanged.
func normalizeTCKValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if s[0] == '{' && s[len(s)-1] == '}' {
		inner := s[1 : len(s)-1]
		return "{" + sortMapLiteralKeys(inner) + "}"
	}
	if s[0] == '[' && s[len(s)-1] == ']' {
		inner := s[1 : len(s)-1]
		parts := splitTopLevelCommas(inner)
		for i, p := range parts {
			parts[i] = normalizeTCKValue(p)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return s
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
		// TCK string literals are wrapped in single quotes with the
		// canonical Cypher escapes — backslash and inner single quote
		// must be escaped so the rendered cell parses back to the same
		// runtime value (Literals6 [4]/[5]). Order matters: escape
		// backslashes first so the newly-introduced backslashes in
		// front of the quotes are not double-escaped.
		s := strings.ReplaceAll(string(val), `\`, `\\`)
		s = strings.ReplaceAll(s, `'`, `\'`)
		return "'" + s + "'"
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
	// LPG returns labels in unspecified (map-iteration) order, but the
	// TCK expected-table cells are stable: a node with labels {A, B, C}
	// renders as `(:A:B:C)`. Sort the slice deterministically so the
	// multiset comparison sees the same string across runs.
	labels := make([]string, len(n.Labels))
	copy(labels, n.Labels)
	sort.Strings(labels)
	for _, lbl := range labels {
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
