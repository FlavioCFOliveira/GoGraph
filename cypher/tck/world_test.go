package tck_test

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"gograph/cypher"
	"gograph/cypher/procs"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// world holds per-scenario execution state for the godog TCK runner.
// A fresh world is created for each scenario via [newWorld].
//
// world is NOT safe for concurrent use.
type world struct {
	g      *lpg.Graph[string, float64]
	eng    *cypher.Engine
	result *cypher.Result
	err    error
	params map[string]any

	// queryCancel cancels the context used by the last whenExecutingQuery call.
	// It is used to abort result iteration if the engine goroutines hang.
	queryCancel context.CancelFunc

	// nodesBefore / relsBefore capture graph size before the test query runs,
	// enabling lightweight side-effect verification.
	nodesBefore int64
	relsBefore  int64
	// Per-direction counters snapshotted before the test query runs.
	// openCypher's side-effect spec tracks ADDITIONS and REMOVALS
	// separately (not net change), so the comparator subtracts the
	// before-snapshot from the after-snapshot of each counter.
	nodesAddedBefore   uint64
	nodesRemovedBefore uint64
	edgesAddedBefore   uint64
	edgesRemovedBefore uint64
}

// newWorld allocates a fresh world backed by an empty directed graph.
func newWorld() *world {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return &world{g: g, eng: cypher.NewEngine(g)}
}

// ─────────────────────────────────────────────────────────────────────────────
// Given steps
// ─────────────────────────────────────────────────────────────────────────────

