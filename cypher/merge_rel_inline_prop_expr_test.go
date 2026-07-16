package cypher_test

// merge_rel_inline_prop_expr_test.go — regression coverage for row-driven inline
// relationship property maps on the both-endpoints-bound MERGE fast path
// ([exec.MergeRelationship]).
//
// A MERGE such as
//
//	UNWIND [{f:'a', t:'b', pk:'normal'}] AS r
//	MATCH (x:Repro {k:r.f}), (y:Repro {k:r.t})
//	MERGE (x)-[:E {kind: r.pk}]->(y)
//
// used to parse the inline relationship property map as literals only. The
// non-literal value `r.pk` was silently dropped, so the created edge stored
// null for `kind` — the statement reported success while losing the write (a
// fail-silent Consistency defect). The same held for a $param value
// (`{kind: $pk}`), which the literal-only parser also skips. These tests pin
// that the evaluated value is (1) written on the created edge and (2) used as
// the existing-edge search predicate, across the create, match-idempotent,
// match-discriminating (multigraph), parameter, mixed-map, and undirected
// shapes.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// relScalarString runs query and returns the string value of column col in the
// first row, failing the test when the column is absent or not a string.
func relScalarString(t *testing.T, eng *cypher.Engine, query string) string {
	t.Helper()
	v := mergeExprScalar(t, eng, query, "v")
	if v == nil {
		t.Fatalf("query %q: column v missing / no rows", query)
	}
	sv, ok := v.(expr.StringValue)
	if !ok {
		t.Fatalf("query %q: column v = %v (%T), want expr.StringValue", query, v, v)
	}
	return string(sv)
}

// TestMerge_Rel_InlineProp_NonLiteral_Create is the reported repro: a row-driven
// (`{kind: r.pk}`) inline relationship property must be written on the created
// edge, not silently dropped to null.
func TestMerge_Rel_InlineProp_NonLiteral_Create(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'}), (:Repro {k:'b'})`)
	drainRunInTx(t, eng, `UNWIND [{f:'a', t:'b', pk:'normal'}] AS r
		MATCH (x:Repro {k:r.f}), (y:Repro {k:r.t})
		MERGE (x)-[:E {kind: r.pk}]->(y)`)

	if got := relScalarString(t, eng,
		`MATCH (:Repro {k:'a'})-[e:E]->(:Repro {k:'b'}) RETURN e.kind AS v`); got != "normal" {
		t.Fatalf("e.kind = %q, want \"normal\"", got)
	}
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 1)
}

// TestMerge_Rel_InlineProp_NonLiteral_DrivesSearch_Idempotent verifies the
// evaluated property is also used as the MATCH predicate: re-running the same
// MERGE finds the edge it created rather than creating a duplicate.
func TestMerge_Rel_InlineProp_NonLiteral_DrivesSearch_Idempotent(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'}), (:Repro {k:'b'})`)
	const q = `UNWIND [{f:'a', t:'b', pk:'normal'}] AS r
		MATCH (x:Repro {k:r.f}), (y:Repro {k:r.t})
		MERGE (x)-[:E {kind: r.pk}]->(y)`

	drainRunInTx(t, eng, q)
	drainRunInTx(t, eng, q) // second run must MATCH the first, not create a duplicate.

	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 1)
	if got := relScalarString(t, eng,
		`MATCH (:Repro {k:'a'})-[e:E]->(:Repro {k:'b'}) RETURN e.kind AS v`); got != "normal" {
		t.Fatalf("e.kind = %q, want \"normal\"", got)
	}
}

