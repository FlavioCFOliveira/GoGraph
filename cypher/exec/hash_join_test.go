package exec

// hash_join_test.go — unit tests for the HashJoin operator (#1506).

import (
	"context"
	"math"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// sliceSource is a trivial Operator emitting a fixed sequence of rows. Each Init
// rewinds to the start so the operator can be re-driven (the HashJoin drains the
// build side once and the probe side once, but Apply-style re-init must be safe).
type sliceSource struct {
	rows []Row
	idx  int
	ctx  context.Context //nolint:containedctx // test stub
}

func (s *sliceSource) Init(ctx context.Context) error { s.ctx = ctx; s.idx = 0; return nil }
func (s *sliceSource) Next(out *Row) (bool, error) {
	if err := s.ctx.Err(); err != nil {
		return false, err
	}
	if s.idx >= len(s.rows) {
		return false, nil
	}
	*out = s.rows[s.idx]
	s.idx++
	return true, nil
}
func (s *sliceSource) Close() error { return nil }

// keyCol returns a KeyFn that reads the value at column c.
func keyCol(c int) KeyFn {
	return func(row Row) (expr.Value, error) {
		if c < len(row) {
			return row[c], nil
		}
		return expr.Null, nil
	}
}

// drainJoin runs the join to completion and returns each output row rendered as
// a stable string for multiset comparison.
func drainJoin(t *testing.T, hj *HashJoin) []string {
	t.Helper()
	if err := hj.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out []string
	for {
		var r Row
		ok, err := hj.Next(&r)
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
	if err := hj.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func iv(n int64) expr.Value   { return expr.IntegerValue(n) }
func fv(f float64) expr.Value { return expr.FloatValue(f) }
func sv(s string) expr.Value  { return expr.StringValue(s) }

func TestHashJoin_BasicEquiJoin(t *testing.T) {
	// probe: rows keyed 1,2,3 at col 0; build: rows keyed 2,3,3 at col 0.
	probe := &sliceSource{rows: []Row{{iv(1)}, {iv(2)}, {iv(3)}}}
	build := &sliceSource{rows: []Row{{iv(2)}, {iv(3)}, {iv(3)}}}
	// buildOnLeft=false → output is probe||build.
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	got := drainJoin(t, hj)
	// 2 matches: probe 2 × build {2} = 1; probe 3 × build {3,3} = 2. Total 3.
	want := []string{"2,2", "3,3", "3,3"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %d rows %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestHashJoin_NullKeyMatchesNothing(t *testing.T) {
	probe := &sliceSource{rows: []Row{{iv(1)}, {expr.Null}, {iv(2)}}}
	build := &sliceSource{rows: []Row{{iv(1)}, {expr.Null}, {iv(2)}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	got := drainJoin(t, hj)
	// NULL keys excluded on both sides; only 1↔1 and 2↔2 match.
	want := []string{"1,1", "2,2"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestHashJoin_NaNKeyMatchesNothing(t *testing.T) {
	probe := &sliceSource{rows: []Row{{fv(math.NaN())}, {fv(1.0)}}}
	build := &sliceSource{rows: []Row{{fv(math.NaN())}, {fv(1.0)}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	got := drainJoin(t, hj)
	// NaN never matches; only 1.0 ↔ 1.0.
	if len(got) != 1 || got[0] != "1,1" {
		t.Fatalf("got %v, want [1,1]", got)
	}
}

func TestHashJoin_CrossTypeNumericMatches(t *testing.T) {
	// integer 1 must match float 1.0 (openCypher numeric equality).
	probe := &sliceSource{rows: []Row{{iv(1)}, {iv(2)}, {iv(3)}}}
	build := &sliceSource{rows: []Row{{fv(1.0)}, {fv(2.0)}, {fv(99.0)}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	got := drainJoin(t, hj)
	want := []string{"1,1", "2,2"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestHashJoin_StringNeverMatchesNumber(t *testing.T) {
	probe := &sliceSource{rows: []Row{{iv(1)}, {iv(2)}}}
	build := &sliceSource{rows: []Row{{sv("1")}, {sv("2")}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	got := drainJoin(t, hj)
	if len(got) != 0 {
		t.Fatalf("expected no matches between numbers and numeric strings, got %v", got)
	}
}

func TestHashJoin_EmptyBuildSide(t *testing.T) {
	probe := &sliceSource{rows: []Row{{iv(1)}, {iv(2)}}}
	build := &sliceSource{rows: nil}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	got := drainJoin(t, hj)
	if len(got) != 0 {
		t.Fatalf("empty build side must yield no rows, got %v", got)
	}
}

func TestHashJoin_BuildOnLeftColumnOrder(t *testing.T) {
	// With buildOnLeft=true the output is build||probe. Use distinct payload
	// columns to observe the order.
	probe := &sliceSource{rows: []Row{{iv(1), sv("P")}}}
	build := &sliceSource{rows: []Row{{iv(1), sv("B")}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), true)
	got := drainJoin(t, hj)
	// build row (1,"B") || probe row (1,"P") = 1,"B",1,"P"
	if len(got) != 1 || got[0] != `1,"B",1,"P"` {
		t.Fatalf("got %v, want [1,\"B\",1,\"P\"]", got)
	}
}

func TestHashJoin_Cancellation(t *testing.T) {
	probe := &sliceSource{rows: []Row{{iv(1)}}}
	build := &sliceSource{rows: []Row{{iv(1)}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	ctx, cancel := context.WithCancel(context.Background())
	if err := hj.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	var r Row
	if _, err := hj.Next(&r); err == nil {
		t.Fatal("expected cancellation error from Next after cancel")
	}
	_ = hj.Close()
}

func TestCanonicalKeyHash_IntFloatAgree(t *testing.T) {
	if canonicalKeyHash(iv(5)) != canonicalKeyHash(fv(5.0)) {
		t.Fatal("integer 5 and float 5.0 must hash to the same bucket")
	}
	if canonicalKeyHash(iv(-3)) != canonicalKeyHash(fv(-3.0)) {
		t.Fatal("integer -3 and float -3.0 must hash to the same bucket")
	}
	// A non-integral float keeps its native hash (no folding).
	if canonicalKeyHash(fv(2.5)) == canonicalKeyHash(iv(2)) {
		t.Fatal("2.5 must not collide with integer 2 by construction")
	}
}

// TestCanonicalKeyHash_LargeIntFloatAgree is the regression gate for rmp
// #1865/#1868: an independent, opposite-direction cross-type fold
// (float→integer, converting an integral float to an int64 and hashing
// THAT) disagreed with expr.EquivalentHash's integer→float direction for any
// pair where float64 rounding makes an out-of-int64-mantissa-range integer
// and a float collapse to the same float64 value — exactly the pair
// expr.Value.Equal itself treats as equal (it compares via
// float64(a)==float64(b)). Before the fix this silently dropped a matching
// row from the join output instead of erroring.
func TestCanonicalKeyHash_LargeIntFloatAgree(t *testing.T) {
	// 2^53+1 is not exactly representable in float64; it rounds to 2^53, the
	// same value float64(2^53) already has — so Equal treats the pair as
	// equal, and canonicalKeyHash must agree.
	const big = int64(1) << 53
	huge := expr.IntegerValue(big + 1)
	rounded := expr.FloatValue(float64(big))
	if !toBoolValue(t, huge.Equal(rounded)) {
		t.Fatalf("precondition failed: Equal(%v, %v) = false, want true (float64 rounding collapses both to %g)", huge, rounded, float64(big))
	}
	if canonicalKeyHash(huge) != canonicalKeyHash(rounded) {
		t.Fatalf("canonicalKeyHash(%v)=%d != canonicalKeyHash(%v)=%d, but Equal says they match — a HashJoin would silently drop this row",
			huge, canonicalKeyHash(huge), rounded, canonicalKeyHash(rounded))
	}
}

// TestHashJoin_LargeIntFloatMatches drives the same scenario through the
// real HashJoin operator end-to-end, proving the fix closes the actual
// silently-wrong-result bug, not just the unit-level hash disagreement.
func TestHashJoin_LargeIntFloatMatches(t *testing.T) {
	const big = int64(1) << 53
	probe := &sliceSource{rows: []Row{{expr.IntegerValue(big + 1)}}}
	build := &sliceSource{rows: []Row{{expr.FloatValue(float64(big))}}}
	hj := NewHashJoin(build, probe, keyCol(0), keyCol(0), false)
	got := drainJoin(t, hj)
	if len(got) != 1 {
		t.Fatalf("HashJoin(2^53+1, float64(2^53)) produced %d rows, want 1 — Equal treats them as equal (both round to the same float64), so the join must not silently drop this match", len(got))
	}
}

// toBoolValue unwraps an expr.Value known to be a BoolValue (as Equal always
// returns for two non-null operands), failing the test on any other shape.
func toBoolValue(t *testing.T, v expr.Value) bool {
	t.Helper()
	b, ok := v.(expr.BoolValue)
	if !ok {
		t.Fatalf("expected BoolValue, got %T (%v)", v, v)
	}
	return bool(b)
}

func TestIsUnjoinableKey(t *testing.T) {
	cases := []struct {
		v    expr.Value
		want bool
	}{
		{expr.Null, true},
		{fv(math.NaN()), true},
		{iv(0), false},
		{fv(0.0), false},
		{sv(""), false},
	}
	for _, c := range cases {
		if got := isUnjoinableKey(c.v); got != c.want {
			t.Errorf("isUnjoinableKey(%v) = %v, want %v", c.v, got, c.want)
		}
	}
}
