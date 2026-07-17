package exec_test

// grouping_intfloat_exact_test.go — rmp #2050
//
// Exact cross-type INTEGER↔FLOAT equality (CIP2016-06-14) at the DISTINCT and
// EagerAggregation-grouping consumers. Both resolve hash-bucket collisions with
// expr.Equivalent, so they inherit the value-layer exactness fix.
//
// The pre-fix bug had two symptoms this file pins against:
//
//  1. Non-transitivity → order-dependence. With the lossy float64 promotion,
//     int 2^53 ≡ float 2^53.0 AND int 2^53+1 ≡ float 2^53.0 (both ints round to
//     2^53.0) yet int 2^53 ≢ int 2^53+1 — a non-transitive "equivalence" whose
//     DISTINCT/group count depended on insertion order (1 or 2). The exact
//     comparator restores transitivity: int 2^53 ≡ float 2^53.0 (still equal,
//     representable) and int 2^53+1 is distinct from both, giving a stable
//     count of 2 for EVERY ordering.
//
//  2. Over-collapse. Three pairwise-distinct numbers whose two ints share a
//     float64 image (2^53+1, 2^53+2, 2^53.0) must stay three distinct
//     groups/values, where the lossy path collapsed them.
//
// Note on the task's nominal "{2^53, 2^53+1, 2^53.0} → 3 distinct": under the
// spec-exact semantics int 2^53 EQUALS float 2^53.0 (2^53 is representable), so
// that set is correctly 2 distinct, not 3. The pairwise-distinct set below is
// the faithful "3 distinct" case.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

const (
	p53   int64   = 1 << 53       // 9007199254740992
	p53p1 int64   = (1 << 53) + 1 // 9007199254740993 (not float64-representable)
	p53p2 int64   = (1 << 53) + 2 // 9007199254740994 (not float64-representable)
	p53f  float64 = float64(1 << 53)
)

// permute3 returns every ordering of a 3-element slice.
func permute3(a, b, c expr.Value) [][]expr.Value {
	return [][]expr.Value{
		{a, b, c}, {a, c, b}, {b, a, c},
		{b, c, a}, {c, a, b}, {c, b, a},
	}
}

// TestDistinct_IntFloatExact_OrderIndependent verifies DISTINCT over
// {int 2^53, int 2^53+1, float 2^53.0} is a stable 2 for every insertion order
// (int 2^53 ≡ float 2^53.0; int 2^53+1 distinct). Pre-fix this was 1 or 2.
func TestDistinct_IntFloatExact_OrderIndependent(t *testing.T) {
	for _, order := range permute3(
		expr.IntegerValue(p53), expr.IntegerValue(p53p1), expr.FloatValue(p53f),
	) {
		if got := collectDistinct(t, order); got != 2 {
			t.Errorf("DISTINCT %v: got %d, want 2 (2^53 ≡ 2^53.0; 2^53+1 distinct)", order, got)
		}
	}
}

// TestGrouping_IntFloatExact_OrderIndependent is the grouping analogue: two
// groups for every ordering of the same set.
func TestGrouping_IntFloatExact_OrderIndependent(t *testing.T) {
	for _, order := range permute3(
		expr.IntegerValue(p53), expr.IntegerValue(p53p1), expr.FloatValue(p53f),
	) {
		rows := make([]exec.Row, 0, len(order))
		for _, v := range order {
			rows = append(rows, exec.Row{v})
		}
		if got := groupCount(t, rows); got != 2 {
			t.Errorf("grouping %v: got %d groups, want 2", order, got)
		}
	}
}

// TestDistinct_IntFloatExact_ThreePairwiseDistinct verifies three pairwise-
// distinct numbers whose two integers share a float64 image stay three distinct
// values for every ordering. Pre-fix the lossy path collapsed them.
func TestDistinct_IntFloatExact_ThreePairwiseDistinct(t *testing.T) {
	for _, order := range permute3(
		expr.IntegerValue(p53p1), expr.IntegerValue(p53p2), expr.FloatValue(p53f),
	) {
		if got := collectDistinct(t, order); got != 3 {
			t.Errorf("DISTINCT %v: got %d, want 3 (all pairwise distinct)", order, got)
		}
	}
}

// TestGrouping_IntFloatExact_ThreePairwiseDistinct is the grouping analogue.
func TestGrouping_IntFloatExact_ThreePairwiseDistinct(t *testing.T) {
	for _, order := range permute3(
		expr.IntegerValue(p53p1), expr.IntegerValue(p53p2), expr.FloatValue(p53f),
	) {
		rows := make([]exec.Row, 0, len(order))
		for _, v := range order {
			rows = append(rows, exec.Row{v})
		}
		if got := groupCount(t, rows); got != 3 {
			t.Errorf("grouping %v: got %d groups, want 3", order, got)
		}
	}
}

// TestDistinct_IntFloatExact_StillEqual is a non-regression guard: the small
// representable cases must still collapse (1 ≡ 1.0, 2^53 ≡ 2^53.0).
func TestDistinct_IntFloatExact_StillEqual(t *testing.T) {
	if got := collectDistinct(t, []expr.Value{expr.IntegerValue(1), expr.FloatValue(1.0)}); got != 1 {
		t.Errorf("DISTINCT [1, 1.0]: got %d, want 1", got)
	}
	if got := collectDistinct(t, []expr.Value{expr.IntegerValue(p53), expr.FloatValue(p53f)}); got != 1 {
		t.Errorf("DISTINCT [2^53, 2^53.0]: got %d, want 1", got)
	}
}
