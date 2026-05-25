package cypher_test

// unwind_null_test.go — UNWIND behaviour with NULL elements in the list (T714).
//
// OpenCypher specification (§ UNWIND):
//   "If the list contains null as an element, a null row is produced for it."
//
// This engine's observed behaviour is documented inline per test. If the engine
// later changes its semantics, update the expected counts and comments.

import (
	"context"
	"testing"

	"gograph/cypher/expr"
)

// TestUnwindNull_MixedList verifies the engine's handling of a list that
// contains both non-null integers and a null element:
//
//	UNWIND [1, null, 3] AS x RETURN x
//
// OpenCypher says null produces a row with x = NULL; the engine may either:
//
//	(a) emit a NULL row  → 3 rows total
//	(b) skip null        → 2 rows total
//
// The test documents whichever behaviour the engine exhibits and asserts it is
// stable. Non-null values must appear in position order.
func TestUnwindNull_MixedList(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	res, err := eng.Run(context.Background(), `UNWIND [1, null, 3] AS x RETURN x`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	// Observed engine behaviour: null in a list position produces a NULL row
	// (openCypher-compliant). Expected count is 3.
	//
	// If the engine skips nulls (non-compliant but acceptable shortcut),
	// update the want below to 2 and note it here.
	const want = 3
	if len(rows) != want {
		t.Errorf("UNWIND [1, null, 3]: got %d rows, want %d (null row expected)", len(rows), want)
	}

	// Regardless of null-row count, the non-null values must appear and carry
	// their integer values. Collect only non-null rows for value verification.
	var nonNull []int64
	for _, row := range rows {
		v := row["x"]
		if iv, ok := v.(expr.IntegerValue); ok {
			nonNull = append(nonNull, int64(iv))
		}
		// NULL row: expr.Null or nil — both are acceptable null representations.
	}
	wantVals := []int64{1, 3}
	if len(nonNull) != len(wantVals) {
		t.Fatalf("non-null values: got %v, want %v", nonNull, wantVals)
	}
	for i := range wantVals {
		if nonNull[i] != wantVals[i] {
			t.Errorf("non-null[%d] = %d, want %d", i, nonNull[i], wantVals[i])
		}
	}
}

// TestUnwindNull_AllNulls verifies UNWIND over a list of only null values:
//
//	UNWIND [null, null] AS x RETURN x
//
// Expected by openCypher: 2 rows, each with x = NULL.
// If the engine skips nulls it emits 0 rows; update want accordingly.
func TestUnwindNull_AllNulls(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	res, err := eng.Run(context.Background(), `UNWIND [null, null] AS x RETURN x`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	// OpenCypher-compliant: 2 rows with x = NULL.
	// Engine may emit 0 rows if it skips nulls — document that divergence here.
	const want = 2
	if len(rows) != want {
		t.Errorf("UNWIND [null, null]: got %d rows, want %d", len(rows), want)
	}

	// Every row must carry a null value for x. expr.Value is an interface;
	// the concrete type expr.nullValue is unexported, so we check via
	// expr.IsNull after a type assertion to expr.Value.
	for i, row := range rows {
		v := row["x"]
		ev, ok := v.(expr.Value)
		if !ok {
			t.Errorf("row %d: x = %v (%T), expected expr.Value", i, v, v)
			continue
		}
		if !expr.IsNull(ev) {
			t.Errorf("row %d: x = %v, want NULL", i, ev)
		}
	}
}

// TestUnwindNull_WhereIsNotNull verifies that a WHERE x IS NOT NULL filter
// downstream of UNWIND eliminates the null row.
//
// The Cypher parser does not accept WHERE directly after UNWIND; the filter
// must be expressed as a WITH … WHERE clause:
//
//	UNWIND [null] AS x WITH x WHERE x IS NOT NULL RETURN x
//
// Expected: 0 rows regardless of whether the engine produces a NULL row for
// null or skips it entirely — the WHERE clause must filter it out either way.
func TestUnwindNull_WhereIsNotNull(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	// WHERE is only valid after a MATCH/WITH clause; use WITH to bridge.
	res, err := eng.Run(context.Background(), `UNWIND [null] AS x WITH x WHERE x IS NOT NULL RETURN x`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 0 {
		t.Errorf("UNWIND [null] WITH x WHERE x IS NOT NULL: got %d rows, want 0", len(rows))
	}
}
