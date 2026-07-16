package cypher_test

// entity_valued_prop_test.go — regression coverage for a node- or
// relationship-valued inline property (#2025).
//
// A property value must be a primitive or a homogeneous list of primitives; a
// node or relationship is not storable (openCypher InvalidPropertyType). Such a
// value used to be silently dropped by the property evaluator (the target kept
// null), instead of raising. The rule is engine-wide: CREATE, node Merge,
// MergeRelationship, MergePattern. The scalar-vs-NodeID mis-upgrade guard is
// preserved: a scalar integer from an UNWIND element / aggregate / projection
// alias used as a property value still works.

import (
	"context"
	"strings"
	"testing"
)

func assertInvalidPropertyType(t *testing.T, err error, query string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an InvalidPropertyType error for an entity-valued property, got nil", query)
	}
	if !strings.Contains(err.Error(), "InvalidPropertyType") {
		t.Fatalf("%s: error %q should be InvalidPropertyType", query, err.Error())
	}
}

// TestEntityProp_Create_Rel_NodeValued is the reported repro: a node-valued
// inline relationship property on CREATE.
func TestEntityProp_Create_Rel_NodeValued(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {k:1})`)
	q := `MATCH (a:A) CREATE (a)-[e:R {owner: a}]->(b:B)`
	assertInvalidPropertyType(t, runDrainErr(t, eng, q), q)
}

// TestEntityProp_Create_Node_NodeValued: a node-valued property on a created
// node.
func TestEntityProp_Create_Node_NodeValued(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {k:1})`)
	q := `MATCH (a:A) CREATE (n:N {owner: a})`
	assertInvalidPropertyType(t, runDrainErr(t, eng, q), q)
}

// TestEntityProp_Merge_Rel_NodeValued: the MergeRelationship path.
func TestEntityProp_Merge_Rel_NodeValued(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {k:1}),(:B {k:2})`)
	q := `MATCH (a:A),(b:B) MERGE (a)-[e:R {owner: a}]->(b)`
	assertInvalidPropertyType(t, runDrainErr(t, eng, q), q)
}

// TestEntityProp_Merge_Node_NodeValued: node-valued property on a node MERGE
// pattern predicate.
func TestEntityProp_Merge_Node_NodeValued(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {k:1})`)
	q := `MATCH (a:A) MERGE (n:N {owner: a})`
	assertInvalidPropertyType(t, runDrainErr(t, eng, q), q)
}

// TestEntityProp_ScalarCol_Unaffected guards the Merge1 flake fix: a scalar
// integer from an UNWIND element used as a property value is written, not
// mistaken for a node reference and dropped or (now) rejected.
func TestEntityProp_ScalarCol_Unaffected(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `UNWIND range(0, 5) AS i CREATE (:N {count: i})`)
	assertCount(context.Background(), t, eng, `MATCH (n:N) RETURN count(n) AS n`, 6)
	// The value must be the scalar, faithfully stored — not dropped.
	assertCount(context.Background(), t, eng, `MATCH (n:N) WHERE n.count = 3 RETURN count(n) AS n`, 1)
	assertCount(context.Background(), t, eng, `MATCH (n:N) WHERE n.count IS NULL RETURN count(n) AS n`, 0)
}
