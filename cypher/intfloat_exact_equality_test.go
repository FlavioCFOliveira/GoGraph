package cypher_test

// intfloat_exact_equality_test.go — rmp #2050
//
// End-to-end proof that the exact cross-type INTEGER↔FLOAT equality
// (CIP2016-06-14) reaches every consumer through the real Engine: the `=`
// operator, the WHERE predicate, list equality, DISTINCT, and grouping. Before
// the fix a large integer no float64 can represent (2^53+1 = 9007199254740993)
// wrongly compared/grouped equal to the nearest float (2^53.0 =
// 9007199254740992.0).
//
// Literals are spelled in full decimal so the parser sees genuine INTEGER and
// FLOAT tokens (2^53   = 9007199254740992, 2^53+1 = 9007199254740993,
// 2^53+2 = 9007199254740994, 2^53.0 = 9007199254740992.0).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func wantBool(t *testing.T, row map[string]interface{}, key string, want bool) {
	t.Helper()
	got, ok := row[key].(expr.BoolValue)
	if !ok {
		t.Fatalf("%s: want BoolValue, got %T (%v)", key, row[key], row[key])
	}
	if bool(got) != want {
		t.Errorf("%s = %v, want %v", key, bool(got), want)
	}
}

func wantInt(t *testing.T, row map[string]interface{}, key string, want int64) {
	t.Helper()
	got, ok := row[key].(expr.IntegerValue)
	if !ok {
		t.Fatalf("%s: want IntegerValue, got %T (%v)", key, row[key], row[key])
	}
	if int64(got) != want {
		t.Errorf("%s = %d, want %d", key, int64(got), want)
	}
}

// TestE2E_EqualityOperator_CrossTypeExact exercises the `=` operator path.
func TestE2E_EqualityOperator_CrossTypeExact(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `RETURN
		9007199254740993 = 9007199254740992.0 AS big,
		9007199254740992 = 9007199254740992.0 AS rep,
		1 = 1.0 AS one,
		2 = 2.5 AS frac`)
	wantBool(t, row, "big", false)  // 2^53+1 ≠ 2^53.0 (the fix)
	wantBool(t, row, "rep", true)   // 2^53 == 2^53.0 (representable)
	wantBool(t, row, "one", true)   // 1 == 1.0 (still equal)
	wantBool(t, row, "frac", false) // 2 ≠ 2.5
}

// TestE2E_ListEquality_CrossTypeExact exercises list element equality.
func TestE2E_ListEquality_CrossTypeExact(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)
	row := singleRow(t, eng, `RETURN
		[9007199254740993] = [9007199254740992.0] AS big,
		[9007199254740992] = [9007199254740992.0] AS rep`)
	wantBool(t, row, "big", false)
	wantBool(t, row, "rep", true)
}

// TestE2E_Where_CrossTypeExact exercises the WHERE predicate path: a row whose
// integer is 2^53+1 must be filtered out by `= 2^53.0`, while 2^53 survives.
func TestE2E_Where_CrossTypeExact(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)

	out := singleRow(t, eng,
		`UNWIND [9007199254740993] AS x WITH x WHERE x = 9007199254740992.0 RETURN count(*) AS c`)
	wantInt(t, out, "c", 0)

	in := singleRow(t, eng,
		`UNWIND [9007199254740992] AS x WITH x WHERE x = 9007199254740992.0 RETURN count(*) AS c`)
	wantInt(t, in, "c", 1)
}

// TestE2E_CountDistinct_CrossTypeExact exercises count(DISTINCT …). The set
// {2^53, 2^53+1, 2^53.0} is 2 distinct (2^53 ≡ 2^53.0; 2^53+1 distinct); the
// pairwise-distinct set {2^53+1, 2^53+2, 2^53.0} is 3.
func TestE2E_CountDistinct_CrossTypeExact(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)

	two := singleRow(t, eng,
		`UNWIND [9007199254740992, 9007199254740993, 9007199254740992.0] AS x RETURN count(DISTINCT x) AS c`)
	wantInt(t, two, "c", 2)

	three := singleRow(t, eng,
		`UNWIND [9007199254740993, 9007199254740994, 9007199254740992.0] AS x RETURN count(DISTINCT x) AS c`)
	wantInt(t, three, "c", 3)
}

// TestE2E_Grouping_CrossTypeExact exercises EagerAggregation grouping through
// the engine: the number of groups matches the distinct-value count above.
func TestE2E_Grouping_CrossTypeExact(t *testing.T) {
	t.Parallel()
	eng := newAggEngine(t)

	res2, err := eng.Run(t.Context(),
		`UNWIND [9007199254740992, 9007199254740993, 9007199254740992.0] AS x RETURN x AS k, count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("Run (2-group): %v", err)
	}
	if n := countRows(t, res2); n != 2 {
		t.Errorf("grouping {2^53, 2^53+1, 2^53.0}: got %d groups, want 2", n)
	}

	res3, err := eng.Run(t.Context(),
		`UNWIND [9007199254740993, 9007199254740994, 9007199254740992.0] AS x RETURN x AS k, count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("Run (3-group): %v", err)
	}
	if n := countRows(t, res3); n != 3 {
		t.Errorf("grouping {2^53+1, 2^53+2, 2^53.0}: got %d groups, want 3", n)
	}
}
