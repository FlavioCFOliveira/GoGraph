package cypher_test

// merge_on_action_expr_test.go — #1965 regression coverage.
//
// MERGE's ON CREATE / ON MATCH SET actions used to parse each right-hand side
// as a literal and silently drop (node-only Merge / MergePattern) or error
// (MergeRelationship) on any non-literal expression. A self-referential
// assignment such as `ON MATCH SET n.num = n.num + 1` therefore committed
// without changing the value — a fail-silent Consistency defect. These tests
// pin the per-row expression evaluation across all three MERGE operator paths
// (lone node, both-bound relationship, and compound pattern), that constant
// RHS still works, and that regular (non-MERGE) SET is unaffected.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mergeExprScalar runs query and returns the value of column col in the first
// row (nil when there are no rows).
func mergeExprScalar(t *testing.T, eng *cypher.Engine, query, col string) any {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	rows := drainRecords(t, res)
	if len(rows) == 0 {
		return nil
	}
	return rows[0][col]
}

// TestMerge_OnMatchSet_SelfReferentialExpr_Node pins the node-only Merge path:
// `ON MATCH SET n.num = n.num + 1` must read the current value and increment it
// on every re-merge (was fail-silent, #1965).
func TestMerge_OnMatchSet_SelfReferentialExpr_Node(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	const q = `MERGE (n:Person {name:'x'}) ON CREATE SET n.num = 1 ON MATCH SET n.num = n.num + 1`

	// First run creates the node → ON CREATE SET n.num = 1.
	drainRunInTx(t, eng, q)
	if got := fmtAny(mergeExprScalar(t, eng, `MATCH (n:Person {name:'x'}) RETURN n.num AS num`, "num")); got != "1" {
		t.Fatalf("after ON CREATE: n.num = %s, want 1", got)
	}

	// Each subsequent run matches → ON MATCH SET n.num = n.num + 1.
	for want := int64(2); want <= 4; want++ {
		drainRunInTx(t, eng, q)
		got := fmtAny(mergeExprScalar(t, eng, `MATCH (n:Person {name:'x'}) RETURN n.num AS num`, "num"))
		if got != fmtAny(want) {
			t.Fatalf("after re-merge #%d: n.num = %s, want %d", want, got, want)
		}
	}

	// Exactly one node throughout (MERGE stayed idempotent).
	assertCount(context.Background(), t, eng, `MATCH (n:Person) RETURN count(n) AS n`, 1)
}

