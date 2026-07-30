package exec

// join_index_nested_loop_test.go — operator-level unit tests for
// [IndexNestedLoopJoin]'s exact re-verification of its seek candidates
// (rmp #2263).
//
// These sit BELOW the cypher-package regression suite deliberately. That suite
// drives a real graph and a real numeric companion, which means it can only
// present the operator with candidates a real index would produce. Here the
// index is a stub, so a non-matching candidate can be handed to the operator
// directly and the filtering can be observed in isolation from the planner, the
// index and the graph.

import (
	"context"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// stubPointLookup is a [NumericPointLookup] returning a caller-supplied posting
// list per key, so a test can hand the operator candidates that do not match.
type stubPointLookup struct {
	byKey map[float64][]uint64
}

func (s stubPointLookup) LookupAppend(value float64, dst []uint64) []uint64 {
	return append(dst, s.byKey[value]...)
}

// drainINLJ runs the join to completion and renders each output row.
func drainINLJ(t *testing.T, op *IndexNestedLoopJoin) []string {
	t.Helper()
	if err := op.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out []string
	for {
		var r Row
		ok, err := op.Next(&r)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		s := ""
		for i, v := range r {
			if i > 0 {
				s += ","
			}
			s += v.String()
		}
		out = append(out, s)
	}
	if err := op.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestIndexNestedLoopJoin_SeekCandidatesAreReverified hands the operator a
// posting list holding one node that does NOT equal the outer key — the shape a
// lossy float64 index key produces above 2^53 — and requires it to be dropped.
func TestIndexNestedLoopJoin_SeekCandidatesAreReverified(t *testing.T) {
	const big = int64(1) << 53

	// The nodes' ACTUAL values, keyed by node id. Ids 10 and 12 equal the outer
	// key (across the integer/float boundary, which openCypher requires to hold);
	// id 11 holds the distinct integer that shares 10's float64 key; id 13 holds a
	// value that is not joinable at all.
	values := map[int64]expr.Value{
		10: expr.IntegerValue(big),
		11: expr.IntegerValue(big + 1),
		12: expr.FloatValue(float64(big)),
		13: expr.Null,
	}
	innerKeyFn := func(row Row) (expr.Value, error) {
		id, ok := row[0].(expr.IntegerValue)
		if !ok {
			t.Fatalf("seek row column 0 must be the node id, got %T", row[0])
		}
		v, present := values[int64(id)]
		if !present {
			return expr.Null, nil
		}
		return v, nil
	}

	// Every one of these is what the companion would really return for the key:
	// they all file under float64(2^53).
	idx := stubPointLookup{byKey: map[float64][]uint64{
		float64(big): {10, 11, 12, 13},
	}}

	outer := &sliceSource{rows: []Row{{expr.IntegerValue(big)}}}
	inner := &sliceSource{} // the fallback arm is never reached by a numeric key

	op := NewIndexNestedLoopJoin(outer, inner, idx, keyCol(0), innerKeyFn)

	got := drainINLJ(t, op)
	want := []string{"9007199254740992,10", "9007199254740992,12"}

	if len(got) != len(want) {
		t.Fatalf("emitted %d rows, want %d\n  got  %q\n  want %q\n"+
			"Node 11 holds 2^53+1, which shares node 10's float64 index key but is "+
			"NOT equal to the outer key 2^53 under openCypher; node 13 holds NULL, "+
			"which equals nothing. Both must be filtered by the exact re-verification.",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %q, want %q (full: %q)", i, got[i], want[i], got)
		}
	}
}

// TestIndexNestedLoopJoin_ReverificationPreservesOrder checks that dropping a
// candidate does not disturb the ascending-node-id order the operator promises
// within one outer row, nor the outer-major order across rows.
func TestIndexNestedLoopJoin_ReverificationPreservesOrder(t *testing.T) {
	const big = int64(1) << 53

	values := map[int64]expr.Value{
		1: expr.IntegerValue(big), 2: expr.IntegerValue(big + 1),
		3: expr.IntegerValue(big), 4: expr.IntegerValue(big + 1),
		5: expr.IntegerValue(big),
	}
	innerKeyFn := func(row Row) (expr.Value, error) {
		return values[int64(row[0].(expr.IntegerValue))], nil
	}
	idx := stubPointLookup{byKey: map[float64][]uint64{
		float64(big): {1, 2, 3, 4, 5},
	}}

	// Two outer rows carrying the same key, so the per-row seek state (the posting
	// list, its cursor, the ambiguity flag) has to be reset correctly between them.
	outer := &sliceSource{rows: []Row{
		{expr.IntegerValue(big)},
		{expr.IntegerValue(big)},
	}}
	op := NewIndexNestedLoopJoin(outer, &sliceSource{}, idx, keyCol(0), innerKeyFn)

	got := drainINLJ(t, op)
	want := []string{
		"9007199254740992,1", "9007199254740992,3", "9007199254740992,5",
		"9007199254740992,1", "9007199254740992,3", "9007199254740992,5",
	}
	if len(got) != len(want) {
		t.Fatalf("emitted %d rows, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %q, want %q (full: %q)", i, got[i], want[i], got)
		}
	}
}

// BenchmarkIndexNestedLoopJoin_SeekDrain measures the operator's ORDINARY path:
// an unambiguous key, whose candidates are emitted straight from the posting
// list with no property read.
//
// It exists to hold rmp #2263's re-verification to its stated cost. The
// correctness fix is unconditional in effect but conditional in work, and the
// claim that the skip keeps the common seek free is only worth what it is
// measured at — so the shape here is the one the operator was admitted for:
// small keys, a short posting list, one seek per outer row.
func BenchmarkIndexNestedLoopJoin_SeekDrain(b *testing.B) {
	const (
		outerRows = 256
		postings  = 4
	)
	ids := make([]uint64, postings)
	values := make(map[int64]expr.Value, postings)
	for i := range ids {
		ids[i] = uint64(i + 1)
		values[int64(i+1)] = expr.IntegerValue(7)
	}
	idx := stubPointLookup{byKey: map[float64][]uint64{7: ids}}
	innerKeyFn := func(row Row) (expr.Value, error) {
		return values[int64(row[0].(expr.IntegerValue))], nil
	}

	rows := make([]Row, outerRows)
	for i := range rows {
		rows[i] = Row{expr.IntegerValue(7)}
	}
	outer := &sliceSource{rows: rows}
	op := NewIndexNestedLoopJoin(outer, &sliceSource{}, idx, keyCol(0), innerKeyFn)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := op.Init(ctx); err != nil {
			b.Fatal(err)
		}
		n := 0
		for {
			var r Row
			ok, err := op.Next(&r)
			if err != nil {
				b.Fatal(err)
			}
			if !ok {
				break
			}
			n++
		}
		if n != outerRows*postings {
			b.Fatalf("emitted %d rows, want %d", n, outerRows*postings)
		}
	}
}

