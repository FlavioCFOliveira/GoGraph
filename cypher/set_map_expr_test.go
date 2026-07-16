package cypher_test

// set_map_expr_test.go — regression coverage for whole-entity SET whose RHS is a
// map-valued expression that is neither a `{…}` literal, a bound entity
// variable resolving to a node/relationship, nor a `$param`.
//
// Two fail-classes are pinned here for the standalone SET forms (routed through
// [exec.SetAllProperties]):
//
//   - #2030: a bound MAP variable (e.g. `UNWIND [{x:'V'}] AS r … SET n = r`)
//     used to route to the entity-copy path, whose resolve error on a non-entity
//     value was swallowed as a no-op — the target silently kept null/stale data
//     (a fail-silent Consistency defect). It must now write the map's entries.
//
// The MERGE ON CREATE / ON MATCH whole-entity forms use a separate mechanism
// (parseMergeActions) and are covered by their own task/tests. The map-returning
// function form (`SET n = properties(m)`, #2027) is covered in its own file.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// runDrainErr runs a write query, drains it, and returns the first error from
// either the call or the drained result (writes are applied lazily).
func runDrainErr(t *testing.T, eng *cypher.Engine, query string) error {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), query, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	defer res.Close()
	return res.Err()
}

// TestSetMapExpr_Var_Merge_Node: `SET n += <mapvar>` writes the map's entries
// and keeps the target's other properties.
func TestSetMapExpr_Var_Merge_Node(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	drainRunInTx(t, eng, `UNWIND [{x:'V'}] AS r MATCH (n:N {id:1}) SET n += r`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	if setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.id AS v`) {
		t.Fatal("n.id must be kept by += merge")
	}
}

// TestSetMapExpr_Var_Replace_Node: `SET n = <mapvar>` writes the map's entries
// and clears the target's other properties.
func TestSetMapExpr_Var_Replace_Node(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1, keep:'old'})`)
	drainRunInTx(t, eng, `UNWIND [{x:'V'}] AS r MATCH (n:N {id:1}) SET n = r`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.keep AS v`) {
		t.Fatal("n.keep must be cleared by = replace")
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.id AS v`) {
		t.Fatal("n.id must be cleared by = replace")
	}
}

// TestSetMapExpr_Var_Merge_Rel / _Replace_Rel: the relationship shapes.
func TestSetMapExpr_Var_Merge_Rel(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {id:1})-[:R {keep:'old'}]->(:B {id:2})`)
	drainRunInTx(t, eng, `UNWIND [{x:'RV'}] AS row MATCH (:A)-[rel:R]->(:B) SET rel += row`)
	if got := setScalarString(t, eng, `MATCH ()-[rel:R]->() RETURN rel.x AS v`); got != "RV" {
		t.Fatalf("rel.x = %q, want \"RV\"", got)
	}
	if setScalarIsNull(t, eng, `MATCH ()-[rel:R]->() RETURN rel.keep AS v`) {
		t.Fatal("rel.keep must be kept by += merge")
	}
}

func TestSetMapExpr_Var_Replace_Rel(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {id:1})-[:R {keep:'old'}]->(:B {id:2})`)
	drainRunInTx(t, eng, `UNWIND [{x:'RV'}] AS row MATCH (:A)-[rel:R]->(:B) SET rel = row`)
	if got := setScalarString(t, eng, `MATCH ()-[rel:R]->() RETURN rel.x AS v`); got != "RV" {
		t.Fatalf("rel.x = %q, want \"RV\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH ()-[rel:R]->() RETURN rel.keep AS v`) {
		t.Fatal("rel.keep must be cleared by = replace")
	}
}

// TestSetMapExpr_Var_CreateThenSet: `CREATE (n) SET n = <mapvar>`.
func TestSetMapExpr_Var_CreateThenSet(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `UNWIND [{x:'V'}] AS r CREATE (n:N {id:1}) SET n = r`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.id AS v`) {
		t.Fatal("n.id must be cleared by = replace")
	}
}

// TestSetMapExpr_Var_NullKeyRemoved: a null-valued entry in the map removes the
// key from the target (openCypher SET-map null semantics), on the += form.
func TestSetMapExpr_Var_NullKeyRemoved(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1, x:'HAD'})`)
	drainRunInTx(t, eng, `UNWIND [{x: null, y:'K'}] AS r MATCH (n:N {id:1}) SET n += r`)
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.x AS v`) {
		t.Fatal("n.x must be removed when its map value is null")
	}
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.y AS v`); got != "K" {
		t.Fatalf("n.y = %q, want \"K\"", got)
	}
}

// TestSetMapExpr_NodeCopy_Unaffected guards that `SET n = m` where m is a bound
// NODE still copies the node's properties (entity-copy path, not the map path).
func TestSetMapExpr_NodeCopy_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1}), (:M {x:'V', y:'W'})`)
	drainRunInTx(t, eng, `MATCH (n:N),(m:M) SET n = m`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\" (node-copy)", got)
	}
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.y AS v`); got != "W" {
		t.Fatalf("n.y = %q, want \"W\" (node-copy)", got)
	}
}

// TestSetMapExpr_ParamMap_Unaffected guards the `$param` whole-map form.
func TestSetMapExpr_ParamMap_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	if _, err := eng.RunInTxAny(context.Background(),
		`MATCH (n:N) SET n += $p`, map[string]any{"p": map[string]any{"x": "V"}}); err != nil {
		t.Fatalf("param SET: %v", err)
	}
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\" (param map)", got)
	}
}

// TestSetMapExpr_MapLiteral_Unaffected guards the `{…}`-literal-with-row-value
// form (the #2026 capability) against regression.
func TestSetMapExpr_MapLiteral_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	drainRunInTx(t, eng, `UNWIND [{y:'V'}] AS r MATCH (n:N) SET n += {x: r.y}`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\" (map literal)", got)
	}
}

// TestSetMapExpr_ScalarVar_TypeError: `SET n = <scalar-valued expression>` must
// fail rather than silently no-op or corrupt the target. A map-projection-free
// scalar function on the default (FromExpr) path yields a runtime TypeError
// (never a SyntaxError — openCypher grammar permits any Expression on the RHS).
func TestSetMapExpr_ScalarVar_TypeError(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	// The write is applied lazily; the TypeError surfaces on drain (res.Err())
	// or from the call — accept either.
	err := runDrainErr(t, eng, `MATCH (n:N) SET n = size([1,2,3])`)
	if err == nil {
		t.Fatal("SET n = <scalar> must be a TypeError, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "TypeError") {
		t.Fatalf("error %q should be a TypeError", err.Error())
	}
	if strings.Contains(err.Error(), "SyntaxError") {
		t.Fatalf("error %q must not be a SyntaxError (RHS shape is a runtime type error)", err.Error())
	}
}