// TestMerge_OnActions_SelfReferentialExpr_Relationship pins the both-endpoints-
// bound MergeRelationship fast path: `ON CREATE SET r.n = 1` then
// `ON MATCH SET r.n = r.n + 1` must increment the edge property while keeping a
// single edge (was a literal-parse error, #1965).
func TestMerge_OnActions_SelfReferentialExpr_Relationship(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (a:A {id:1}), (b:B {id:2})`)

	const q = `MATCH (a:A {id:1}), (b:B {id:2})
		MERGE (a)-[r:KNOWS]->(b)
		ON CREATE SET r.n = 1
		ON MATCH SET r.n = r.n + 1`

	drainRunInTx(t, eng, q)
	if got := fmtAny(mergeExprScalar(t, eng, `MATCH ()-[r:KNOWS]->() RETURN r.n AS n`, "n")); got != "1" {
		t.Fatalf("after ON CREATE: r.n = %s, want 1", got)
	}
	assertCount(context.Background(), t, eng, `MATCH ()-[r:KNOWS]->() RETURN count(r) AS n`, 1)

	for want := int64(2); want <= 4; want++ {
		drainRunInTx(t, eng, q)
		got := fmtAny(mergeExprScalar(t, eng, `MATCH ()-[r:KNOWS]->() RETURN r.n AS n`, "n"))
		if got != fmtAny(want) {
			t.Fatalf("after re-merge #%d: r.n = %s, want %d", want, got, want)
		}
		// Still a single edge — MERGE matched the existing one.
		assertCount(context.Background(), t, eng, `MATCH ()-[r:KNOWS]->() RETURN count(r) AS n`, 1)
	}
}

// TestMerge_OnActions_SelfReferentialExpr_Pattern pins the compound
// MergePattern path (fresh endpoints): a relationship-targeted ON MATCH
// expression must increment the edge property and a node-targeted ON MATCH
// expression must increment a node property, both without duplicating the
// pattern (#1965).
func TestMerge_OnActions_SelfReferentialExpr_Pattern(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	const q = `MERGE (a:A {id:1})-[r:R]->(b:B {id:2})
		ON CREATE SET r.n = 1, a.cnt = 1
		ON MATCH SET r.n = r.n + 1, a.cnt = a.cnt + 1`

	drainRunInTx(t, eng, q)
	if got := fmtAny(mergeExprScalar(t, eng, `MATCH ()-[r:R]->() RETURN r.n AS n`, "n")); got != "1" {
		t.Fatalf("after ON CREATE: r.n = %s, want 1", got)
	}
	if got := fmtAny(mergeExprScalar(t, eng, `MATCH (a:A {id:1}) RETURN a.cnt AS cnt`, "cnt")); got != "1" {
		t.Fatalf("after ON CREATE: a.cnt = %s, want 1", got)
	}

	for want := int64(2); want <= 4; want++ {
		drainRunInTx(t, eng, q)
		if got := fmtAny(mergeExprScalar(t, eng, `MATCH ()-[r:R]->() RETURN r.n AS n`, "n")); got != fmtAny(want) {
			t.Fatalf("after re-merge #%d: r.n = %s, want %d", want, got, want)
		}
		if got := fmtAny(mergeExprScalar(t, eng, `MATCH (a:A {id:1}) RETURN a.cnt AS cnt`, "cnt")); got != fmtAny(want) {
			t.Fatalf("after re-merge #%d: a.cnt = %s, want %d", want, got, want)
		}
	}

	// The pattern was matched, not recreated: one edge, one A, one B.
	assertCount(context.Background(), t, eng, `MATCH (a:A) RETURN count(a) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH (b:B) RETURN count(b) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS n`, 1)
}

// TestMerge_OnMatchSet_ConstantRHS_StillWorks pins that a constant (literal)
// ON MATCH RHS keeps taking the literal fast path unchanged.
func TestMerge_OnMatchSet_ConstantRHS_StillWorks(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Person {name:'y'})`)
	drainRunInTx(t, eng, `MERGE (n:Person {name:'y'}) ON MATCH SET n.num = 42`)
	if got := fmtAny(mergeExprScalar(t, eng, `MATCH (n:Person {name:'y'}) RETURN n.num AS num`, "num")); got != "42" {
		t.Fatalf("constant ON MATCH RHS: n.num = %s, want 42", got)
	}
}

// TestMerge_OnActions_CrossVariableExpr pins that an ON MATCH RHS referencing
// another bound variable resolves against the driving row (not only self-
// reference) — the general expression path, exercised on the node Merge.
func TestMerge_OnActions_CrossVariableExpr(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (s:Source {v: 7})`)
	drainRunInTx(t, eng, `CREATE (n:Person {name:'z'})`)
	drainRunInTx(t, eng,
		`MATCH (s:Source) MERGE (n:Person {name:'z'}) ON MATCH SET n.num = s.v + 1`)
	if got := fmtAny(mergeExprScalar(t, eng, `MATCH (n:Person {name:'z'}) RETURN n.num AS num`, "num")); got != "8" {
		t.Fatalf("cross-variable ON MATCH RHS: n.num = %s, want 8", got)
	}
}

// TestMerge_OnMatchSet_ExprToNull_RemovesProperty pins openCypher SET-to-null
// semantics on the expression path: an ON MATCH RHS that evaluates to null
// removes the property.
func TestMerge_OnMatchSet_ExprToNull_RemovesProperty(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Person {name:'k', num: 5})`)
	// Precondition: n.num is present and non-null.
	assertCount(context.Background(), t, eng,
		`MATCH (n:Person {name:'k'}) WHERE n.num IS NOT NULL RETURN count(n) AS n`, 1)
	// n.missing is unset → n.missing + 1 evaluates to null → removes n.num.
	drainRunInTx(t, eng,
		`MERGE (n:Person {name:'k'}) ON MATCH SET n.num = n.missing + 1`)
	// The property is now absent (reads back as null).
	assertCount(context.Background(), t, eng,
		`MATCH (n:Person {name:'k'}) WHERE n.num IS NULL RETURN count(n) AS n`, 1)
}

// TestRegularSet_SelfReferentialExpr_Unaffected pins that ordinary (non-MERGE)
// SET still evaluates a self-referential expression RHS — a control confirming
// the shared SET machinery is untouched.
func TestRegularSet_SelfReferentialExpr_Unaffected(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Counter {v: 10})`)
	drainRunInTx(t, eng, `MATCH (n:Counter) SET n.v = n.v + 5`)
	if got := fmtAny(mergeExprScalar(t, eng, `MATCH (n:Counter) RETURN n.v AS v`, "v")); got != "15" {
		t.Fatalf("regular SET self-referential RHS: n.v = %s, want 15", got)
	}
}
