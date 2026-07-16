package cypher_test

// merge_oncreate_setall_test.go — regression coverage for whole-entity
// ON CREATE / ON MATCH SET on the node Merge and MergePattern paths (#2031).
//
// `MERGE (n) ON CREATE SET n = {…}` / `n += {…}` / `n = <mapvar>` / `n = <node>`
// used to be silently dropped: parseMergeActions only recognised `var.key = …`
// and `var:Label` forms, so a keyless whole-entity action was discarded and the
// node kept null/stale data while the statement reported success. The
// relationship MergeRelationship fast path already handled the whole-entity
// form (TCK Merge6[6]); this pins the node/pattern paths to parity.

import (
	"context"
	"strings"
	"testing"
)

// TestMergeSetAll_Node_OnCreate_Replace: `ON CREATE SET n = {lit}` writes the
// map and (replace) clears the merge key.
func TestMergeSetAll_Node_OnCreate_Replace(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `MERGE (n:N {id:1}) ON CREATE SET n = {x:'V'}`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.id AS v`) {
		t.Fatal("n.id must be cleared by = replace")
	}
}

// TestMergeSetAll_Node_OnCreate_Merge: `ON CREATE SET n += {lit}` writes the map
// and keeps the merge key.
func TestMergeSetAll_Node_OnCreate_Merge(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `MERGE (n:N {id:1}) ON CREATE SET n += {x:'V'}`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	if setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.id AS v`) {
		t.Fatal("n.id must be kept by += merge")
	}
}

// TestMergeSetAll_Node_OnCreate_MapVar: `ON CREATE SET n += <mapvar>` (row map).
func TestMergeSetAll_Node_OnCreate_MapVar(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `UNWIND [{x:'V'}] AS r MERGE (n:N {id:1}) ON CREATE SET n += r`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
}

// TestMergeSetAll_Node_OnMatch fires ON MATCH on an existing node.
func TestMergeSetAll_Node_OnMatch(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	drainRunInTx(t, eng, `MERGE (n:N {id:1}) ON MATCH SET n += {x:'V'}`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
}

// TestMergeSetAll_Node_OnCreate_NodeCopy: `ON CREATE SET n = m` copies node m.
func TestMergeSetAll_Node_OnCreate_NodeCopy(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:M {x:'V'})`)
	drainRunInTx(t, eng, `MATCH (m:M) MERGE (n:N {id:1}) ON CREATE SET n = m`)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\" (node copy)", got)
	}
}

// TestMergeSetAll_Node_OnCreate_OnlyOnCreate: ON CREATE does NOT fire when the
// node already exists (the write must be conditional on creation).
func TestMergeSetAll_Node_OnCreate_OnlyOnCreate(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:N {id:1})`)
	drainRunInTx(t, eng, `MERGE (n:N {id:1}) ON CREATE SET n += {x:'V'}`)
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.x AS v`) {
		t.Fatal("ON CREATE must not fire when the node already existed")
	}
}

// TestMergeSetAll_Pattern_OnCreate: MergePattern (fresh endpoint) ON CREATE SET
// on the fresh node.
func TestMergeSetAll_Pattern_OnCreate(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {k:1})`)
	drainRunInTx(t, eng, `MATCH (a:A) MERGE (a)-[:R]->(b:New) ON CREATE SET b += {x:'V'}`)
	if got := setScalarString(t, eng, `MATCH (:A)-[:R]->(b:New) RETURN b.x AS v`); got != "V" {
		t.Fatalf("b.x = %q, want \"V\" (pattern ON CREATE)", got)
	}
}

// TestMergeSetAll_Rel_Unaffected guards the relationship whole-entity path
// (Merge6[6]-style) against regression.
func TestMergeSetAll_Rel_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {k:1}),(:B {k:2})`)
	drainRunInTx(t, eng, `MATCH (a:A),(b:B) MERGE (a)-[r:R]->(b) ON CREATE SET r += {x:'V'}`)
	if got := setScalarString(t, eng, `MATCH ()-[r:R]->() RETURN r.x AS v`); got != "V" {
		t.Fatalf("r.x = %q, want \"V\" (rel ON CREATE)", got)
	}
}

// TestMergeSetAll_ScalarRHS_TypeError: a scalar RHS in a whole-entity ON CREATE
// SET is a TypeError, not a silent no-op.
func TestMergeSetAll_ScalarRHS_TypeError(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	err := runDrainErr(t, eng, `MERGE (n:N {id:1}) ON CREATE SET n = size([1,2,3])`)
	if err == nil {
		t.Fatal("ON CREATE SET n = <scalar> must be a TypeError, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "TypeError") {
		t.Fatalf("error %q should be a TypeError", err.Error())
	}
}

// TestMergeSetAll_Node_Idempotent verifies the whole-entity ON CREATE value is
// written once and re-running the MERGE matches (no duplicate, no re-clobber).
func TestMergeSetAll_Node_Idempotent(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	const q = `MERGE (n:N {id:1}) ON CREATE SET n += {x:'V'}`
	drainRunInTx(t, eng, q)
	drainRunInTx(t, eng, q)
	assertCount(context.Background(), t, eng, `MATCH (n:N) RETURN count(n) AS n`, 1)
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
}
