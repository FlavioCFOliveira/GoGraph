package cypher_test

// create_param_map_test.go — regression coverage for a whole property map
// supplied as a bare parameter on CREATE (#2028): CREATE (n $props) and
// CREATE (a)-[:R $props]->(b). openCypher's Properties production allows a
// Parameter in place of a map literal; this used to fail at build time with
// "expected map literal enclosed in {}". MERGE, by contrast, forbids a
// parameter as its whole property predicate (InvalidParameterUse) and must
// still be rejected.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// runDrainErrP runs a write with params, drains it, and returns the first error.
func runDrainErrP(t *testing.T, eng *cypher.Engine, query string, params map[string]any) error {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), query, params)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	defer res.Close()
	return res.Err()
}

// TestCreateParamMap_Node: CREATE (n $props) writes the parameter map's entries.
func TestCreateParamMap_Node(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	if err := runDrainErrP(t, eng, `CREATE (n:N $m)`,
		map[string]any{"m": map[string]any{"x": "V", "n": int64(7)}}); err != nil {
		t.Fatalf("CREATE (n:N $m): %v", err)
	}
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
}

// TestCreateParamMap_Relationship: CREATE (a)-[:R $props]->(b).
func TestCreateParamMap_Relationship(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	drainRunInTx(t, eng, `CREATE (:A {k:1}),(:B {k:2})`)
	if err := runDrainErrP(t, eng, `MATCH (a:A),(b:B) CREATE (a)-[:R $m]->(b)`,
		map[string]any{"m": map[string]any{"x": "V"}}); err != nil {
		t.Fatalf("CREATE rel $m: %v", err)
	}
	if got := setScalarString(t, eng, `MATCH ()-[e:R]->() RETURN e.x AS v`); got != "V" {
		t.Fatalf("e.x = %q, want \"V\"", got)
	}
}

// TestCreateParamMap_NullEntryOmitted: a null-valued entry in the parameter map
// is omitted (assigning null on CREATE is a no-op), the rest written.
func TestCreateParamMap_NullEntryOmitted(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	if err := runDrainErrP(t, eng, `CREATE (n:N $m)`,
		map[string]any{"m": map[string]any{"x": "V", "y": nil}}); err != nil {
		t.Fatalf("CREATE (n:N $m): %v", err)
	}
	if got := setScalarString(t, eng, `MATCH (n:N) RETURN n.x AS v`); got != "V" {
		t.Fatalf("n.x = %q, want \"V\"", got)
	}
	if !setScalarIsNull(t, eng, `MATCH (n:N) RETURN n.y AS v`) {
		t.Fatal("n.y must be omitted (null entry)")
	}
}

// TestCreateParamMap_NonMap_TypeError: a scalar parameter used as a whole
// property map is a TypeError.
func TestCreateParamMap_NonMap_TypeError(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	err := runDrainErrP(t, eng, `CREATE (n:N $m)`, map[string]any{"m": "scalar"})
	if err == nil {
		t.Fatal("CREATE (n:N $scalar) must be a TypeError")
	}
	if !strings.Contains(err.Error(), "TypeError") {
		t.Fatalf("error %q should be a TypeError", err.Error())
	}
}

// TestMergeParamMap_StillRejected guards that MERGE (n $param) remains an
// InvalidParameterUse compile-time error (openCypher forbids it; TCK Merge1[16]).
func TestMergeParamMap_StillRejected(t *testing.T) {
	t.Parallel()
	eng := setMapEng(t)
	err := runDrainErrP(t, eng, `MERGE (n:N $m)`, map[string]any{"m": map[string]any{"x": "V"}})
	if err == nil {
		t.Fatal("MERGE (n $param) must be rejected (InvalidParameterUse)")
	}
}
