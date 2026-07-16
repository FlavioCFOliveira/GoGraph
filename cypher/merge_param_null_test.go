package cypher_test

// merge_param_null_test.go — regression tests for the production-readiness
// audit finding [CY2] (rmp #2035).
//
// openCypher forbids merging on a null property value (it can never match its
// own write; TCK Merge1[17]/Merge5[29] -> SemanticError.MergeReadOwnWrites).
// Literal-null (MERGE (n {p: null})) and runtime-expression-null
// (WITH null AS x MERGE (n {p: x})) were both rejected, but a PARAMETER
// resolving to null slipped through: it was resolved at build time via
// WithParams, which omitted the null entry (CREATE no-op semantics), so the
// MERGE ran with the key silently dropped and no error. These tests pin the
// third arrival route.

import (
	"strings"
	"testing"
)

// mergeParamNull runs a MERGE whose property comes from a null parameter and
// returns the resulting error (RunInTx error or drain error). Endpoints for the
// relationship shapes are seeded first.
func mergeParamNull(t *testing.T, q string, params map[string]any) error {
	t.Helper()
	eng, ctx := newPlainEngine(t)
	writeMust(t, eng, `CREATE (:A), (:B)`)
	res, err := eng.RunInTxAny(ctx, q, params)
	if err != nil {
		return err
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
	}
	return res.Err()
}

func mustBeMergeReadOwnWrites(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected SemanticError.MergeReadOwnWrites, got nil (the null param was silently dropped)")
	}
	if !strings.Contains(err.Error(), "MergeReadOwnWrites") {
		t.Fatalf("expected MergeReadOwnWrites, got: %v", err)
	}
}

func TestMergeParamNull_NodeSingleProp(t *testing.T) {
	t.Parallel()
	mustBeMergeReadOwnWrites(t, mergeParamNull(t, `MERGE (n:N {p: $x})`, map[string]any{"x": nil}))
}

func TestMergeParamNull_NodeMixedProps(t *testing.T) {
	t.Parallel()
	mustBeMergeReadOwnWrites(t, mergeParamNull(t, `MERGE (n:N {a: 1, p: $x})`, map[string]any{"x": nil}))
}

func TestMergeParamNull_Relationship(t *testing.T) {
	t.Parallel()
	mustBeMergeReadOwnWrites(t, mergeParamNull(t,
		`MATCH (a:A), (b:B) MERGE (a)-[r:R {p: $x}]->(b)`, map[string]any{"x": nil}))
}

// Control: a non-null parameter must still work (no false positive).
func TestMergeParamNull_NonNullParamStillWorks(t *testing.T) {
	t.Parallel()
	if err := mergeParamNull(t, `MERGE (n:N {p: $x})`, map[string]any{"x": "v"}); err != nil {
		t.Fatalf("non-null param MERGE must succeed, got: %v", err)
	}
}
