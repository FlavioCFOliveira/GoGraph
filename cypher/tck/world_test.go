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

// givenBinaryTree1 builds the standard TCK binary-tree-1 graph.
// The graph is a complete binary tree with 3 levels (7 nodes, 6 edges).
// Nodes are labelled Root, A-F; edges have type KNOWS.
//
// Structure:
//
//	    Root
//	   /    \
//	  A      B
//	 / \    / \
//	C   D  E   F
func (w *world) givenBinaryTree1(ctx context.Context) error {
	if err := w.givenAnEmptyGraph(ctx); err != nil {
		return err
	}
	return w.runSetup(ctx, `
CREATE (root:Root {name: 'root'})
CREATE (a:A {name: 'a'})
CREATE (b:B {name: 'b'})
CREATE (c:C {name: 'c'})
CREATE (d:D {name: 'd'})
CREATE (e:E {name: 'e'})
CREATE (f:F {name: 'f'})
CREATE (root)-[:KNOWS]->(a)
CREATE (root)-[:KNOWS]->(b)
CREATE (a)-[:KNOWS]->(c)
CREATE (a)-[:KNOWS]->(d)
CREATE (b)-[:KNOWS]->(e)
CREATE (b)-[:KNOWS]->(f)
`)
}

// givenBinaryTree2 builds the standard TCK binary-tree-2 graph.
func (w *world) givenBinaryTree2(ctx context.Context) error {
	if err := w.givenAnEmptyGraph(ctx); err != nil {
		return err
	}
	return w.runSetup(ctx, `
CREATE (a:A {name: 'a'})
CREATE (b:B {name: 'b'})
CREATE (c:C {name: 'c'})
CREATE (d:D {name: 'd'})
CREATE (e:E {name: 'e'})
CREATE (f:F {name: 'f'})
CREATE (g:G {name: 'g'})
CREATE (h:H {name: 'h'})
CREATE (i:I {name: 'i'})
CREATE (j:J {name: 'j'})
CREATE (k:K {name: 'k'})
CREATE (l:L {name: 'l'})
CREATE (m:M {name: 'm'})
CREATE (n:N {name: 'n'})
CREATE (o:O {name: 'o'})
CREATE (a)-[:KNOWS]->(b)
CREATE (a)-[:KNOWS]->(c)
CREATE (b)-[:KNOWS]->(d)
CREATE (b)-[:KNOWS]->(e)
CREATE (c)-[:KNOWS]->(f)
CREATE (c)-[:KNOWS]->(g)
CREATE (d)-[:KNOWS]->(h)
CREATE (d)-[:KNOWS]->(i)
CREATE (e)-[:KNOWS]->(j)
CREATE (e)-[:KNOWS]->(k)
CREATE (f)-[:KNOWS]->(l)
CREATE (f)-[:KNOWS]->(m)
CREATE (g)-[:KNOWS]->(n)
CREATE (g)-[:KNOWS]->(o)
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
		return s[1 : len(s)-1]
	}
	// Double-quoted string → strip quotes
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
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

// snapshotCounts records the current node and relationship counts before the
// test query runs so side-effect verification can compare against them.
func (w *world) snapshotCounts() {
	w.nodesBefore = int64(w.g.AdjList().Order())
	w.relsBefore = int64(w.g.AdjList().Size())
}
