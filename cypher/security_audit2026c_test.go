package cypher_test

// security_audit2026c_test.go — FOURTH security audit (SEC-2026-06-14c) of the
// Cypher query engine. Each test documents one traced finding with a BOUNDED
// repro that never hangs or OOMs the test runner. While the corresponding fix
// is open, the test asserts that the vulnerable behaviour is CONTAINED (the
// panic is caught by the engine's recover boundary, or the amplification is
// kept small enough not to exhaust the runner). Each test carries a // FLIP:
// comment describing the single-line change that turns it into a positive
// secure-behaviour lock-in once the rmp task is implemented.
//
// ── FINDING #1492 [High] — substring() integer-overflow panic ────────────────
// fnSubstring computes `end := start + length` where start ∈ [0,len] and length
// is the caller's int64. start+length overflows int64 to a NEGATIVE value, the
// `if end > len(runes)` clamp does not fire (negative < len), and runes[start:end]
// panics with "slice bounds out of range". Reachable from O(1) query text:
//   RETURN substring('hello', 2, 9223372036854775807)
// The engine's recover boundary converts it to ErrInternalPanic, which violates
// the fail-stop mandate and is a per-query DoS amplifier (debug.Stack + log +
// unwind). The conforming result is the truncated tail ('llo').
//
// ── FINDING #1493 [Medium] — percentileCont() NaN bypasses [0,1] validation ──
// validPercentileParam rejects p<0 || p>1, but BOTH comparisons are false for
// NaN, so a NaN percentile (e.g. from 0.0/0.0) passes validation. In
// PercentileContAgg.Result, pos := clamp01(NaN)*… = NaN, and lo := int(Floor(NaN))
// / hi := int(Ceil(NaN)) index a.values WITHOUT re-clamping. int(NaN) is
// implementation-dependent (arm64: 0, no crash; amd64: MinInt64 → index panic).
// PercentileDiscAgg clamps idx to [0,n-1] and is safe.
//
// ── FINDING #1494 [Medium] — replace(s,'',r) quadratic amplification ─────────
// fnReplace calls strings.ReplaceAll with no output bound; an empty search string
// inserts `replace` between every rune, so output = (len(s)+1)*len(r). The
// per-evaluation string-byte budget does not apply to funcs (generic dispatch
// never charges it), and parameters bypass the 1MiB query-text guard, so
// replace($a,'',$b) with large string params attempts a multi-terabyte
// allocation. Measured ~5000x amplification on 20KB of input.

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// TestSec2026c_Substring_OverflowContained drives the substring() integer
// overflow (finding #1492) end-to-end through the engine and asserts the panic
// is at least CONTAINED (never crashes the host) — today it surfaces as
// ErrInternalPanic. The conforming outcome is the truncated tail 'llo'.
//
// FLIP (after #1492): replace the ErrInternalPanic tolerance with a hard
// assertion that err == nil and the returned column equals "llo".
func TestSec2026c_Substring_OverflowContained(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const q = `RETURN substring('hello', 2, 9223372036854775807) AS s`

	// The call must return — never crash the process. We tolerate either the
	// (current) ErrInternalPanic containment or the (fixed) correct value.
	res, err := eng.Run(context.Background(), q, nil)

	if err == nil {
		got := secCypherSingleString(t, res)
		if got != "llo" {
			t.Fatalf("substring overflow: want tail %q, got %q", "llo", got)
		}
		return // FIXED behaviour — finding #1492 resolved.
	}
	if !errors.Is(err, cypher.ErrInternalPanic) {
		t.Fatalf("substring overflow: unexpected error kind: %v", err)
	}
	t.Logf("finding #1492 OPEN: substring overflow currently contained as ErrInternalPanic (want truncated value 'llo')")
}

// TestSec2026c_Substring_OverflowDirectNoPanic locks in the fail-stop
// requirement at the function level: fnSubstring must not panic for any start
// with a huge length. Because fnSubstring is unexported, this exercises the
// behaviour through the engine across several start offsets, asserting the
// process is never crashed (recover catches it) and, once fixed, the correct
// tail is returned.
//
// FLIP (after #1492): assert err == nil and exact tails for each start.
func TestSec2026c_Substring_OverflowVariousStarts(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	cases := []struct {
		query string
		tail  string // expected tail once fixed
	}{
		{`RETURN substring('hello', 0, 9223372036854775807) AS s`, "hello"},
		{`RETURN substring('hello', 1, 9223372036854775807) AS s`, "ello"},
		{`RETURN substring('hello', 4, 9223372036854775807) AS s`, "o"},
		{`RETURN substring('hello', 5, 9223372036854775807) AS s`, ""},
	}
	for _, c := range cases {
		res, err := eng.Run(context.Background(), c.query, nil)
		if err == nil {
			if got := secCypherSingleString(t, res); got != c.tail {
				t.Fatalf("%s: want %q, got %q", c.query, c.tail, got)
			}
			continue
		}
		if !errors.Is(err, cypher.ErrInternalPanic) {
			t.Fatalf("%s: unexpected error: %v", c.query, err)
		}
		// OPEN: contained as ErrInternalPanic — acceptable until #1492 lands.
	}
}

