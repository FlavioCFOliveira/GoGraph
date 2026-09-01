package exec

// keys_compare_test.go — #2509
//
// [keysLess] and [keysCompare] are two spellings of one ordering, kept apart for
// a measured reason ([Sort] performs Θ(n log n) boolean comparisons and must not
// pay a non-inlinable wrapper call for each; [Top] must distinguish "before" from
// "equal" in one pass or pay a second full comparison on every tie). Two
// spellings is two places for the ordering to drift, so this file pins them to
// each other over inputs built to exercise every branch they contain.

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestKeysCompareAgreesWithKeysLess is the anti-drift gate.
//
// The generated key blocks draw from a domain of four values plus NULL, so ties
// and NULL comparisons are the common case rather than the exception — a
// generator of distinct values would exercise neither the tie-break continuation
// nor the DESC negation that puts NULL first.
func TestKeysCompareAgreesWithKeysLess(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nKeys := rapid.IntRange(1, 4).Draw(rt, "keys")
		keys := make([]SortKey, nKeys)
		for j := range keys {
			keys[j] = SortKey{Ascending: rapid.Bool().Draw(rt, "asc")}
		}
		draw := func(label string) []expr.Value {
			out := make([]expr.Value, nKeys)
			for j := range out {
				v := rapid.IntRange(-1, 3).Draw(rt, label)
				if v < 0 {
					out[j] = expr.Null
					continue
				}
				out[j] = expr.IntegerValue(int64(256 + v))
			}
			return out
		}
		a, b := draw("a"), draw("b")

		cmp := keysCompare(keys, a, b)
		if got, want := keysLess(keys, a, b), cmp < 0; got != want {
			rt.Fatalf("keysLess=%v but keysCompare=%d for a=%v b=%v keys=%v", got, cmp, a, b, keys)
		}
		// Antisymmetry, which also catches a sign error that happened to agree
		// with keysLess in one direction only.
		if rev := keysCompare(keys, b, a); (cmp < 0) != (rev > 0) || (cmp == 0) != (rev == 0) {
			rt.Fatalf("keysCompare is not antisymmetric: (a,b)=%d (b,a)=%d for a=%v b=%v", cmp, rev, a, b)
		}
	})
}

// TestKeysCompareMatchesRowCompare pins the DECORATED three-way comparator
// against the LEGACY [rowCompareForKeys], so the arrival-ordinal tie-break in
// [Top] cannot be reached from two different notions of "tied". Without it,
// Top's two arms could order equal-keyed rows differently while every
// same-arm test still passed.
func TestKeysCompareMatchesRowCompare(t *testing.T) {
	keys := []SortKey{
		{ColIdx: 0, Ascending: true},
		{ColIdx: 1, Ascending: false},
	}
	vals := []expr.Value{expr.Null, expr.IntegerValue(256), expr.IntegerValue(257)}
	for _, a0 := range vals {
		for _, a1 := range vals {
			for _, b0 := range vals {
				for _, b1 := range vals {
					rowA := Row{a0, a1}
					rowB := Row{b0, b1}
					dec := keysCompare(keys, []expr.Value{a0, a1}, []expr.Value{b0, b1})
					leg := rowCompareForKeys(rowA, rowB, keys)
					if (dec < 0) != (leg < 0) || (dec > 0) != (leg > 0) {
						t.Fatalf("decorated=%d legacy=%d for a=%v b=%v", dec, leg, rowA, rowB)
					}
				}
			}
		}
	}
}