// TestAmbiguousSeekKey_Boundary pins the exact threshold at which the
// re-verification switches on.
func TestAmbiguousSeekKey_Boundary(t *testing.T) {
	const bound = float64(int64(1) << 53)

	cases := []struct {
		name string
		f    float64
		want bool
	}{
		{"zero", 0, false},
		{"one", 1, false},
		{"minus one", -1, false},
		{"just inside the positive bound", bound - 2, false},
		{"exactly the positive bound", bound, true},
		{"exactly the negative bound", -bound, true},
		{"above the positive bound", bound * 2, true},
		{"below the negative bound", -bound * 2, true},
		{"positive infinity", math.Inf(1), true},
		{"negative infinity", math.Inf(-1), true},
		// NaN cannot reach the seek — isUnjoinableKey and exactFloat64Key both
		// exclude it — but the predicate must still fail SAFE if it ever does.
		{"NaN reads as ambiguous", math.NaN(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ambiguousSeekKey(tc.f); got != tc.want {
				t.Fatalf("ambiguousSeekKey(%v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

// TestAmbiguousSeekKey_UnambiguousKeysAreExact is the guard on the SKIP, and the
// most load-bearing test in this file.
//
// [ambiguousSeekKey] licenses the operator to emit a seek candidate without
// checking it, on the ground that an unambiguous key can only have been produced
// by an integer that float64 holds exactly. If that ever stopped being true the
// skip would start admitting phantom rows again, and no end-to-end test would
// notice, because a real index cannot be made to produce the counterexample on
// demand.
//
// So the property is asserted directly: for every int64 j whose companion key is
// unambiguous, float64(j) round-trips back to j. The sweep covers both sides of
// the boundary exhaustively for a window around 2^53, plus a deterministic spread
// across the whole int64 range.
func TestAmbiguousSeekKey_UnambiguousKeysAreExact(t *testing.T) {
	check := func(j int64) {
		t.Helper()
		f := float64(j)
		if ambiguousSeekKey(f) {
			return // the operator will re-verify this candidate, so nothing to prove
		}
		if int64(f) != j {
			t.Fatalf("int64 %d files under the unambiguous key %v, which the operator "+
				"emits WITHOUT re-verification, but %v converts back to %d — the skip "+
				"would admit a phantom row", j, f, f, int64(f))
		}
	}

	const bound = int64(1) << 53
	for d := int64(-256); d <= 256; d++ {
		check(bound + d)
		check(-bound + d)
	}
	// A deterministic spread over the rest of the range: powers of two and their
	// neighbours, which is where float64 spacing changes.
	for shift := uint(0); shift < 63; shift++ {
		p := int64(1) << shift
		for _, d := range []int64{-1, 0, 1} {
			check(p + d)
			check(-p + d)
		}
	}
	check(0)
	check(math.MaxInt64)
	check(math.MinInt64)
}
