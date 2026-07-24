package exec_test

// parallel_aggregate_scan_test.go — determinism + correctness tests for
// ParallelAggregateScan (#2111).
//
// The decisive property is that the morsel-parallel result is BYTE-IDENTICAL to
// the serial EagerAggregation over the same rows, INCLUDING each value's exact
// String() representation, under every worker count and morsel-boundary. The
// oracle is the real serial exec.EagerAggregation (wrapped in the global adapter
// for a group-by-less aggregate), so a divergence in the parallel combine — most
// dangerously in the min/max tie REPRESENTATIVE — surfaces directly. The
// adversarial cases (mixed int/float tie, ±0.0, NaN) are the ones a value-only
// combine would silently mis-handle.

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ─────────────────────────────────────────────────────────────────────────────
// Harness
// ─────────────────────────────────────────────────────────────────────────────

// aggScanFactory returns an AggInputFactory that emits, for the node IDs in a
// morsel, the pre-cooked pre-aggregation rows keyed by node ID (in morsel order).
// It models the planner's per-worker sub-plan (AllNodesScan(morsel) → Project),
// letting a test drive the operator with fully controlled adversarial values.
func aggScanFactory(rowsByID map[graph.NodeID]exec.Row) exec.AggInputFactory {
	return func(ids []graph.NodeID) (exec.Operator, error) {
		rows := make([]exec.Row, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, rowsByID[id])
		}
		return newSliceOperator(rows...), nil
	}
}

// oracleFactory maps a reducer kind to the serial funcs.Aggregator that defines
// the byte-identical reference result.
func oracleFactory(kind exec.AggReducerKind) funcs.AggregatorFactory {
	switch kind {
	case exec.ReduceCountStar:
		return funcs.NewCountStarAgg()
	case exec.ReduceCount:
		return funcs.NewCountAgg()
	case exec.ReduceMin:
		return funcs.NewMinAgg()
	case exec.ReduceMax:
		return funcs.NewMaxAgg()
	default:
		panic("oracleFactory: unknown reducer kind")
	}
}

// serialOracleRows computes the reference result by running the SAME rows through
// the real serial EagerAggregation (global adapter when group-by-less), rendering
// every output value through String().
func serialOracleRows(t *testing.T, allRows []exec.Row, nKeys int, reducers []exec.AggReducerKind) []string {
	t.Helper()
	keyCols := make([]int, nKeys)
	for i := range keyCols {
		keyCols[i] = i
	}
	factories := make([]funcs.AggregatorFactory, len(reducers))
	for i, k := range reducers {
		factories[i] = oracleFactory(k)
	}
	ea, err := exec.NewEagerAggregation(newSliceOperator(allRows...), keyCols, factories, 0)
	if err != nil {
		t.Fatalf("serial oracle NewEagerAggregation: %v", err)
	}
	var op exec.Operator = ea
	if nKeys == 0 {
		op = exec.NewGlobalAggregateAdapter(ea, factories)
	}
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("serial oracle drain: %v", err)
	}
	return renderAggRows(rows)
}

// parallelRows runs ParallelAggregateScan over ids 0..len(allRows)-1 with the given
// morsel size and worker cap, rendering every output value through String().
func parallelRows(t *testing.T, allRows []exec.Row, nKeys int, reducers []exec.AggReducerKind, morselSize, gomaxprocs int) []string {
	t.Helper()
	prev := runtime.GOMAXPROCS(gomaxprocs)
	defer runtime.GOMAXPROCS(prev)

	rowsByID := make(map[graph.NodeID]exec.Row, len(allRows))
	for i, r := range allRows {
		rowsByID[graph.NodeID(i)] = r
	}
	op := exec.NewParallelAggregateScan(buildWalker(len(allRows)), aggScanFactory(rowsByID), nKeys, reducers, morselSize, nil)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("parallel drain (morsel=%d gomaxprocs=%d): %v", morselSize, gomaxprocs, err)
	}
	return renderAggRows(rows)
}

