package cypher_test

// set_with_whole_entity_regression_test.go — rmp #2010
//
// Regression coverage for a parser ATN-simulation panic on a valid openCypher
// query. Before the fix, the whole-entity SET forms
//
//	MATCH (n) WITH n SET n = {a: 1}
//	MATCH (n) WITH n SET n = $p
//
// panicked during ANTLR ATN simulation with
//
//	interface conversion: antlr.Transition is *antlr.EpsilonTransition,
//	not *antlr.RuleTransition
//
// (recovered into a parse error, so the valid query was wrongly rejected).
// The `setItem` grammar alternative `propertyExpression ASSIGN expression`
// let a bare `symbol` match both the property-set and the whole-entity forms,
// forcing ANTLR into full-context prediction that trips a runtime bug in the
// pinned antlr4-go v4.13.1 — but only inside the deeper multiPartQ context a
// preceding WITH creates. The `+=` (append) form and the no-WITH form were
// unaffected. The fix requires alternative 1 to carry at least one `.name`
// accessor (`atom (DOT name)+ ASSIGN expression`), removing the ambiguity.
//
// These tests assert both that the queries parse and execute AND that the
// whole-entity replace semantics hold (absent keys are cleared), so a future
// regression in either the grammar or the visitor is caught.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// isCypherNull reports whether a projected record value is Cypher null — a
// cleared property reads back as an expr null sentinel, not Go nil.
func isCypherNull(v any) bool {
	if v == nil {
		return true
	}
	ev, ok := v.(expr.Value)
	return ok && expr.IsNull(ev)
}

// seedNodeXY creates a single (:N {x:1, y:2}) node.
func seedNodeXY(ctx context.Context, t *testing.T, eng *cypher.Engine) {
	t.Helper()
	res, err := eng.RunInTx(ctx, `CREATE (n:N {x: 1, y: 2})`, nil)
	if err != nil {
		t.Fatalf("seed CREATE: %v", err)
	}
	drainResult(t, res)
}

// TestWithSetWholeEntityReplace_MapLiteral covers the previously-panicking
// `WITH n SET n = {map}` shape and asserts whole-entity replace semantics:
// the map literal replaces every property, so the pre-existing x and y are
// cleared and only a remains.
func TestWithSetWholeEntityReplace_MapLiteral(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	seedNodeXY(ctx, t, eng)

	res, err := eng.RunInTx(ctx,
		`MATCH (n:N) WITH n SET n = {a: 1} RETURN n.a AS a, n.x AS x, n.y AS y`, nil)
	if err != nil {
		t.Fatalf("WITH n SET n = {map}: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	if isCypherNull(rows[0]["a"]) {
		t.Errorf("new key a must be set, got null")
	}
	if !isCypherNull(rows[0]["x"]) {
		t.Errorf("whole-entity replace must clear x, got %v", rows[0]["x"])
	}
	if !isCypherNull(rows[0]["y"]) {
		t.Errorf("whole-entity replace must clear y, got %v", rows[0]["y"])
	}
}

// TestWithSetWholeEntityReplace_Param covers the previously-panicking
// `WITH n SET n = $p` shape, with the parameter resolving to a map.
func TestWithSetWholeEntityReplace_Param(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	seedNodeXY(ctx, t, eng)

	params := map[string]any{"p": map[string]any{"a": int64(1)}}
	res, err := eng.RunInTxAny(ctx,
		`MATCH (n:N) WITH n SET n = $p RETURN n.a AS a, n.x AS x`, params)
	if err != nil {
		t.Fatalf("WITH n SET n = $p: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	if isCypherNull(rows[0]["a"]) {
		t.Errorf("new key a must be set, got null")
	}
	if !isCypherNull(rows[0]["x"]) {
		t.Errorf("whole-entity replace must clear x, got %v", rows[0]["x"])
	}
}

// TestWithSetWholeEntityReplace_DeepContext exercises the deeper multiPartQ
// context (WITH … WHERE … SET … RETURN) that is required to reach the ATN
// state the bug lived in.
func TestWithSetWholeEntityReplace_DeepContext(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res0, err := eng.RunInTx(ctx, `CREATE (n:N {a: 0, x: 1})`, nil)
	if err != nil {
		t.Fatalf("seed CREATE: %v", err)
	}
	drainResult(t, res0)

	res, err := eng.RunInTx(ctx,
		`MATCH (n:N) WITH n WHERE n.a = 0 SET n = {b: 2} RETURN n.b AS b, n.a AS a`, nil)
	if err != nil {
		t.Fatalf("WITH n WHERE … SET n = {map} RETURN: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	if isCypherNull(rows[0]["b"]) {
		t.Errorf("new key b must be set, got null")
	}
	if !isCypherNull(rows[0]["a"]) {
		t.Errorf("whole-entity replace must clear a, got %v", rows[0]["a"])
	}
}

// TestWithSetAppend_KeepsExisting confirms the append form `WITH n SET n += …`
// (which never panicked) still merges rather than replaces, guarding against
// the fix accidentally routing `+=` through the replace path.
func TestWithSetAppend_KeepsExisting(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	seedNodeXY(ctx, t, eng)

	res, err := eng.RunInTx(ctx,
		`MATCH (n:N) WITH n SET n += {a: 1} RETURN n.a AS a, n.x AS x, n.y AS y`, nil)
	if err != nil {
		t.Fatalf("WITH n SET n += {map}: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	if isCypherNull(rows[0]["a"]) {
		t.Errorf("appended key a must be set, got null")
	}
	if isCypherNull(rows[0]["x"]) || isCypherNull(rows[0]["y"]) {
		t.Errorf("append must keep existing x and y, got x=%v y=%v", rows[0]["x"], rows[0]["y"])
	}
}

// TestWithSetProperty_StillWorks confirms the property-set form `WITH n SET
// n.p = expr` (the other setItem alternative) is unaffected by the grammar
// change and still targets a single property.
func TestWithSetProperty_StillWorks(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	seedNodeXY(ctx, t, eng)

	res, err := eng.RunInTx(ctx,
		`MATCH (n:N) WITH n SET n.z = 3 RETURN n.z AS z, n.x AS x`, nil)
	if err != nil {
		t.Fatalf("WITH n SET n.z = 3: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	if isCypherNull(rows[0]["z"]) {
		t.Errorf("property z must be set, got null")
	}
	if isCypherNull(rows[0]["x"]) {
		t.Errorf("property set must not clear x, got null")
	}
}
