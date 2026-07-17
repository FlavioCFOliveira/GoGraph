package exec

// hash_join_intfloat_exact_test.go — rmp #2050
//
// HashJoin equi-join keys resolve a bucket hit with expr.Value.Equal (see the
// per-bucket check in advanceProbe). This test pins that the exact cross-type
// INTEGER↔FLOAT equality (CIP2016-06-14) reaches the join: the representable
// 2^53 and the plain 1 still join their float twins, while an integer no
// float64 can represent (2^53+1) does not. The large-int NON-match is pinned by
// TestHashJoin_LargeIntFloatNoMatch in hash_join_test.go.

import "testing"

const (
	twoP53   int64   = 1 << 53
	twoP53p1 int64   = (1 << 53) + 1
	twoP53f  float64 = float64(1 << 53)
)

// TestHashJoin_CrossTypeExact_RepresentableMatches: the representable 2^53 and
// the small 1 still join their float twins (still-equal cases preserved).
func TestHashJoin_CrossTypeExact_RepresentableMatches(t *testing.T) {
	// probe keys 2^53, 2^53+1, 1 against build floats 2^53.0, 1.0.
	// Only 2^53↔2^53.0 and 1↔1.0 are numerically equal; 2^53+1 has no twin.
	probe := &sliceSource{rows: []Row{{iv(twoP53)}, {iv(twoP53p1)}, {iv(1)}}}
	build := &sliceSource{rows: []Row{{fv(twoP53f)}, {fv(1.0)}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	got := drainJoin(t, hj)
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 matches (2^53↔2^53.0, 1↔1.0), got %d: %v", len(got), got)
	}
}
