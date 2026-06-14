package cypher_test

// security_audit2026c_test.go — FOURTH security audit (SEC-2026-06-14c) of the
// Cypher query engine. Each test documents one traced finding with a BOUNDED
// repro that never hangs or OOMs the test runner. The findings below have now
// been REMEDIATED, so each test asserts the conforming, SECURE behaviour and
// stands as a permanent regression guard (the prior "contained vulnerable
// behaviour" tolerances have been flipped).
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
// overflow (finding #1492) end-to-end through the engine and asserts the
// conforming, fail-stop behaviour: NO error and the truncated tail 'llo' (the
// Neo4j/openCypher result for an over-large length). FLIPPED after #1492 landed:
// the prior ErrInternalPanic tolerance is gone — a panic is now a hard failure.
func TestSec2026c_Substring_OverflowContained(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const q = `RETURN substring('hello', 2, 9223372036854775807) AS s`

	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("substring overflow: want no error and tail 'llo', got error: %v", err)
	}
	if got := secCypherSingleString(t, res); got != "llo" {
		t.Fatalf("substring overflow: want tail %q, got %q", "llo", got)
	}
}

// TestSec2026c_Substring_OverflowDirectNoPanic locks in the fail-stop
// requirement at the function level: fnSubstring must not panic for any start
// with a huge length. Because fnSubstring is unexported, this exercises the
// behaviour through the engine across several start offsets, asserting the
// process is never crashed (recover catches it) and, once fixed, the correct
// tail is returned.
//
// FLIPPED after #1492: asserts err == nil and the exact tail for each start.
func TestSec2026c_Substring_OverflowVariousStarts(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	cases := []struct {
		query string
		tail  string // expected tail
	}{
		{`RETURN substring('hello', 0, 9223372036854775807) AS s`, "hello"},
		{`RETURN substring('hello', 1, 9223372036854775807) AS s`, "ello"},
		{`RETURN substring('hello', 4, 9223372036854775807) AS s`, "o"},
		{`RETURN substring('hello', 5, 9223372036854775807) AS s`, ""},
	}
	for _, c := range cases {
		res, err := eng.Run(context.Background(), c.query, nil)
		if err != nil {
			t.Fatalf("%s: want no error, got: %v", c.query, err)
		}
		if got := secCypherSingleString(t, res); got != c.tail {
			t.Fatalf("%s: want %q, got %q", c.query, c.tail, got)
		}
	}
}

// TestSec2026c_PercentileCont_NaNContained drives a NaN percentile into
// percentileCont (finding #1493) and asserts the conforming behaviour: a NaN
// percentile is rejected at plan-build time with a NumberOutOfRange-class error
// (NOT ErrInternalPanic, NOT a panic), matching the out-of-range behaviour
// already verified for p=5.0 / p=-3.0. FLIPPED after #1493 landed.
func TestSec2026c_PercentileCont_NaNContained(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const q = `UNWIND [1,2,3,4,5] AS x RETURN percentileCont(x, 0.0/0.0) AS p`

	_, err := eng.Run(context.Background(), q, nil)
	if err == nil {
		t.Fatalf("percentileCont(NaN): want a NumberOutOfRange error, got nil")
	}
	if errors.Is(err, cypher.ErrInternalPanic) {
		t.Fatalf("percentileCont(NaN): got ErrInternalPanic (a contained panic), want a typed NumberOutOfRange error: %v", err)
	}
	if !strings.Contains(err.Error(), "NumberOutOfRange") {
		t.Fatalf("percentileCont(NaN): want a NumberOutOfRange error, got: %v", err)
	}
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
// The inputs are deliberately small enough that even the (former) unbounded path
// stayed within a few hundred MiB (safe for CI) yet demonstrate the
// amplification: 16 KiB × 16 KiB ≈ 256 MiB worst-case output, which trips the
// 1 GiB cap only with the parameters scaled up — so this test pins the SMALLER
// amplification by asserting the worst-case ESTIMATE (not the materialised
// bytes) is rejected. To keep the assertion deterministic and well over the cap
// we size the params so (runeCount($a)+1)*len($b) exceeds 1 GiB while the inputs
// themselves stay tiny (the empty-search worst case is quadratic in the rune
// count, so 64 KiB × 64 KiB ≈ 4 GiB estimate, comfortably over the 1 GiB cap,
// from 128 KiB of total input).
//
// FLIPPED after #1494: asserts a NumberOutOfRange-class error and that no giant
// string is materialised.
func TestSec2026c_Replace_EmptySearchAmplificationBounded(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	const unit = 64 * 1024 // 64 KiB each; worst-case estimate ≈ 4 GiB > 1 GiB cap.
	params := map[string]expr.Value{
		"a": expr.StringValue(strings.Repeat("a", unit)),
		"b": expr.StringValue(strings.Repeat("b", unit)),
	}
	const q = `RETURN replace($a, '', $b) AS r`

	// The budget rejection is raised while materialising the row, so it can
	// surface either from Run or from the result iterator — drain to capture it.
	err := secCypherDrainErr(eng.Run(context.Background(), q, params))
	if err == nil {
		t.Fatalf("replace amplification: want a NumberOutOfRange error, got nil (output materialised unbounded)")
	}
	if errors.Is(err, cypher.ErrInternalPanic) {
		t.Fatalf("replace amplification: got ErrInternalPanic, want a typed NumberOutOfRange budget error: %v", err)
	}
	if !strings.Contains(err.Error(), "NumberOutOfRange") {
		t.Fatalf("replace amplification: want a NumberOutOfRange error, got: %v", err)
	}
}

// secCypherDrainErr returns the first error from a (result, err) pair, draining
// the result iterator so a lazily-raised per-row evaluation error (which
// surfaces from res.Err(), not from Run) is captured. It never materialises row
// values, so an over-budget string is never built.
func secCypherDrainErr(res *cypher.Result, err error) error {
	if err != nil {
		return err
	}
	defer res.Close()
	for res.Next() {
	}
	return res.Err()
}

// TestSec2026c_Replace_LegitimateUnchanged is a POSITIVE lock-in: a normal,
// small replace() (non-empty search, no amplification) is unaffected by the
// #1494 output-size budget.
func TestSec2026c_Replace_LegitimateUnchanged(t *testing.T) {
	t.Parallel()
	eng := secCypherNewEngine(t)

	res, err := eng.Run(context.Background(), `RETURN replace('hello', 'l', 'L') AS r`, nil)
	if err != nil {
		t.Fatalf("replace('hello','l','L'): unexpected error: %v", err)
	}
	if got := secCypherSingleString(t, res); got != "heLLo" {
		t.Fatalf("replace('hello','l','L'): want %q, got %q", "heLLo", got)
	}
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