// givenAnEmptyGraph resets the world to a fresh empty graph.
func (w *world) givenAnEmptyGraph(_ context.Context) error {
	if w.queryCancel != nil {
		w.queryCancel()
		w.queryCancel = nil
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	w.g = g
	w.eng = cypher.NewEngine(g)
	w.result = nil
	w.err = nil
	w.params = nil
	return nil
}

// givenAnyGraph is a no-op — the existing graph (empty by default) is used.
func (w *world) givenAnyGraph(_ context.Context) error { return nil }

// givenBinaryTree1 builds the canonical openCypher TCK binary-tree-1 graph.
// 13 nodes (a:A + b1..b4:X + c11..c42:X), edges :KNOWS / :FOLLOWS / :FRIEND.
// Source: github.com/opencypher/openCypher tck/graphs/binary-tree-1/binary-tree-1.cypher
func (w *world) givenBinaryTree1(ctx context.Context) error {
	if err := w.givenAnEmptyGraph(ctx); err != nil {
		return err
	}
	return w.runSetup(ctx, `
CREATE (a:A {name: 'a'})
CREATE (b1:X {name: 'b1'})
CREATE (b2:X {name: 'b2'})
CREATE (b3:X {name: 'b3'})
CREATE (b4:X {name: 'b4'})
CREATE (c11:X {name: 'c11'})
CREATE (c12:X {name: 'c12'})
CREATE (c21:X {name: 'c21'})
CREATE (c22:X {name: 'c22'})
CREATE (c31:X {name: 'c31'})
CREATE (c32:X {name: 'c32'})
CREATE (c41:X {name: 'c41'})
CREATE (c42:X {name: 'c42'})
CREATE (a)-[:KNOWS]->(b1)
CREATE (a)-[:KNOWS]->(b2)
CREATE (a)-[:FOLLOWS]->(b3)
CREATE (a)-[:FOLLOWS]->(b4)
CREATE (b1)-[:FRIEND]->(c11)
CREATE (b1)-[:FRIEND]->(c12)
CREATE (b2)-[:FRIEND]->(c21)
CREATE (b2)-[:FRIEND]->(c22)
CREATE (b3)-[:FRIEND]->(c31)
CREATE (b3)-[:FRIEND]->(c32)
CREATE (b4)-[:FRIEND]->(c41)
CREATE (b4)-[:FRIEND]->(c42)
CREATE (b1)-[:FRIEND]->(b2)
CREATE (b2)-[:FRIEND]->(b3)
CREATE (b3)-[:FRIEND]->(b4)
CREATE (b4)-[:FRIEND]->(b1)
`)
}

// givenBinaryTree2 builds the canonical openCypher TCK binary-tree-2 graph.
// Same topology as binary-tree-1 but the cN2 leaves (c12, c22, c32, c42)
// carry label :Y instead of :X.
// Source: github.com/opencypher/openCypher tck/graphs/binary-tree-2/binary-tree-2.cypher
func (w *world) givenBinaryTree2(ctx context.Context) error {
	if err := w.givenAnEmptyGraph(ctx); err != nil {
		return err
	}
	return w.runSetup(ctx, `
CREATE (a:A {name: 'a'})
CREATE (b1:X {name: 'b1'})
CREATE (b2:X {name: 'b2'})
CREATE (b3:X {name: 'b3'})
CREATE (b4:X {name: 'b4'})
CREATE (c11:X {name: 'c11'})
CREATE (c12:Y {name: 'c12'})
CREATE (c21:X {name: 'c21'})
CREATE (c22:Y {name: 'c22'})
CREATE (c31:X {name: 'c31'})
CREATE (c32:Y {name: 'c32'})
CREATE (c41:X {name: 'c41'})
CREATE (c42:Y {name: 'c42'})
CREATE (a)-[:KNOWS]->(b1)
CREATE (a)-[:KNOWS]->(b2)
CREATE (a)-[:FOLLOWS]->(b3)
CREATE (a)-[:FOLLOWS]->(b4)
CREATE (b1)-[:FRIEND]->(c11)
CREATE (b1)-[:FRIEND]->(c12)
CREATE (b2)-[:FRIEND]->(c21)
CREATE (b2)-[:FRIEND]->(c22)
CREATE (b3)-[:FRIEND]->(c31)
CREATE (b3)-[:FRIEND]->(c32)
CREATE (b4)-[:FRIEND]->(c41)
CREATE (b4)-[:FRIEND]->(c42)
CREATE (b1)-[:FRIEND]->(b2)
CREATE (b2)-[:FRIEND]->(b3)
CREATE (b3)-[:FRIEND]->(b4)
CREATE (b4)-[:FRIEND]->(b1)
`)
}

// ─────────────────────────────────────────────────────────────────────────────
// And steps
// ─────────────────────────────────────────────────────────────────────────────

// havingExecuted runs a setup query and drains its result.
// Errors during setup are returned immediately (they are setup failures,
// not scenario-level expected errors).
func (w *world) havingExecuted(ctx context.Context, query *godog.DocString) error {
	return w.runSetup(ctx, query.Content)
}

// parametersAre parses the Gherkin table (key | value rows) into w.params.
// The table has no header: each row is a name/value pair where value is a
// Cypher-literal string. The values are parsed best-effort: integers,
// booleans, and quoted strings are detected; everything else is kept as a
// string.
func (w *world) parametersAre(_ context.Context, table *godog.Table) error {
	m := make(map[string]any, len(table.Rows))
	for _, row := range table.Rows {
		if len(row.Cells) < 2 {
			continue
		}
		key := strings.TrimSpace(row.Cells[0].Value)
		raw := strings.TrimSpace(row.Cells[1].Value)
		m[key] = parseCypherLiteral(raw)
	}
	w.params = m
	return nil
}

// parseCypherLiteral converts a raw Gherkin cell value to a Go primitive
// suitable for passing to [cypher.Engine.RunAny].
func parseCypherLiteral(s string) any {
	s = strings.TrimSpace(s)
	// Boolean
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	// Integer
	var i int64
	if _, err := fmt.Sscanf(s, "%d", &i); err == nil && fmt.Sprintf("%d", i) == s {
		return i
	}
	// Float
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err == nil {
		return f
	}
	// Single-quoted string → strip quotes
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return unescapeCypherString(s[1 : len(s)-1])
	}
	// Double-quoted string → strip quotes
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return unescapeCypherString(s[1 : len(s)-1])
	}
	// List literal: [e1, e2, …]
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return parseCypherList(s[1 : len(s)-1])
	}
	// Map literal: {k1: v1, k2: v2, …}
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		return parseCypherMap(s[1 : len(s)-1])
	}
	return s
}

