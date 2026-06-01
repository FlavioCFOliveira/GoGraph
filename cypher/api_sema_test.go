package cypher_test

// api_sema_test.go — engine-level tests for the sema → Engine.Run wiring
// (task-395). These exercise the pre-execution semantic-validation gate
// that surfaces *sema.SemanticError before plan execution begins, and the
// fast-path that skips planning when sema fails.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runQuery is a one-line helper that funnels Run(ctx, query, nil) for tests
// that only need to inspect the returned error.
func runQuery(t *testing.T, eng *cypher.Engine, query string) error {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if res != nil {
		// Drain any rows so the engine releases resources even when the
		// test expects a fail-fast error.
		for res.Next() {
		}
		_ = res.Close()
	}
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// UndefinedVariable: RETURN x  (x never introduced)
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_SemaUndefinedVariable verifies that a query referencing an
// undefined variable returns a *sema.SemanticError tagged
// SyntaxError.UndefinedVariable, BEFORE plan execution begins.
func TestEngine_SemaUndefinedVariable(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))

	err := runQuery(t, eng, "RETURN x")
	if err == nil {
		t.Fatal("expected sema error, got nil")
	}
	var se *sema.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("expected *sema.SemanticError, got %T: %v", err, err)
	}
	if se.Category != sema.CategorySyntaxError {
		t.Errorf("Category: got %q, want %q", se.Category, sema.CategorySyntaxError)
	}
	if se.SubType != sema.SubTypeUndefinedVariable {
		t.Errorf("SubType: got %q, want %q", se.SubType, sema.SubTypeUndefinedVariable)
	}
}

// TestEngine_SemaUndefinedVariableInWhere covers `MATCH (n) WHERE m.k = 1
// RETURN n` — the undefined variable appears in a WHERE expression rather
// than the projection list.
func TestEngine_SemaUndefinedVariableInWhere(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))

	err := runQuery(t, eng, "MATCH (n) WHERE m.k = 1 RETURN n")
	if err == nil {
		t.Fatal("expected sema error, got nil")
	}
	var se *sema.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("expected *sema.SemanticError, got %T: %v", err, err)
	}
	if se.SubType != sema.SubTypeUndefinedVariable {
		t.Errorf("SubType: got %q, want %q", se.SubType, sema.SubTypeUndefinedVariable)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// VariableTypeConflict: re-use a relationship variable as a node
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_SemaVariableTypeConflict_NodeFromRel covers the TCK Match1 §7
// pattern: a relationship variable is reused as a node in a later MATCH.
func TestEngine_SemaVariableTypeConflict_NodeFromRel(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))

	err := runQuery(t, eng, "MATCH (a)-[r]-(b) MATCH (r) RETURN r")
	if err == nil {
		t.Fatal("expected sema error, got nil")
	}
	var se *sema.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("expected *sema.SemanticError, got %T: %v", err, err)
	}
	if se.SubType != sema.SubTypeVariableTypeConflict {
		t.Errorf("SubType: got %q, want %q", se.SubType, sema.SubTypeVariableTypeConflict)
	}
}

// TestEngine_SemaVariableTypeConflict_RelFromNode covers the inverse: a
// node variable is reused as a relationship.
func TestEngine_SemaVariableTypeConflict_RelFromNode(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))

	err := runQuery(t, eng, "MATCH (r) MATCH (a)-[r]-(b) RETURN a")
	if err == nil {
		t.Fatal("expected sema error, got nil")
	}
	var se *sema.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("expected *sema.SemanticError, got %T: %v", err, err)
	}
	if se.SubType != sema.SubTypeVariableTypeConflict {
		t.Errorf("SubType: got %q, want %q", se.SubType, sema.SubTypeVariableTypeConflict)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fast-path: sema must short-circuit before plan build
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_SemaFastPath_SkipsPlanBuild verifies that the SemanticError
// produced by the analyser does NOT carry a "build plan" prefix — proving
// the engine returned before invoking buildPlanEngine. The build-plan
// pipeline always wraps its errors with "cypher: build plan:" so any
// occurrence of that string indicates the fast-path was bypassed.
func TestEngine_SemaFastPath_SkipsPlanBuild(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))

	err := runQuery(t, eng, "RETURN nonexistent")
	if err == nil {
		t.Fatal("expected sema error, got nil")
	}
	if strings.Contains(err.Error(), "build plan") {
		t.Errorf("fast-path violated: error mentions plan build: %v", err)
	}
	var se *sema.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("expected *sema.SemanticError, got %T: %v", err, err)
	}
	_ = se
}

