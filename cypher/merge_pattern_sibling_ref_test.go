package cypher_test

// merge_pattern_sibling_ref_test.go — regression coverage for a MergePattern
// inline property that references a node introduced FRESH by the same MERGE
// pattern (#2024).
//
// CREATE resolves such references left-to-right (`CREATE (a {id:1})-[e:R {k:
// a.id}]->(b)` → e.k = 1). MergePattern used to build its property evaluators
// against the driving-row snapshot taken before the pattern's own columns were
// added, so a same-pattern reference resolved to null on both the fresh-node
// and hop property. It must now resolve left-to-right as CREATE does.

import (
	"context"
	"testing"
)

// TestMergePatternSiblingRef_NodeProp: fresh node b's property references the
// fresh sibling a.
func TestMergePatternSiblingRef_NodeProp(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `MERGE (a:A {id:1})-[:R]->(b:B {k: a.id})`)
	assertCount(context.Background(), t, eng, `MATCH (:A)-[:R]->(b:B) WHERE b.k = 1 RETURN count(b) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH (b:B) WHERE b.k IS NULL RETURN count(b) AS n`, 0)
}

// TestMergePatternSiblingRef_HopProp: the hop's property references the fresh
// source node a.
func TestMergePatternSiblingRef_HopProp(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `MERGE (a:A {id:1})-[e:R {k: a.id}]->(b:B)`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:R]->() WHERE e.k = 1 RETURN count(e) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:R]->() WHERE e.k IS NULL RETURN count(e) AS n`, 0)
}

// TestMergePatternSiblingRef_Idempotent: the evaluated sibling value drives the
// search predicate too, so re-running the MERGE matches the pattern it created
// rather than duplicating it.
func TestMergePatternSiblingRef_Idempotent(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	const q = `MERGE (a:A {id:1})-[e:R {k: a.id}]->(b:B {k: a.id})`
	drainRunInTx(t, eng, q)
	drainRunInTx(t, eng, q)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:R]->() RETURN count(e) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH (n) RETURN count(n) AS n`, 2)
}

// TestMergePatternSiblingRef_CreateParity guards that CREATE (which already
// resolves fresh siblings left-to-right) still does.
func TestMergePatternSiblingRef_CreateParity(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (a:A {id:1})-[e:R {k: a.id}]->(b:B {k: a.id})`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:R]->() WHERE e.k = 1 RETURN count(e) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH (:A)-[:R]->(b:B) WHERE b.k = 1 RETURN count(b) AS n`, 1)
}

// TestMergePatternSiblingRef_DrivingRowStillWorks guards the common case (an
// inline property that references the DRIVING row, not a sibling) — it must
// keep using the fast per-row path unchanged.
func TestMergePatternSiblingRef_DrivingRowStillWorks(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `UNWIND [{pk:'V'}] AS r MERGE (a:A {id:1})-[e:R {kind: r.pk}]->(b:New)`)
	assertCount(context.Background(), t, eng, `MATCH ()-[e:R]->() WHERE e.kind = 'V' RETURN count(e) AS n`, 1)
}
