package cypher_test

// set_map_nonliteral_test.go — regression coverage for row-driven whole-entity
// SET maps ([exec.SetAllProperties]).
//
// `SET n = {…}` / `SET n += {…}` used to parse the source map as literals only:
// a non-literal map value (a variable reference, property access, or arithmetic
// — e.g. `SET n += {x: row.y}`) was silently dropped, so the target kept null
// or stale data while the statement reported success (a fail-silent Consistency
// defect, the same class as the MERGE inline-relationship-property bug). The
// single-property form `SET n.x = row.y` already evaluated per row; only the
// whole-entity map form was affected. These tests pin that the evaluated value
// is written (and null-valued keys removed) across the node/relationship and
// merge/replace shapes, that literals and $param maps still work, and that a
// self-reference resolves against the pre-SET state.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func setMapEng(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return cypher.NewEngine(g)
}

// setScalarString runs query and returns the string value of column v in the
// first row, failing when the column is absent or not a string.
func setScalarString(t *testing.T, eng *cypher.Engine, query string) string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer res.Close()
	if !res.Next() {
		t.Fatalf("query %q: no rows", query)
	}
	v := res.Record()["v"]
	sv, ok := v.(expr.StringValue)
	if !ok {
		t.Fatalf("query %q: v = %v (%T), want expr.StringValue", query, v, v)
	}
	return string(sv)
}

// setScalarIsNull reports whether column v of the first row is null.
func setScalarIsNull(t *testing.T, eng *cypher.Engine, query string) bool {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer res.Close()
	if !res.Next() {
		t.Fatalf("query %q: no rows", query)
	}
	v := res.Record()["v"]
	if v == nil {
		return true
	}
	ev, ok := v.(expr.Value)
	return ok && expr.IsNull(ev)
}

// TestSet_MapMerge_NonLiteral_Node pins the reported class on the node merge
// form: `SET n += {x: r.y}` must write the evaluated value, not null.
func TestSet_MapMerge_NonLiteral_Node(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	drainRunInTx(t, eng, `UNWIND [{y:'DYN'}] AS r MATCH (n:N {id:1}) SET n += {x: r.y}`)
	if got := setScalarString(t, eng, `MATCH (n:N {id:1}) RETURN n.x AS v`); got != "DYN" {
		t.Fatalf("n.x = %q, want \"DYN\"", got)
	}
}

// TestSet_MapReplace_NonLiteral_Node pins the node replace form: `SET n = {x: r.y}`
// writes the evaluated value and clears the other properties.
func TestSet_MapReplace_NonLiteral_Node(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1, keep:'old'})`)
	drainRunInTx(t, eng, `UNWIND [{y:'REP'}] AS r MATCH (n:N {id:1}) SET n = {x: r.y}`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "REP" {
		t.Fatalf("n.x = %q, want \"REP\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.keep AS v`) {
		t.Fatal("n.keep should be cleared by SET n = {…} replace")
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.id AS v`) {
		t.Fatal("n.id should be cleared by SET n = {…} replace")
	}
}

// TestSet_MapMerge_NonLiteral_Relationship pins the relationship merge form.
func TestSet_MapMerge_NonLiteral_Relationship(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {id:1})-[:R]->(:B {id:2})`)
	drainRunInTx(t, eng, `UNWIND [{y:'RDYN'}] AS row MATCH (:A)-[r:R]->(:B) SET r += {x: row.y}`)
	if got := setScalarString(t, eng, `MATCH ()-[r:R]->() RETURN r.x AS v`); got != "RDYN" {
		t.Fatalf("r.x = %q, want \"RDYN\"", got)
	}
}

// TestSet_MapReplace_NonLiteral_Relationship pins the relationship replace form.
func TestSet_MapReplace_NonLiteral_Relationship(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {id:1})-[:R {keep:'old'}]->(:B {id:2})`)
	drainRunInTx(t, eng, `UNWIND [{y:'RREP'}] AS row MATCH (:A)-[r:R]->(:B) SET r = {x: row.y}`)
	if got := setScalarString(t, eng, `MATCH ()-[r:R]->() RETURN r.x AS v`); got != "RREP" {
		t.Fatalf("r.x = %q, want \"RREP\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH ()-[r:R]->() RETURN r.keep AS v`) {
		t.Fatal("r.keep should be cleared by SET r = {…} replace")
	}
}

// TestSet_MapMerge_Mixed pins a map mixing a literal and a non-literal value —
// both must be written (the literal must not be lost when the evaluator is
// installed, and vice versa).
func TestSet_MapMerge_Mixed(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	drainRunInTx(t, eng, `UNWIND [{y:'D'}] AS r MATCH (n:N {id:1}) SET n += {lit: 'L', dyn: r.y}`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.lit AS v`); got != "L" {
		t.Fatalf("n.lit = %q, want \"L\"", got)
	}
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.dyn AS v`); got != "D" {
		t.Fatalf("n.dyn = %q, want \"D\"", got)
	}
}

// TestSet_MapMerge_SelfReferenceArithmetic pins that a self-reference in the map
// resolves against the pre-SET node state: `SET n += {x: n.id + 100}`.
func TestSet_MapMerge_SelfReferenceArithmetic(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:5})`)
	drainRunInTx(t, eng, `MATCH (n:N) SET n += {x: n.id + 100}`)
	res, err := eng.Run(context.Background(), `MATCH (n:N) RETURN n.x AS v`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()
	if !res.Next() {
		t.Fatal("no rows")
	}
	if got := res.Record()["v"]; got != expr.IntegerValue(105) {
		t.Fatalf("n.x = %v (%T), want IntegerValue(105)", got, got)
	}
}

// TestSet_MapMerge_DynamicNullRemovesKey pins openCypher SET-map null semantics
// for a non-literal value that evaluates to null: the key is removed, not left
// stale. `SET n += {x: r.missing}` (r.missing → null) removes an existing x.
func TestSet_MapMerge_DynamicNullRemovesKey(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1, x:'HAD'})`)
	drainRunInTx(t, eng, `UNWIND [{m:1}] AS r MATCH (n:N {id:1}) SET n += {x: r.missing}`)
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.x AS v`) {
		t.Fatal("n.x should be removed when its SET-map value evaluates to null")
	}
}

// TestSet_MapMerge_LiteralAndParam_Unaffected guards that the all-literal and
// $param whole-map forms (which already worked) are not regressed.
func TestSet_MapMerge_LiteralAndParam_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	drainRunInTx(t, eng, `MATCH (n:N) SET n += {x: 'LIT'}`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "LIT" {
		t.Fatalf("literal SET: n.x = %q, want \"LIT\"", got)
	}
	if _, err := eng.RunInTxAny(context.Background(),
		`MATCH (n:N {id:1}) SET n += $p`, map[string]any{"p": map[string]any{"y": "PARAM"}}); err != nil {
		t.Fatalf("param SET: %v", err)
	}
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.y AS v`); got != "PARAM" {
		t.Fatalf("param SET: n.y = %q, want \"PARAM\"", got)
	}
}