// ─────────────────────────────────────────────────────────────────────────────
// Cache: repeated calls with a semantically-invalid query share the verdict
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_SemaCached verifies that the second invocation of a
// semantically-invalid query returns an error identical in type and code
// to the first — proving the sema verdict is cached alongside the plan.
func TestEngine_SemaCached(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	const query = "RETURN missing"

	first := runQuery(t, eng, query)
	second := runQuery(t, eng, query)

	if first == nil || second == nil {
		t.Fatalf("expected both calls to fail; first=%v second=%v", first, second)
	}

	var se1, se2 *sema.SemanticError
	if !errors.As(first, &se1) || !errors.As(second, &se2) {
		t.Fatalf("expected both errors to wrap *sema.SemanticError: first=%T second=%T", first, second)
	}
	if se1.Category != se2.Category || se1.SubType != se2.SubType {
		t.Errorf("cached verdict diverged: first=%s.%s second=%s.%s",
			se1.Category, se1.SubType, se2.Category, se2.SubType)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Regression guard: valid queries must not be flagged by sema
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_SemaCleanValidQueries runs a battery of valid queries
// covering the most regression-prone patterns (WITH *, ORDER BY alias,
// interleaved updating clauses, OPTIONAL MATCH + WHERE on WITH) and
// asserts that none of them produce a sema error.
func TestEngine_SemaCleanValidQueries(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"MatchReturn", "MATCH (n) RETURN n"},
		{"WithProject", "MATCH (n) WITH n RETURN n"},
		{"WithAlias", "MATCH (n) WITH n AS m RETURN m"},
		{"WithStarPreservesScope", "MATCH (n) WITH * RETURN n"},
		{"OrderByAlias", "MATCH (n) RETURN n.k AS k ORDER BY k DESC"},
		{"OrderByAliasSkipLimit", "MATCH (n) RETURN n.k AS k ORDER BY k DESC SKIP 0 LIMIT 5"},
		{"InterleavedUnwindCreate", "UNWIND [1,2,3] AS x CREATE (n {v: x}) RETURN n"},
		{
			"OptionalMatchPreWithWhere",
			"MATCH (a)-[:R]->(b)-->(c) OPTIONAL MATCH (a)-[r:R]->(c) WITH c WHERE r IS NULL RETURN c",
		},
		{"DoubleWith", "MATCH (n) WITH n WITH n RETURN n"},
		{
			"UpdatingBetweenWiths",
			"MATCH (n) WITH n, n.x AS x DELETE n WITH x WHERE x > 0 RETURN x",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
			err := runQuery(t, eng, tc.query)
			var se *sema.SemanticError
			if errors.As(err, &se) {
				t.Fatalf("valid query flagged by sema: %v", err)
			}
			// Non-sema errors (e.g. unsupported IR) are acceptable for this
			// regression guard; only sema false-positives must fail it.
		})
	}
}

// TestEngine_SemaRedeclarationPathConflict covers re-using a node
// variable as a path-pattern path variable.
func TestEngine_SemaRedeclarationPathConflict(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))

	err := runQuery(t, eng, "MATCH (p) MATCH p = (a)-[r]-(b) RETURN p")
	if err == nil {
		t.Fatal("expected sema error, got nil")
	}
	var se *sema.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("expected *sema.SemanticError, got %T: %v", err, err)
	}
	if se.SubType != sema.SubTypeVariableTypeConflict {
		t.Errorf("SubType: got %q, want %q", se.SubType, sema.SubTypeVariableTypeConflict)
	}
}

// TestEngine_SemaErrorMessagePrefix verifies the user-facing format of
// the engine's sema error so external callers (Bolt server, REPL) can
// parse the (Category, SubType) pair from the string.
func TestEngine_SemaErrorMessagePrefix(t *testing.T) {
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	err := runQuery(t, eng, "RETURN ghost")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	const wantPrefix = "cypher: SyntaxError.UndefinedVariable:"
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("error message: got %q, want prefix %q", err.Error(), wantPrefix)
	}
}