// TestMerge_Rel_InlineProp_NonLiteral_DiscriminatesMatch_Multigraph verifies the
// evaluated property discriminates the search predicate: against a single
// existing edge, a MERGE whose evaluated inline property equals it binds to it
// (no new edge), while a MERGE whose evaluated property differs does not match
// and creates a distinct parallel edge. This mirrors the literal-property fast
// path exactly — the non-literal value is used as the match predicate, not just
// written on creation.
//
// (Discriminating among *two or more* already-parallel edges by property is a
// separate, pre-existing MergeRelationship limitation shared by the literal
// path — the per-pair property view cannot tell parallel edges apart — and is
// deliberately out of scope here.)
func TestMerge_Rel_InlineProp_NonLiteral_DiscriminatesMatch_Multigraph(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'}), (:Repro {k:'b'})`)

	// First value 'one' → creates the first edge.
	drainRunInTx(t, eng, `UNWIND [{pk:'one'}] AS r
		MATCH (x:Repro {k:'a'}), (y:Repro {k:'b'})
		MERGE (x)-[:E {kind: r.pk}]->(y)`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 1)

	// Re-asserting 'one' matches the existing {kind:'one'} edge → no new edge.
	drainRunInTx(t, eng, `UNWIND [{pk:'one'}] AS r
		MATCH (x:Repro {k:'a'}), (y:Repro {k:'b'})
		MERGE (x)-[:E {kind: r.pk}]->(y)`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 1)

	// A distinct value 'two' does NOT match the {kind:'one'} edge → a parallel
	// edge is created.
	drainRunInTx(t, eng, `UNWIND [{pk:'two'}] AS r
		MATCH (x:Repro {k:'a'}), (y:Repro {k:'b'})
		MERGE (x)-[:E {kind: r.pk}]->(y)`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 2)
}

// TestMerge_Rel_InlineProp_Param pins the $param variant of the inline
// relationship property map, which the literal-only parser also dropped.
func TestMerge_Rel_InlineProp_Param(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'}), (:Repro {k:'b'})`)
	if _, err := eng.RunInTxAny(context.Background(),
		`MATCH (x:Repro {k:'a'}), (y:Repro {k:'b'}) MERGE (x)-[:E {kind: $pk}]->(y)`,
		map[string]any{"pk": "viaparam"}); err != nil {
		t.Fatalf("RunInTxAny: %v", err)
	}

	if got := relScalarString(t, eng,
		`MATCH (:Repro {k:'a'})-[e:E]->(:Repro {k:'b'}) RETURN e.kind AS v`); got != "viaparam" {
		t.Fatalf("e.kind = %q, want \"viaparam\"", got)
	}
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 1)
}

// TestMerge_Rel_InlineProp_Mixed verifies a map mixing a literal entry and a
// non-literal entry writes both — the literal must not be lost when the dynamic
// evaluator is installed, and vice versa.
func TestMerge_Rel_InlineProp_Mixed(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'}), (:Repro {k:'b'})`)
	drainRunInTx(t, eng, `UNWIND [{pk:'dyn'}] AS r
		MATCH (x:Repro {k:'a'}), (y:Repro {k:'b'})
		MERGE (x)-[:E {tag: 'lit', kind: r.pk}]->(y)`)

	if got := relScalarString(t, eng,
		`MATCH (:Repro {k:'a'})-[e:E]->(:Repro {k:'b'}) RETURN e.tag AS v`); got != "lit" {
		t.Fatalf("e.tag = %q, want \"lit\"", got)
	}
	if got := relScalarString(t, eng,
		`MATCH (:Repro {k:'a'})-[e:E]->(:Repro {k:'b'}) RETURN e.kind AS v`); got != "dyn" {
		t.Fatalf("e.kind = %q, want \"dyn\"", got)
	}
}

