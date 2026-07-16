package cypher_test

// merge_runtime_null_test.go — regression coverage for a MERGE property whose
// value evaluates to null at RUNTIME (#2023).
//
// openCypher forbids merging on a null property value: the pattern can never
// match its own write (MergeReadOwnWrites). A literal null is rejected at build
// time; a value that only evaluates to null at runtime (e.g. `MERGE (n {p:
// row.missing})`) used to be silently omitted, so the MERGE ran with no
// predicate for that key and wrote a node/edge with the key absent. It must
// raise the same MergeReadOwnWrites error. The rule is engine-wide: node Merge,
// MergeRelationship, and MergePattern. CREATE keeps its null-is-a-no-op
// semantics (a null property on CREATE is not an error).

import (
	"strings"
	"testing"
)

func assertMergeReadOwnWrites(t *testing.T, err error, query string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a MergeReadOwnWrites error for a runtime-null MERGE property, got nil", query)
	}
	if !strings.Contains(err.Error(), "MergeReadOwnWrites") {
		t.Fatalf("%s: error %q should be MergeReadOwnWrites", query, err.Error())
	}
}

// TestMergeRuntimeNull_Node: node MERGE with a runtime-null property raises.
func TestMergeRuntimeNull_Node(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	q := `UNWIND [{m:1}] AS r MERGE (n:L {p: r.missing})`
	assertMergeReadOwnWrites(t, runDrainErr(t, eng, q), q)
}

// TestMergeRuntimeNull_Relationship: MergeRelationship (both endpoints bound)
// with a runtime-null inline property raises.
func TestMergeRuntimeNull_Relationship(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {k:1}),(:B {k:2})`)
	q := `UNWIND [{m:1}] AS r MATCH (a:A),(b:B) MERGE (a)-[:R {kind: r.missing}]->(b)`
	assertMergeReadOwnWrites(t, runDrainErr(t, eng, q), q)
}

// TestMergeRuntimeNull_Pattern_HopProp: MergePattern (fresh endpoint) with a
// runtime-null hop property raises.
func TestMergeRuntimeNull_Pattern_HopProp(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {id:1})`)
	q := `UNWIND [{m:1}] AS r MATCH (a:A) MERGE (a)-[:R {k: r.missing}]->(y:New)`
	assertMergeReadOwnWrites(t, runDrainErr(t, eng, q), q)
}

// TestMergeRuntimeNull_Pattern_NodeProp: MergePattern with a runtime-null
// fresh-node property raises.
func TestMergeRuntimeNull_Pattern_NodeProp(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {id:1})`)
	q := `UNWIND [{m:1}] AS r MATCH (a:A) MERGE (a)-[:R]->(y:New {p: r.missing})`
	assertMergeReadOwnWrites(t, runDrainErr(t, eng, q), q)
}

// TestMergeRuntimeNonNull_Node_Unaffected: a MERGE whose property evaluates to
// a non-null value still creates/matches normally.
func TestMergeRuntimeNonNull_Node_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `UNWIND [{v:'V'}] AS r MERGE (n:L {p: r.v})`)
	if got := setScalarString(t, eng, `MATCH (n:L) RETURN n.p AS v`); got != "V" {
		t.Fatalf("n.p = %q, want \"V\"", got)
	}
}

// TestCreateRuntimeNull_Node_NoError guards that CREATE keeps its
// null-is-a-no-op semantics: a runtime-null property is omitted, not an error.
func TestCreateRuntimeNull_Node_NoError(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	if err := runDrainErr(t, eng, `UNWIND [{m:1}] AS r CREATE (n:N {name:'keep', p: r.missing})`); err != nil {
		t.Fatalf("CREATE with a runtime-null property must be a no-op, not an error: %v", err)
	}
	// The node is still created, with the non-null property written and the
	// null-valued one omitted.
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.name AS v`); got != "keep" {
		t.Fatalf("n.name = %q, want \"keep\" (node created)", got)
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.p AS v`) {
		t.Fatal("CREATE runtime-null property should be omitted (null)")
	}
}