func renderAggRows(rows []exec.Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, v := range r {
			parts[j] = v.String()
		}
		out[i] = strings.Join(parts, "|")
	}
	return out
}

// morselSweep and workerSweep are the partition-boundary and worker-count sweeps
// the determinism obligation (design §7.2) requires. The morsel sizes cross and
// straddle group/tie boundaries; the worker counts range from serial (1) to
// oversubscription (2×GOMAXPROCS).
var (
	morselSweep = []int{1, 2, 3, 5, 7, 13, 64, 1024}
	workerSweep = []int{1, 2, 3, 4, 7, 2 * runtimeGOMAXPROCS()}
)

func runtimeGOMAXPROCS() int { return runtime.GOMAXPROCS(0) }

// assertMatchesSerialAcrossSweeps is the core assertion: for every (morsel, worker)
// pair the parallel result equals the serial oracle, row-for-row and byte-for-byte.
func assertMatchesSerialAcrossSweeps(t *testing.T, allRows []exec.Row, nKeys int, reducers []exec.AggReducerKind) {
	t.Helper()
	want := serialOracleRows(t, allRows, nKeys, reducers)
	for _, ms := range morselSweep {
		for _, w := range workerSweep {
			if w < 1 {
				w = 1
			}
			got := parallelRows(t, allRows, nKeys, reducers, ms, w)
			if len(got) != len(want) {
				t.Fatalf("row-count mismatch (morsel=%d workers=%d): parallel=%d serial=%d\n parallel=%v\n serial=%v",
					ms, w, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d BYTE-DIVERGENCE (morsel=%d workers=%d):\n parallel = %q\n serial   = %q",
						i, ms, w, got[i], want[i])
				}
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Scalar (global) min / max / count
// ─────────────────────────────────────────────────────────────────────────────

// TestParallelAggregateScan_ScalarMatchesSerial sweeps count/min/max over a
// mixed-type dataset and asserts byte-identity to serial across every partition
// and worker count.
func TestParallelAggregateScan_ScalarMatchesSerial(t *testing.T) {
	defer goleak.VerifyNone(t)

	// A dataset with a spread of ints, floats, and strings so the min/max tier
	// ordering (Number tier vs kind weight) is exercised, plus NULLs that count(v)
	// must skip. Column layout: [arg] (nKeys=0, one reducer at a time below).
	vals := []expr.Value{
		expr.IntegerValue(5), expr.FloatValue(2.5), expr.IntegerValue(-3),
		expr.Null, expr.FloatValue(-3.0), expr.IntegerValue(10),
		expr.FloatValue(10.0), expr.StringValue("apple"), expr.IntegerValue(0),
		expr.Null, expr.FloatValue(7.25), expr.IntegerValue(-3),
	}
	build := func(sentinel bool) []exec.Row {
		rows := make([]exec.Row, len(vals))
		for i, v := range vals {
			if sentinel { // count(*) argument column is a constant non-null tick
				rows[i] = exec.Row{expr.BoolValue(true)}
			} else {
				rows[i] = exec.Row{v}
			}
		}
		return rows
	}

	assertMatchesSerialAcrossSweeps(t, build(false), 0, []exec.AggReducerKind{exec.ReduceMin})
	assertMatchesSerialAcrossSweeps(t, build(false), 0, []exec.AggReducerKind{exec.ReduceMax})
	assertMatchesSerialAcrossSweeps(t, build(false), 0, []exec.AggReducerKind{exec.ReduceCount})
	assertMatchesSerialAcrossSweeps(t, build(true), 0, []exec.AggReducerKind{exec.ReduceCountStar})

	// Multi-aggregate in one operator: min, max, count(*), count(v) together. The
	// row carries [arg, sentinel] so reducer 3 (count(v)) reads a NULL-bearing
	// column while count(*) reads the constant tick.
	multi := make([]exec.Row, len(vals))
	for i, v := range vals {
		multi[i] = exec.Row{v, expr.BoolValue(true), v, v} // min(v), count(*), max(v), count(v)... see reducers
	}
	// Reducers read column nKeys+i; with nKeys=0: r0→col0, r1→col1, r2→col2, r3→col3.
	assertMatchesSerialAcrossSweeps(t, multi, 0,
		[]exec.AggReducerKind{exec.ReduceMin, exec.ReduceCountStar, exec.ReduceMax, exec.ReduceCount})
}

// ─────────────────────────────────────────────────────────────────────────────
// The decisive tie-representative cases (design §3, §9)
// ─────────────────────────────────────────────────────────────────────────────

// TestParallelAggregateScan_TieRepresentative proves the position-carrying combine
// reproduces the serial first-seen representative EXACTLY for the tie cases a
// value-only combine would get wrong: a mixed int/float tie whose two members
// render DIFFERENTLY (int 2^53 = "9007199254740992" vs float 2^53 =
// "9.007199254740992e+15", Compare == 0), ±0.0 ("-0" vs "0"), and NaN — with the
// tie at either scan order, across every worker count and morsel boundary.
//
// Each case fixes the tie at the extremum, embeds fillers on the far side, and
// asserts (a) the two tied members render DIFFERENTLY (so the choice is observable
// and the test cannot pass by luck), (b) the serial oracle keeps the first-seen
// member, and (c) the parallel path reproduces the serial representative
// byte-for-byte across the whole sweep. A value-only combine would fail (b)/(c)
// the moment the non-first member won a partition.
func TestParallelAggregateScan_TieRepresentative(t *testing.T) {
	defer goleak.VerifyNone(t)

	big := int64(1) << 53                               // 2^53: int and float render differently
	bigI, bigF := iv(big), fv(float64(big))             // "9007199254740992" vs "9.007199254740992e+15"
	negZero, posZero := fv(math.Copysign(0, -1)), fv(0) // "-0" vs "0"
	nan1 := fv(math.Float64frombits(0x7FF8000000000001))
	nan2 := fv(math.Float64frombits(0x7FF8000000000002))

	cases := []struct {
		name              string
		vals              []expr.Value
		reducer           exec.AggReducerKind
		firstSeen         expr.Value // the tied member at the lowest scan index
		secondSeen        expr.Value // the other tied member
		distinguavailable bool       // whether firstSeen/secondSeen render differently
	}{
		{
			// min tie, float first: float 2^53 (id 0) before int 2^53 (id 2).
			name:              "min-float-before-int",
			vals:              []expr.Value{bigF, iv(big + 5), bigI, iv(big + 9)},
			reducer:           exec.ReduceMin,
			firstSeen:         bigF,
			secondSeen:        bigI,
			distinguavailable: true,
		},
		{
			// min tie, int first: int 2^53 (id 0) before float 2^53 (id 2).
			name:              "min-int-before-float",
			vals:              []expr.Value{bigI, iv(big + 5), bigF, iv(big + 9)},
			reducer:           exec.ReduceMin,
			firstSeen:         bigI,
			secondSeen:        bigF,
			distinguavailable: true,
		},
		{
			// max tie, int first: int 2^53 (id 0) before float 2^53 (id 2).
			name:              "max-int-before-float",
			vals:              []expr.Value{bigI, iv(big - 5), bigF, iv(big - 9)},
			reducer:           exec.ReduceMax,
			firstSeen:         bigI,
			secondSeen:        bigF,
			distinguavailable: true,
		},
		{
			// max tie, float first: float 2^53 (id 0) before int 2^53 (id 2).
			name:              "max-float-before-int",
			vals:              []expr.Value{bigF, iv(big - 5), bigI, iv(big - 9)},
			reducer:           exec.ReduceMax,
			firstSeen:         bigF,
			secondSeen:        bigI,
			distinguavailable: true,
		},
		{
			// ±0.0 minimum, -0.0 first.
			name:              "min-neg-zero-first",
			vals:              []expr.Value{negZero, fv(3), posZero, fv(5)},
			reducer:           exec.ReduceMin,
			firstSeen:         negZero,
			secondSeen:        posZero,
			distinguavailable: true,
		},
		{
			// ±0.0 minimum, +0.0 first.
			name:              "min-pos-zero-first",
			vals:              []expr.Value{posZero, fv(3), negZero, fv(5)},
			reducer:           exec.ReduceMin,
			firstSeen:         posZero,
			secondSeen:        negZero,
			distinguavailable: true,
		},
		{
			// NaN is the maximum (sorts last); the first NaN payload seen is kept.
			// Both payloads render "NaN", so this proves PLACEMENT, not byte choice.
			name:              "max-nan-placement",
			vals:              []expr.Value{iv(5), nan1, iv(3), nan2, iv(9)},
			reducer:           exec.ReduceMax,
			firstSeen:         nan1,
			secondSeen:        nan2,
			distinguavailable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.distinguavailable && tc.firstSeen.String() == tc.secondSeen.String() {
				t.Fatalf("test is not meaningful: tied members render identically (%q)", tc.firstSeen.String())
			}
			rows := make([]exec.Row, len(tc.vals))
			for i, v := range tc.vals {
				rows[i] = exec.Row{v}
			}
			reducers := []exec.AggReducerKind{tc.reducer}

			// The serial oracle must itself keep the first-seen member — this pins
			// the expectation to real serial behaviour, not the test's belief.
			want := serialOracleRows(t, rows, 0, reducers)
			if len(want) != 1 || want[0] != tc.firstSeen.String() {
				t.Fatalf("serial oracle representative = %q, want first-seen %q", want, tc.firstSeen.String())
			}
			// And the parallel path reproduces it byte-for-byte across every sweep.
			assertMatchesSerialAcrossSweeps(t, rows, 0, reducers)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Group-by min / max / count
// ─────────────────────────────────────────────────────────────────────────────

// TestParallelAggregateScan_GroupByMatchesSerial proves the grouped forms are
// byte-identical to serial INCLUDING the per-group tie representative, the group
// membership (int/float grouping-key collisions resolved identically), and the
// emission ORDER (ascending first-seen position = serial insertion order).
func TestParallelAggregateScan_GroupByMatchesSerial(t *testing.T) {
	defer goleak.VerifyNone(t)

	// Rows: [groupKey, arg, sentinel]. Two groups keyed "a"/"b" interleaved so the
	// first-seen order is deterministic (group "a" first at id 0). Each group holds
	// a mixed int/float tie at its extremum so the representative is load-bearing.
	rows := []exec.Row{
		{sv("a"), fv(1.0), tick()},  // a: min tie 1.0 (first)
		{sv("b"), iv(20), tick()},   // b first-seen at id 1
		{sv("a"), iv(1), tick()},    // a: min tie 1 (later)
		{sv("b"), fv(20.0), tick()}, // b: max tie 20 (int first at id1) vs 20.0
		{sv("a"), iv(9), tick()},
		{sv("b"), iv(5), tick()},
		{sv("a"), iv(1), tick()}, // another 1 in a
	}
	// Group-by min(arg): keyCol 0, reducer over col1.
	assertMatchesSerialAcrossSweeps(t, rows, 1, []exec.AggReducerKind{exec.ReduceMin})
	// Group-by max(arg).
	assertMatchesSerialAcrossSweeps(t, rows, 1, []exec.AggReducerKind{exec.ReduceMax})
	// Group-by count(*): reads col nKeys+0 = col1 (arg) for count(v) or the tick for
	// count(*); use the tick column layout by pointing a countStar reducer at col1.
	assertMatchesSerialAcrossSweeps(t, rows, 1, []exec.AggReducerKind{exec.ReduceCountStar})
	// Group-by min, max, count(*) together: cols 1,2 hold arg and tick; reducer 0
	// (min) → col1, reducer 1 (max) → col2 (tick) — so build a dedicated layout.
	multi := make([]exec.Row, len(rows))
	for i, r := range rows {
		multi[i] = exec.Row{r[0], r[1], r[1], tick()} // key, min-arg, max-arg, count-tick
	}
	assertMatchesSerialAcrossSweeps(t, multi, 1,
		[]exec.AggReducerKind{exec.ReduceMin, exec.ReduceMax, exec.ReduceCountStar})
}

// TestParallelAggregateScan_GroupByIntFloatKeyCollision proves grouping keys that
// share a float64 bucket but are NOT equivalent (int 2^53+1 vs float 2^53) form
// SEPARATE groups, identically to serial — the collision resolution uses the exact
// grouping comparator, not the hash alone.
func TestParallelAggregateScan_GroupByIntFloatKeyCollision(t *testing.T) {
	defer goleak.VerifyNone(t)

	big := int64(1) << 53
	rows := []exec.Row{
		{iv(big + 1), iv(1), tick()},
		{fv(float64(big)), iv(2), tick()},
		{iv(big + 1), iv(3), tick()},
		{fv(float64(big)), iv(4), tick()},
	}
	assertMatchesSerialAcrossSweeps(t, rows, 1, []exec.AggReducerKind{exec.ReduceCountStar})
	assertMatchesSerialAcrossSweeps(t, rows, 1, []exec.AggReducerKind{exec.ReduceMin})
}

// ─────────────────────────────────────────────────────────────────────────────
// Lifecycle: empty, cancellation, race, memory cap, same-seed reproduction
// ─────────────────────────────────────────────────────────────────────────────

// TestParallelAggregateScan_EmptyInput proves a global aggregate over zero nodes
// emits one neutral row (count 0, min/max NULL) and a grouped aggregate emits zero
// rows — matching serial — with no workers spawned and no goroutine leak.
func TestParallelAggregateScan_EmptyInput(t *testing.T) {
	defer goleak.VerifyNone(t)

	// Global: one neutral row.
	op := exec.NewParallelAggregateScan(&staticNodeWalker{}, aggScanFactory(nil), 0,
		[]exec.AggReducerKind{exec.ReduceCountStar, exec.ReduceMin}, exec.DefaultMorselSize, nil)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	got := renderAggRows(rows)
	want := []string{"0|null"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("empty global aggregate = %v, want %v", got, want)
	}

	// Group-by: zero rows.
	op2 := exec.NewParallelAggregateScan(&staticNodeWalker{}, aggScanFactory(nil), 1,
		[]exec.AggReducerKind{exec.ReduceMin}, exec.DefaultMorselSize, nil)
	rows2, err := exec.Drain(context.Background(), op2)
	if err != nil {
		t.Fatalf("drain group-by empty: %v", err)
	}
	if len(rows2) != 0 {
		t.Fatalf("empty grouped aggregate = %d rows, want 0", len(rows2))
	}
}

// TestParallelAggregateScan_SameSeedReproduction runs the same adversarial input
// many times and asserts the rendered result never varies — an exact same-input
// reproduction independent of scheduler interleaving.
func TestParallelAggregateScan_SameSeedReproduction(t *testing.T) {
	defer goleak.VerifyNone(t)

	rows := []exec.Row{
		{sv("a"), fv(1.0)}, {sv("b"), iv(1)}, {sv("a"), iv(1)},
		{sv("b"), fv(1.0)}, {sv("a"), iv(9)}, {sv("b"), iv(5)},
	}
	reducers := []exec.AggReducerKind{exec.ReduceMin}
	first := parallelRows(t, rows, 1, reducers, 3, runtimeGOMAXPROCS())
	for run := 0; run < 40; run++ {
		got := parallelRows(t, rows, 1, reducers, 3, runtimeGOMAXPROCS())
		if len(got) != len(first) {
			t.Fatalf("run %d row-count drift: %d vs %d", run, len(got), len(first))
		}
		for i := range first {
			if got[i] != first[i] {
				t.Fatalf("run %d NON-REPRODUCIBLE row %d: %q vs %q", run, i, got[i], first[i])
			}
		}
	}
}

// TestParallelAggregateScan_Cancellation proves the operator returns promptly on
// cancellation and leaks no goroutine.
func TestParallelAggregateScan_Cancellation(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 1_000_000
	rowsByID := make(map[graph.NodeID]exec.Row, n)
	for i := 0; i < n; i++ {
		rowsByID[graph.NodeID(i)] = exec.Row{expr.IntegerValue(int64(i))}
	}
	op := exec.NewParallelAggregateScan(buildWalker(n), aggScanFactory(rowsByID), 0,
		[]exec.AggReducerKind{exec.ReduceMin}, 64, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := exec.Drain(ctx, op)
		done <- err
	}()
	time.Sleep(2 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ParallelAggregateScan did not return within 500ms after cancellation")
	}
}

// TestParallelAggregateScan_CloseWithoutNext exercises the never-drained teardown:
// workers spawned in Init must be cancelled and joined by Close (no leak).
func TestParallelAggregateScan_CloseWithoutNext(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 200_000
	rowsByID := make(map[graph.NodeID]exec.Row, n)
	for i := 0; i < n; i++ {
		rowsByID[graph.NodeID(i)] = exec.Row{expr.IntegerValue(int64(i % 7))}
	}
	op := exec.NewParallelAggregateScan(buildWalker(n), aggScanFactory(rowsByID), 0,
		[]exec.AggReducerKind{exec.ReduceMax}, 16, nil)
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestParallelAggregateScan_RaceClean runs independent instances concurrently; the
// race detector must find nothing, and every instance must agree with serial.
func TestParallelAggregateScan_RaceClean(t *testing.T) {
	rows := make([]exec.Row, 3000)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(i % 97)), expr.IntegerValue(int64(i))}
	}
	want := serialOracleRows(t, rows, 1, []exec.AggReducerKind{exec.ReduceMax})

	const goroutines = 8
	results := make(chan []string, goroutines)
	rowsByID := make(map[graph.NodeID]exec.Row, len(rows))
	for i, r := range rows {
		rowsByID[graph.NodeID(i)] = r
	}
	for range goroutines {
		go func() {
			op := exec.NewParallelAggregateScan(buildWalker(len(rows)), aggScanFactory(rowsByID), 1,
				[]exec.AggReducerKind{exec.ReduceMax}, 64, nil)
			got, err := exec.Drain(context.Background(), op)
			if err != nil {
				results <- []string{fmt.Sprintf("error: %v", err)}
				return
			}
			results <- renderAggRows(got)
		}()
	}
	for range goroutines {
		got := <-results
		if len(got) != len(want) {
			t.Errorf("concurrent instance row-count = %d, want %d", len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("concurrent instance row %d = %q, want %q", i, got[i], want[i])
			}
		}
	}
}

// TestParallelAggregateScan_GroupCap proves the per-worker + merged group-count cap
// trips ErrAggMemoryExceeded when the distinct group count exceeds the limit — the
// bounded-memory guard the group-by parallel path requires (design §3.3).
func TestParallelAggregateScan_GroupCap(t *testing.T) {
	defer goleak.VerifyNone(t)

	const n = 5000
	rowsByID := make(map[graph.NodeID]exec.Row, n)
	for i := 0; i < n; i++ {
		rowsByID[graph.NodeID(i)] = exec.Row{expr.IntegerValue(int64(i)), expr.IntegerValue(int64(i))}
	}
	// maxGroups = 10 but n distinct keys ⇒ cap must trip.
	op := exec.NewParallelAggregateScan(buildWalker(n), aggScanFactory(rowsByID), 1,
		[]exec.AggReducerKind{exec.ReduceMin}, 64, nil).WithGroupCap(10)
	_, err := exec.Drain(context.Background(), op)
	if err == nil {
		t.Fatal("expected ErrAggMemoryExceeded with maxGroups=10 over n distinct keys, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// tiny value constructors (iv/fv/sv are shared with agg_column_kernel_test.go)
// ─────────────────────────────────────────────────────────────────────────────

func tick() expr.Value { return expr.BoolValue(true) }