// TestMerge_Pattern_InlineRelProp_NonLiteral_FreshEndpoint pins the compound
// (fresh-endpoint) MERGE path ([exec.MergePattern]): a row-driven inline
// relationship property on a hop whose target node is created fresh must be
// written on the created edge. This path previously rejected such a map with a
// loud compile-time error; it is now supported on par with the
// both-endpoints-bound fast path.
func TestMerge_Pattern_InlineRelProp_NonLiteral_FreshEndpoint(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'})`)
	drainRunInTx(t, eng, `UNWIND [{pk:'normal'}] AS r
		MATCH (x:Repro {k:'a'})
		MERGE (x)-[:E {kind: r.pk}]->(y:New)`)

	if got := relScalarString(t, eng,
		`MATCH (:Repro {k:'a'})-[e:E]->(:New) RETURN e.kind AS v`); got != "normal" {
		t.Fatalf("e.kind = %q, want \"normal\"", got)
	}
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH (n:New) RETURN count(n) AS n`, 1)
}

// TestMerge_Pattern_InlineRelProp_Param_FreshEndpoint pins the $param variant on
// the compound (fresh-endpoint) MERGE path.
func TestMerge_Pattern_InlineRelProp_Param_FreshEndpoint(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'})`)
	if _, err := eng.RunInTxAny(context.Background(),
		`MATCH (x:Repro {k:'a'}) MERGE (x)-[:E {kind: $pk}]->(y:New)`,
		map[string]any{"pk": "viaparam"}); err != nil {
		t.Fatalf("RunInTxAny: %v", err)
	}

	if got := relScalarString(t, eng,
		`MATCH (:Repro {k:'a'})-[e:E]->(:New) RETURN e.kind AS v`); got != "viaparam" {
		t.Fatalf("e.kind = %q, want \"viaparam\"", got)
	}
}

// TestMerge_Pattern_InlineRelProp_DrivesSearch verifies the evaluated hop
// property drives the whole-pattern search on the compound path: a second MERGE
// with the same value matches the pattern created by the first (no duplicate),
// while a MERGE with a different value does not match and creates a fresh
// pattern.
func TestMerge_Pattern_InlineRelProp_DrivesSearch(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'})`)
	const q = `UNWIND [{pk:'normal'}] AS r
		MATCH (x:Repro {k:'a'})
		MERGE (x)-[:E {kind: r.pk}]->(y:New)`

	drainRunInTx(t, eng, q)
	drainRunInTx(t, eng, q) // same value → matches the first pattern, no duplicate.
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH (n:New) RETURN count(n) AS n`, 1)

	// A different value does not match → a fresh pattern (node + edge) is made.
	drainRunInTx(t, eng, `UNWIND [{pk:'other'}] AS r
		MATCH (x:Repro {k:'a'})
		MERGE (x)-[:E {kind: r.pk}]->(y:New)`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 2)
	assertCount(context.Background(), t, eng, `MATCH (n:New) RETURN count(n) AS n`, 2)
}

// TestMerge_Rel_InlineProp_NonLiteral_Undirected verifies the evaluated
// property drives the undirected reverse-direction match: an existing reverse
// edge carrying the evaluated value is bound rather than a new one created.
func TestMerge_Rel_InlineProp_NonLiteral_Undirected(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:Repro {k:'a'}), (:Repro {k:'b'})`)
	// Seed a forward a→b edge with kind='link'.
	drainRunInTx(t, eng, `MATCH (x:Repro {k:'a'}), (y:Repro {k:'b'}) CREATE (x)-[:E {kind:'link'}]->(y)`)

	// Undirected MERGE from b with the evaluated value 'link' must bind to the
	// existing forward edge (reverse probe), not create a second one.
	drainRunInTx(t, eng, `UNWIND [{pk:'link'}] AS r
		MATCH (x:Repro {k:'b'}), (y:Repro {k:'a'})
		MERGE (x)-[:E {kind: r.pk}]-(y)`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 1)

	// A non-matching evaluated value creates a new edge.
	drainRunInTx(t, eng, `UNWIND [{pk:'other'}] AS r
		MATCH (x:Repro {k:'b'}), (y:Repro {k:'a'})
		MERGE (x)-[:E {kind: r.pk}]-(y)`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:E]->() RETURN count(e) AS n`, 2)
}