// unescapeCypherString handles common Cypher string escape sequences.
func unescapeCypherString(s string) string {
	return strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`).Replace(s)
}

// parseCypherList parses the inner content of a Cypher list literal
// (everything between the outer brackets) into a []any.
func parseCypherList(inner string) []any {
	parts := splitTopLevelCypherCommas(inner)
	result := make([]any, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			result = append(result, parseCypherLiteral(p))
		}
	}
	return result
}

// parseCypherMap parses the inner content of a Cypher map literal
// (everything between the outer braces) into a map[string]any.
func parseCypherMap(inner string) map[string]any {
	parts := splitTopLevelCypherCommas(inner)
	result := make(map[string]any, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Split on the first top-level colon.
		colonIdx := topLevelColonIdx(p)
		if colonIdx < 0 {
			continue
		}
		key := strings.TrimSpace(p[:colonIdx])
		val := strings.TrimSpace(p[colonIdx+1:])
		result[key] = parseCypherLiteral(val)
	}
	return result
}

// splitTopLevelCypherCommas splits s on commas not nested inside brackets or
// braces, returning trimmed tokens.
func splitTopLevelCypherCommas(s string) []string {
	var parts []string
	depth := 0
	inStr := false
	strChar := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			if ch == '\\' {
				i++ // skip escaped char
				continue
			}
			if ch == strChar {
				inStr = false
			}
			continue
		}
		switch ch {
		case '\'', '"':
			inStr = true
			strChar = ch
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		parts = append(parts, tail)
	}
	return parts
}

// topLevelColonIdx returns the index of the first colon in s that is not
// inside brackets, braces, or a quoted string. Returns -1 if not found.
func topLevelColonIdx(s string) int {
	depth := 0
	inStr := false
	strChar := byte(0)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			if ch == '\\' {
				i++
				continue
			}
			if ch == strChar {
				inStr = false
			}
			continue
		}
		switch ch {
		case '\'', '"':
			inStr = true
			strChar = ch
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		case ':':
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// thereExistsAProcedure parses a TCK procedure declaration and registers
// the procedure on the world's engine so subsequent CALL invocations
// resolve against the declared signature and result table.
//
// Accepted signature forms:
//
//	test.doNothing() :: ()
//	test.my.proc(name :: STRING?, id :: INTEGER?) :: (city :: STRING?, country_code :: INTEGER?)
//
// The result table (when present) lists rows with input columns followed
// by output columns. Calls filter on the input columns and yield the
// matching output columns. Procedures with empty output (e.g. doNothing)
// are registered with an impl that always returns no rows; the
// exec.ProcedureCallOp's void-proc passthrough preserves the driver row.
//
// Duplicate registrations within a single scenario are tolerated as
// idempotent: TCK backgrounds sometimes declare the same procedure on
// multiple Given/And lines.
func (w *world) thereExistsAProcedure(_ context.Context, sig string, table *godog.Table) error {
	parsed, err := parseProcedureSignature(sig)
	if err != nil {
		return fmt.Errorf("there exists a procedure: %w", err)
	}
	impl, err := buildProcImplFromTable(&parsed, table)
	if err != nil {
		return fmt.Errorf("there exists a procedure %q: %w", parsed.fqn(), err)
	}
	if regErr := w.eng.Procs().Register(parsed.Signature, impl); regErr != nil {
		if errors.Is(regErr, procs.ErrProcAlreadyExists) {
			return nil
		}
		return fmt.Errorf("register %q: %w", parsed.fqn(), regErr)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// runSetup executes a Cypher query that is part of scenario setup (Given /
// havingExecuted). It drains the result set fully and closes it. Errors are
// propagated to the caller.
//
// Panics from the engine are recovered and returned as errors so that the
// godog scenario fails gracefully.
func (w *world) runSetup(ctx context.Context, query string) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("setup panic: %v", r)
		}
	}()
	res, err := w.eng.RunInTxAny(ctx, query, nil)
	if err != nil {
		return fmt.Errorf("setup query failed: %w", err)
	}
	for res.Next() {
		// drain
	}
	if err := res.Err(); err != nil {
		_ = res.Close() //nolint:errcheck // best-effort cleanup after drain error
		return fmt.Errorf("setup query iteration: %w", err)
	}
	return res.Close()
}

// snapshotCounts records the current node and relationship counts and the
// per-direction side-effect counters before the test query runs so the
// comparator can verify ADD-vs-REMOVE counts independently.
func (w *world) snapshotCounts() {
	w.nodesBefore = int64(w.g.LiveOrder())
	w.relsBefore = int64(w.g.AdjList().Size())
	w.nodesAddedBefore, w.nodesRemovedBefore, w.edgesAddedBefore, w.edgesRemovedBefore = w.g.SideEffectCounters()
}