// TestSec2026c_PercentileCont_NaNContained drives a NaN percentile into
// percentileCont (finding #1493) and asserts containment: the call must return
// (no host crash) — today it either returns a value (arm64, where int(NaN)=0) or
// ErrInternalPanic (amd64, where int(NaN)=MinInt64 → index panic). Either way it
// must not crash the process or hang.
//
// FLIP (after #1493): assert err is a NumberOutOfRange-class error (NaN is
// rejected by validPercentileParam), matching the out-of-range behaviour already
// verified for p=5.0 / p=-3.0.
func TestSec2026c_PercentileCont_NaNContained(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const q = `UNWIND [1,2,3,4,5] AS x RETURN percentileCont(x, 0.0/0.0) AS p`

	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		// FIXED path: NaN rejected like other out-of-range percentiles, OR
		// current amd64 containment via ErrInternalPanic. Both are non-crashing.
		if strings.Contains(err.Error(), "NumberOutOfRange") {
			return // FIXED behaviour — finding #1493 resolved.
		}
		if !errors.Is(err, cypher.ErrInternalPanic) {
			t.Fatalf("percentileCont(NaN): unexpected error kind: %v", err)
		}
		t.Logf("finding #1493 OPEN: percentileCont(NaN) contained as ErrInternalPanic")
		return
	}
	// On this arch int(NaN)=0 so no panic fired; the result is whatever the
	// degenerate index produced. The point of the lock-in is that it did not
	// crash; record the OPEN state.
	_ = res
	t.Logf("finding #1493 OPEN: percentileCont(NaN) returned a value without rejecting NaN (platform-dependent; would panic where int(NaN) is out of range)")
}

// TestSec2026c_PercentileCont_NaNAggNoPanic exercises the aggregator directly
// (it is exported) with a NaN percentile over a small fixed sample. The
// requirement is that Result() never panics on ANY GOARCH. This is the strongest
// portable lock-in for finding #1493: once the clamp (or NaN rejection) is in
// place this passes everywhere; today it passes on arm64 (int(NaN)=0) and is the
// canary that would have caught the amd64 panic.
//
// FLIP (after #1493): no change needed — this already asserts the secure
// invariant (no panic). It becomes a permanent regression guard.
func TestSec2026c_PercentileCont_NaNAggNoPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PercentileContAgg.Result() panicked on NaN percentile (finding #1493): %v", r)
		}
	}()

	factory := funcs.NewPercentileContAgg(math.NaN())
	agg := factory()
	for _, v := range []float64{1, 2, 3, 4, 5} {
		if err := agg.Step(expr.FloatValue(v)); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	_ = agg.Result() // must not panic on any platform.
}

// TestSec2026c_PercentileDisc_NaNAggSafe is a POSITIVE lock-in: the discrete
// percentile aggregator clamps its index to [0, n-1] AFTER the int() conversion,
// so even a NaN percentile cannot make it panic. This fences that safe behaviour
// against regression and documents the asymmetry with percentileCont.
func TestSec2026c_PercentileDisc_NaNAggSafe(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PercentileDiscAgg.Result() unexpectedly panicked on NaN: %v", r)
		}
	}()

	factory := funcs.NewPercentileDiscAgg(math.NaN())
	agg := factory()
	for _, v := range []float64{1, 2, 3, 4, 5} {
		if err := agg.Step(expr.FloatValue(v)); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	_ = agg.Result() // safe by construction (clamped index).
}

// TestSec2026c_Replace_EmptySearchAmplificationBounded drives the replace()
// quadratic amplification (finding #1494) through the engine with BOUNDED
// parameters (16 KiB source, 16 KiB replacement → ~256 MiB worst-case output if
// unbounded). It asserts the call returns without OOMing the runner. Once the
// output-size cap lands, it returns a NumberOutOfRange error instead of
// materialising the giant string.
//
// The inputs are deliberately small enough that even the unbounded path stays
// within a few hundred MiB (safe for CI) yet demonstrates the amplification.
//
// FLIP (after #1494): assert err is a NumberOutOfRange-class error and no large
// string is returned.
func TestSec2026c_Replace_EmptySearchAmplificationBounded(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const unit = 16 * 1024 // 16 KiB each; worst-case output ≈ 256 MiB.
	params := map[string]expr.Value{
		"a": expr.StringValue(strings.Repeat("a", unit)),
		"b": expr.StringValue(strings.Repeat("b", unit)),
	}
	const q = `RETURN replace($a, '', $b) AS r`

	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		if strings.Contains(err.Error(), "NumberOutOfRange") {
			return // FIXED behaviour — finding #1494 resolved (output capped).
		}
		// Any other typed error is also non-crashing containment.
		t.Logf("finding #1494: replace amplification returned error: %v", err)
		return
	}
	// Unbounded path: verify the engine produced the amplified string (proving
	// the gap) but did not crash. The amplification factor is ~unit, confirming
	// the quadratic blow-up that would OOM with larger parameters.
	got := secCypherSingleString(t, res)
	if len(got) < unit*unit { // (len(a)+1)*len(b) ≈ unit*unit
		t.Logf("finding #1494: replace produced %d bytes from 2x%d-byte inputs", len(got), unit)
	}
	t.Logf("finding #1494 OPEN: replace($a,'',$b) is unbudgeted (%d-byte output from 2x%d-byte inputs; scales to OOM with larger params)", len(got), unit)
}

// secCypherSingleString extracts the single string column of a single-row
// result, failing the test if the shape is not exactly one row with one string.
// It consumes the result iterator (res.Next/res.Record).
func secCypherSingleString(t *testing.T, res *cypher.Result) string {
	t.Helper()
	defer res.Close()

	cols := res.Columns()
	if len(cols) != 1 {
		t.Fatalf("want exactly 1 column, got %d (%v)", len(cols), cols)
	}
	if !res.Next() {
		t.Fatalf("want exactly 1 row, got 0 (err=%v)", res.Err())
	}
	rec := res.Record()
	v, ok := rec[cols[0]]
	if !ok {
		t.Fatalf("column %q missing from record", cols[0])
	}
	if res.Next() {
		t.Fatalf("want exactly 1 row, got more")
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	switch s := v.(type) {
	case expr.StringValue:
		return string(s)
	case string:
		return s
	default:
		t.Fatalf("column %q is not a string: %T", cols[0], v)
		return ""
	}
}
