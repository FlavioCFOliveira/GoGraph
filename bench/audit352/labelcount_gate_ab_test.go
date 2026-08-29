package audit352_test

// labelcount_gate_ab_test.go — forward regression instrument for the
// constant-time labelled count pushdown (rmp #2654), plus the per-row boxing
// finding from the sprint 352 audit.
//
// # What this file is now
//
// It began as an A/B harness that measured the cost of the size gate on
// tryBuildLabelCountScan. That gate is GONE, so the A/B is gone with it: the new
// disable seam (buildOpts.labelCountPushdownDisabled) is unexported and has no
// EngineOptions counterpart by design, so an external package can no longer build
// the slow arm at all, and both arms of the old sweep would now measure the same
// program. The behavioural inverses of the fix live in-package, where they can
// read labelCountScanBuildCount, in cypher/label_count_pushdown_gate_test.go.
//
// What is left here is what an external instrument can still do that the
// in-package tests do not: assert the PROPERTY the fix bought — that a bare
// labelled count is constant in time, in bytes and in allocations as the graph
// grows — and keep measuring the per-row boxing that the audit found underneath
// it.
//
// # Historical numbers (pre-fix, reproducible ONLY at commit a0e5a990)
//
// These coefficients were measured on the defective build: Apple M4, 10 cores,
// macOS 26.5.2, go1.27.0 darwin/arm64, 10 interleaved passes, benchstat n=10, on
// graphs of 1 000 / 10 000 / 50 000 / 60 000 / 100 000 nodes all carrying one
// label. They CANNOT be reproduced on the current tree, because the arm that
// produced them can no longer be constructed from outside package cypher.
//
//	SERIAL pipeline (the gate declining), a 100x range in n:
//	  time    ns     = 586.6   + 26.5631 * n     R2 = 0.999963
//	  allocs  allocs = -185.2  +  1.0000 * n     R2 = 1.000000   (one box per node)
//	  bytes   B      = 62675.2 +  8.2069 * n     R2 = 0.998890   (8 bytes per node)
//	  log-log exponent k = 0.9728 (R2 = 0.999881) — Theta(n)
//
//	LabelCountScan pushdown:
//	  time    ns     = 1420.8  +  0.0000 * n     R2 = 0.05 (no relationship)
//	  allocs  29 constant, B 2168 constant — ZERO variance over the whole range
//	  log-log exponent k = -0.0001 — Theta(1)
//
//	Headline at n = 50 000 (exactly AT the deleted gate's strict-> boundary):
//	  1 316 829 -> 1 418 ns/op   (929x)
//	    477 944 ->  2 168 B/op   (-99.55%)
//	     49 815 ->     29 allocs/op (-99.94%)
//	  The same query was 927x FASTER on a 20% BIGGER graph (50k -> 60k), because
//	  60 000 crossed the threshold and 50 000 did not.
//
//	EngineOptions.DisableParallelScan, which then also forfeited the O(1) count:
//	  n =  60 000: 1 117x slower;  n = 100 000: 1 872x slower.
//
//	Noise floor of this harness at the time: same-vs-same arms never significant
//	(p >= 0.564, largest point deviation 0.33%); cross-round, cross-binary worst
//	|delta| 0.73%; effective cross-arm floor ~2%.
//
// Reproduction of the historical A/B (at a0e5a990 ONLY):
//
//	git checkout a0e5a990
//	go test -c -o /tmp/a.test ./bench/audit352/
//	for i in $(seq 1 10); do
//	  /tmp/a.test -test.run='^$' -test.bench='BenchmarkLabelCountGate' \
//	    -test.count=1 -test.benchmem >> /tmp/gate.txt
//	done
//	benchstat -col /arm -row /n -filter '/arm:(before OR after OR off)' /tmp/gate.txt
//
// Reproduction of what THIS file measures (current tree):
//
//	go test ./bench/audit352/ -run 'LabelCount' -count=1 -v
//	go test -c -o /tmp/a.test ./bench/audit352/
//	for i in $(seq 1 10); do
//	  /tmp/a.test -test.run='^$' -test.bench='BenchmarkLabelCount' \
//	    -test.count=1 -test.benchmem >> /tmp/flat.txt
//	done
//	benchstat -row /n /tmp/flat.txt

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const pushdownLabel = "Item"

// pushdownQuery is the shape the pushdown serves: a bare, group-by-less,
// non-DISTINCT count over a bare single-label scan.
const pushdownQuery = `MATCH (n:` + pushdownLabel + `) RETURN count(n) AS c`

// scanQuery is the same label, made to SCAN. A WHERE wraps the scan in a Filter,
// which is exactly why tryBuildLabelCountScan declines it (a Selection between
// the scan and the aggregate would change which rows are counted), so this shape
// still walks every labelled node and is the vehicle for the boxing measurement.
// The predicate is true for every node, so the scan emits n rows and the
// aggregate consumes n rows.
const scanQuery = `MATCH (p:` + pushdownLabel + `) WHERE p.v >= 0 RETURN count(p) AS c`

// The two physical plans this file asserts. Pinning the exact text means an
// unexpected third plan fails loudly instead of being measured under the wrong
// label. Kept in step with the same constants in
// cypher/label_count_pushdown_gate_test.go.
const planPushdown = "Project\n└─ LabelCountScan"

// scanPlanFor renders the scanning plan for one label. The boxing differential
// below measures two labels of the SAME shape, so the expected plan has to be
// parameterised rather than pinned as a single constant; pinning the text at all
// is what makes an unexpected third plan fail loudly instead of being measured
// under the wrong name.
func scanPlanFor(label string) string {
	return "Project\n" +
		"└─ GlobalAggregateAdapter\n" +
		"   └─ EagerAggregation\n" +
		"      └─ Project\n" +
		"         └─ Filter\n" +
		"            └─ NodeByLabelScan [" + label + "]"
}

// planScan is scanPlanFor(pushdownLabel), kept in step with the same constants
// in cypher/label_count_pushdown_gate_test.go.
var planScan = scanPlanFor(pushdownLabel)

// ratchetSizes spans a 100x range. 50_000 is kept deliberately: it was the
// largest graph the deleted gate refused (the gate was strict >), so it is where
// a reintroduced size gate would most likely land.
var ratchetSizes = []int{1_000, 10_000, 50_000, 60_000, 100_000}

// pushdownGraphs caches one graph per size, so no measurement pays construction.
var pushdownGraphs = map[int]*lpg.Graph[string, float64]{}

// pushdownEngines caches one warmed engine per size.
var pushdownEngines = map[int]*cypher.Engine{}

// buildPushdownGraph creates n nodes all carrying pushdownLabel, each with an
// integer property v = i%100.
//
// v is deliberately kept BELOW 256. The Go runtime serves interface conversions
// of integers under 256 from its static staticuint64s table, so reading p.v in
// scanQuery's predicate and projection allocates nothing. That is what keeps the
// boxing model's fixed term constant in n: the only per-row heap allocation left
// in that shape is the node id the scan boxes. Raising v above 255 would add a
// second per-row allocation and make TestLabelCountBoxingAttribution measure two
// mechanisms at once.
func buildPushdownGraph(tb testing.TB, n int) *lpg.Graph[string, float64] {
	tb.Helper()
	if g, ok := pushdownGraphs[n]; ok {
		return g
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("i%d", i)
		if err := g.AddNode(k); err != nil {
			tb.Fatalf("buildPushdownGraph(%d) AddNode: %v", n, err)
		}
		if err := g.SetNodeLabel(k, pushdownLabel); err != nil {
			tb.Fatalf("buildPushdownGraph(%d) SetNodeLabel: %v", n, err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i%100))); err != nil {
			tb.Fatalf("buildPushdownGraph(%d) SetNodeProperty: %v", n, err)
		}
	}
	pushdownGraphs[n] = g
	return g
}

// pushdownEngine returns a warmed engine over the size-n graph, having asserted
// that both shapes compile to the plans this file measures. A fixture that
// cannot prove its own plan is never handed to a measurement.
func pushdownEngine(tb testing.TB, n int) *cypher.Engine {
	tb.Helper()
	if e, ok := pushdownEngines[n]; ok {
		return e
	}
	e := cypher.NewEngineWithOptions(buildPushdownGraph(tb, n), cypher.EngineOptions{})
	assertPlan(tb, e, pushdownQuery, planPushdown, n)
	assertPlan(tb, e, scanQuery, planScan, n)
	if got := runPushdownOnce(tb, e, pushdownQuery); got != int64(n) {
		tb.Fatalf("n=%d: %s returned %d", n, pushdownQuery, got)
	}
	if got := runPushdownOnce(tb, e, scanQuery); got != int64(n) {
		tb.Fatalf("n=%d: %s returned %d, so the predicate is not passing every row", n, scanQuery, got)
	}
	pushdownEngines[n] = e
	return e
}

func assertPlan(tb testing.TB, e *cypher.Engine, query, want string, n int) {
	tb.Helper()
	p, err := e.Explain(query, nil)
	if err != nil {
		tb.Fatalf("n=%d Explain(%q): %v", n, query, err)
	}
	if got := strings.TrimSpace(p); got != want {
		tb.Fatalf("n=%d %q compiled to\n%s\nwant\n%s", n, query, got, want)
	}
}

// runPushdownOnce executes query once and returns its single integer column.
func runPushdownOnce(tb testing.TB, e *cypher.Engine, query string) int64 {
	tb.Helper()
	res, err := e.Run(context.Background(), query, nil)
	if err != nil {
		tb.Fatalf("Run(%q): %v", query, err)
	}
	rows, got := 0, int64(-1)
	for res.Next() {
		rows++
		iv, ok := res.ValueAt(0).(expr.IntegerValue)
		if !ok {
			tb.Fatalf("column 0 is %T, want expr.IntegerValue", res.ValueAt(0))
		}
		got = int64(iv)
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("Err(%q): %v", query, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("Close(%q): %v", query, err)
	}
	if rows != 1 {
		tb.Fatalf("%q returned %d rows, want 1", query, rows)
	}
	return got
}

// drainQuery runs query to completion, discarding rows. It is the single
// primitive every measurement in this file uses, and it is named so an
// allocation profile can be attributed to it with `pprof -focus=drainQuery`,
// isolating the measured path from the package's unrelated TestMain fixture.
func drainQuery(tb testing.TB, e *cypher.Engine, query string) {
	res, err := e.Run(context.Background(), query, nil)
	if err != nil {
		tb.Fatalf("Run(%q): %v", query, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("Err(%q): %v", query, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("Close(%q): %v", query, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The O(1) ratchet
// ─────────────────────────────────────────────────────────────────────────────

// Ratchet tolerances. Every one is an ABSOLUTE, STRUCTURAL bound, never a
// fraction of the measured value: a proportional tolerance on a quantity that is
// supposed to be constant grows with the very regression it is meant to catch.
//
//   - allocsSlack / bytesSlack are the deterministic gates and the sharp ones.
//     Pre-fix, allocs/op was exactly n-185.2 with R2 = 1.000000; post-fix it is a
//     flat 29 with zero variance across ten benchstat samples in two rounds. The
//     slack exists only to absorb a single stray allocation from another
//     goroutine, since testing.AllocsPerRun reads process-global counters. A
//     reintroduced size gate puts +49 786 allocations and +475 776 bytes on the
//     n = 50 000 point, i.e. 49 786x and 7 434x these bounds.
//   - timeRatioMax bounds max(ns/op)/min(ns/op) over the whole size range. This
//     is the loose gate, because wall-clock is the noisy metric: the measured
//     ratio is ~1.01, the harness noise floor is ~2%, and any reintroduced size
//     gate — at whatever threshold — splits the range into a slow half and a fast
//     half and drives the ratio to at least 87x (the two endpoints under the
//     historical 26.56 ns/node slope). 1.5 sits two orders of magnitude above the
//     noise and roughly 58x below the smallest regression it must catch.
const (
	allocsSlack  = 1
	bytesSlack   = 64
	timeRatioMax = 1.5
)

// TestLabelCountPushdownIsConstantTime is the forward regression gate for
// rmp #2654: a bare labelled count must cost the same on a 100 000-node graph as
// on a 1 000-node one.
//
// It replaces the A/B sweep this file used to carry. The A/B collapsed by design
// when the gate was removed — there is no longer a slow arm an external package
// can build — so the property, not the difference, is what can still be pinned.
//
// allocs/op is the instrument that actually matters. It is exactly deterministic,
// so a size gate reappearing anywhere in the range shows up as a slope no noise
// floor can hide, and it does so without depending on the machine being quiet.
func TestLabelCountPushdownIsConstantTime(t *testing.T) {
	type point struct {
		n      int
		allocs float64
		bytes  uint64
	}
	pts := make([]point, 0, len(ratchetSizes))

	for _, n := range ratchetSizes {
		e := pushdownEngine(t, n)
		// Warm, then measure. AllocsPerRun does its own GC and warm-up run.
		drainQuery(t, e, pushdownQuery)
		allocs := testing.AllocsPerRun(100, func() { drainQuery(t, e, pushdownQuery) })

		// Bytes: bracket a fixed number of iterations with TotalAlloc. TotalAlloc
		// is process-global, so the bracket must be tight and nothing else may run
		// between the two reads.
		const iters = 200
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		for i := 0; i < iters; i++ {
			drainQuery(t, e, pushdownQuery)
		}
		runtime.ReadMemStats(&m1)
		bytes := (m1.TotalAlloc - m0.TotalAlloc) / iters

		pts = append(pts, point{n: n, allocs: allocs, bytes: bytes})
		t.Logf("n=%6d  allocs/op=%6.1f  B/op=%7d", n, allocs, bytes)
	}

	base := pts[0]
	for _, p := range pts[1:] {
		if d := p.allocs - base.allocs; d < -allocsSlack || d > allocsSlack {
			t.Errorf("allocs/op is NOT constant in graph size: n=%d gives %.1f, n=%d gives %.1f "+
				"(delta %+.1f, tolerance +/-%d). A bare labelled count must read the label "+
				"index in O(1); a per-node term here means a size gate is back on "+
				"tryBuildLabelCountScan, or the pushdown is declining for this shape.",
				base.n, base.allocs, p.n, p.allocs, d, allocsSlack)
		}
		if d := int64(p.bytes) - int64(base.bytes); d < -bytesSlack || d > bytesSlack {
			t.Errorf("B/op is NOT constant in graph size: n=%d gives %d, n=%d gives %d "+
				"(delta %+d, tolerance +/-%d)", base.n, base.bytes, p.n, p.bytes, d, bytesSlack)
		}
	}

	// Wall-clock flatness, over the two extremes only: the ratio is what the
	// property is about, and timing every size would add benchmark time to the
	// short layer for no extra discriminating power.
	lo := testing.Benchmark(func(b *testing.B) {
		e := pushdownEngine(b, ratchetSizes[0])
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			drainQuery(b, e, pushdownQuery)
		}
	})
	hi := testing.Benchmark(func(b *testing.B) {
		e := pushdownEngine(b, ratchetSizes[len(ratchetSizes)-1])
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			drainQuery(b, e, pushdownQuery)
		}
	})
	loNs, hiNs := float64(lo.NsPerOp()), float64(hi.NsPerOp())
	ratio := hiNs / loNs
	if ratio < 1 {
		ratio = 1 / ratio
	}
	t.Logf("ns/op  n=%d: %.0f   n=%d: %.0f   ratio=%.3f (tolerance %.1f)",
		ratchetSizes[0], loNs, ratchetSizes[len(ratchetSizes)-1], hiNs, ratio, timeRatioMax)
	if ratio > timeRatioMax {
		t.Errorf("ns/op is NOT flat in graph size: %.0f ns at n=%d vs %.0f ns at n=%d "+
			"(ratio %.2f > %.1f). Constant-time means the 100x larger graph is not "+
			"measurably slower.", loNs, ratchetSizes[0], hiNs,
			ratchetSizes[len(ratchetSizes)-1], ratio, timeRatioMax)
	}
}

// BenchmarkLabelCountPushdown is the single-arm sweep behind the ratchet. It
// exists to produce the numbers (`benchstat -row /n`) that the ratchet only
// bounds, and it brackets its timed loop with a plan re-check so a plan that
// drifts mid-run fails the run instead of being averaged.
func BenchmarkLabelCountPushdown(b *testing.B) {
	for _, n := range ratchetSizes {
		_ = pushdownEngine(b, n)
	}
	runtime.GC()
	for _, n := range ratchetSizes {
		b.Run(fmt.Sprintf("n=%06d", n), func(b *testing.B) {
			e := pushdownEngine(b, n)
			assertPlan(b, e, pushdownQuery, planPushdown, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				drainQuery(b, e, pushdownQuery)
			}
			b.StopTimer()
			assertPlan(b, e, pushdownQuery, planPushdown, n)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The per-row boxing finding
// ─────────────────────────────────────────────────────────────────────────────

// BenchmarkLabelCountScanBoxing measures the SCANNING shape at the sizes the
// boxing model is asserted on, so the model's inputs come from -benchmem rather
// than from a number typed into a table.
func BenchmarkLabelCountScanBoxing(b *testing.B) {
	for _, n := range []int{200, 256, 300, 1_000, 2_000} {
		b.Run(fmt.Sprintf("n=%06d", n), func(b *testing.B) {
			e := pushdownEngine(b, n)
			assertPlan(b, e, scanQuery, planScan, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				drainQuery(b, e, scanQuery)
			}
		})
	}
}

// TestLabelCountBoxingAttribution establishes the MECHANISM behind
// exec.NodeByLabelScan's per-row allocation, by DIFFERENCE.
//
// The finding is unchanged by rmp #2654, which removed a planner gate and did not
// touch the scan: cypher/exec/scan_label.go still writes
//
//	op.buf[0] = expr.IntegerValue(int64(op.iter.Next()))
//
// which converts an int64-backed named type to the expr.Value interface. The
// compiler lowers that to runtime.convT64, which serves values below 256 from the
// runtime's static staticuint64s table (allocation-free) and otherwise calls
// mallocgc(8, ...) ($GOROOT/src/runtime/iface.go). A heap profile of the pre-fix
// serial count at n = 50 000 attributed 83.2% of the query's bytes and
// essentially all of its objects to that one line. The scan's own godoc now
// documents this, having previously claimed a "zero-alloc contract".
//
// # Why this test is a DIFFERENCE and no longer an absolute model (rmp #2666)
//
// It used to sweep one graph per size and assert
//
//	allocs/op == #{ scanned node ids >= 256 } + F,   F constant in n
//
// with F required to hold a spread of 2 across the sweep. That model is TRUE, and
// the instrument built on it was still WRONG, because F is not a property of the
// engine: it is a property of the BUILD. Measured on the reference host
// (Apple M4, 10 cores, darwin/arm64, go1.27.0):
//
//	go test           ./bench/audit352/ -run TestLabelCountBoxingAttribution
//	  F = [72 73 73 73 73]      spread 1     PASS
//	go test -race     ./bench/audit352/ -run TestLabelCountBoxingAttribution
//	  F = [323 391 447 1325 2574] spread 2251 FAIL
//
// The race build adds ~1.25 allocations per SCANNED ROW — the residue is exactly
// linear in n, slope 1.2505 at the 1 000 -> 2 000 step — so subtracting only the
// boxed ids leaves a per-row term inside "the fixed term", and the spread grows
// with the sweep. `make ci` runs the short layer under -race, so the gate could
// not go green there however quiet the machine was. (The defect was FILED as
// process-global pollution from neighbouring tests in the package; that was
// refuted: the failure reproduces with this test running alone, and the whole
// package passes without -race.)
//
// The correction is the one rmp #2652 applied to the allocation instrument:
// measure the SUBJECT, not the process. Two arms are measured in the same window,
// on the same graph, through the same engine, with the SAME NUMBER OF ROWS, and
// differing in exactly one thing — the numeric value of the node ids the scan
// boxes:
//
//	:LoN  the N nodes with the LOWEST ids  -> every id < 256, no box allocates
//	:HiN  the N lowest ids >= 256          -> every id >= 256, every box allocates
//
// Every per-row cost that is not the id box — the Filter's property read, the
// aggregate's accumulation, and whatever the build adds — is identical on both
// arms and cancels in the difference. What is left is the boxing, and the
// prediction is exact and dimensionless:
//
//	allocs(:HiN) - allocs(:LoN) == #{id >= 256 in HiN} - #{id >= 256 in LoN} == N
//
// Nothing but one convT64 box per scanned id predicts that: no other cost in this
// plan depends on whether a node id happens to be above or below 256.
//
// # Measured
//
//	without -race: the difference is EXACT at every arm and every round —
//	               64/64/64, 128/128/128, 256/256/256 (9 observations).
//	with    -race: 63 64 63 / 129 126 128 / 258 258 254, i.e. |dev| <= 2 and
//	               |dev|/rows <= 1.6%, which is the race build's own allocation
//	               jitter, not the model. Re-measured under 20 competing busy
//	               processes (loadavg 20.5): medians 65 / 128 / 255, ratios
//	               1.0156 / 1.0000 / 0.9961.
//
// # Falsification (measured, rmp #2666)
//
// Two mutations of cypher/exec/scan_label.go were run against this test:
//
//	an extra unconditional allocation for ids BELOW 256, so the per-row cost
//	  stops depending on the id value:  median delta 0 / 0 / 0, ratio 0.0000, FAIL.
//	a SECOND box per scanned id:       median delta 128 / 256 / 512, ratio
//	  2.0000, FAIL.
//
// Both are exactly the two hypotheses boxingRatioSlack is sized to separate, and
// both went red at every arm.
//
// The ids are not dense from 0 in insertion order — they are hash-derived, so the
// 256 lowest ids of a 4 000-node graph are exactly 0..255 while the 200 lowest
// span [0, 761] — and they are therefore READ OUT of the engine with id(n) rather
// than assumed, exactly as before.
func TestLabelCountBoxingAttribution(t *testing.T) {
	fx := boxingArms(t)

	for _, rows := range boxingArmRows {
		loLbl, hiLbl := boxingLoLabel(rows), boxingHiLabel(rows)
		loQ, hiQ := boxingQuery(loLbl), boxingQuery(hiLbl)

		// A measurement on an unasserted plan is void, and a differential on two
		// DIFFERENT plans is worse than void: it would attribute the plan
		// difference to boxing.
		assertPlan(t, fx, loQ, scanPlanFor(loLbl), rows)
		assertPlan(t, fx, hiQ, scanPlanFor(hiLbl), rows)
		if got := runPushdownOnce(t, fx, loQ); got != int64(rows) {
			t.Fatalf("%s counted %d rows, want %d", loQ, got, rows)
		}
		if got := runPushdownOnce(t, fx, hiQ); got != int64(rows) {
			t.Fatalf("%s counted %d rows, want %d", hiQ, got, rows)
		}

		// The contrast is READ BACK, never assumed: the arms are only a
		// differential if their id distributions really do straddle 256.
		loLow, loGE, loMin, loMax := boxingIDSpread(t, fx, loLbl)
		hiLow, hiGE, hiMin, hiMax := boxingIDSpread(t, fx, hiLbl)
		t.Logf("rows=%3d  %-8s ids [%d,%d] #{<256}=%3d #{>=256}=%3d   %-8s ids [%d,%d] #{<256}=%3d #{>=256}=%3d",
			rows, loLbl, loMin, loMax, loLow, loGE, hiLbl, hiMin, hiMax, hiLow, hiGE)
		if loGE != 0 || hiGE != rows {
			t.Fatalf("rows=%d: the arms do not contrast — %s has %d ids >= 256 (want 0) and "+
				"%s has %d (want %d). Without the contrast this measures nothing.",
				rows, loLbl, loGE, hiLbl, hiGE, rows)
		}
		wantDelta := float64(hiGE - loGE)

		// Warm both arms outside every window, then INTERLEAVE: A B A B A B. A
		// block of A followed by a block of B would let drift between the blocks
		// enter the difference.
		drainQuery(t, fx, loQ)
		drainQuery(t, fx, hiQ)
		deltas := make([]float64, 0, boxingRounds)
		for r := 0; r < boxingRounds; r++ {
			lo := testing.AllocsPerRun(boxingRuns, func() { drainQuery(t, fx, loQ) })
			hi := testing.AllocsPerRun(boxingRuns, func() { drainQuery(t, fx, hiQ) })
			deltas = append(deltas, hi-lo)
			t.Logf("  rows=%3d round=%d  allocs/op lo=%6.0f hi=%6.0f  delta=%6.0f (want %.0f)",
				rows, r, lo, hi, hi-lo, wantDelta)
		}

		// The MEDIAN, not the mean: testing.AllocsPerRun divides a PROCESS-GLOBAL
		// malloc counter by the run count, so one window that happened to contain
		// a burst of background allocation shifts a single round and must not be
		// allowed to shift the verdict.
		got := medianOf(deltas)
		ratio := got / wantDelta
		t.Logf("  rows=%3d median delta=%.0f  want %.0f  ratio=%.4f", rows, got, wantDelta, ratio)
		if ratio < 1-boxingRatioSlack || ratio > 1+boxingRatioSlack {
			t.Errorf("rows=%d: %s allocated %.0f more per op than %s, want %.0f "+
				"(ratio %.4f, tolerance 1 +/- %.2f) across rounds %v. The two arms scan the "+
				"SAME number of rows through the SAME plan and differ only in whether the "+
				"scanned node ids are below 256, so the difference IS the convT64 boxing of "+
				"the node id in exec.NodeByLabelScan.Next. A ratio near 0 means the per-row "+
				"allocation is not the id box; a ratio near 2 means the scan now boxes twice.",
				rows, hiLbl, got, loLbl, wantDelta, ratio, boxingRatioSlack, deltas)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The boxing differential's fixture
// ─────────────────────────────────────────────────────────────────────────────

const (
	// boxingBaseLabel carries every node of the differential graph, so the id
	// map can be read with one query.
	boxingBaseLabel = "Boxed"

	// boxingTotal is large enough that ids 0..255 are ALL occupied, which is what
	// lets the :Lo arm be built entirely out of ids the runtime serves from
	// staticuint64s. Measured on this engine: 600 nodes fill 246 of those 256
	// slots, 4 000 fill all 256.
	boxingTotal = 4_000

	// boxingRuns is testing.AllocsPerRun's iteration count. It is high because
	// that function divides a PROCESS-GLOBAL malloc counter by it, so background
	// allocation has to reach boxingRuns objects inside a single window to move
	// the reading by one. Measured under -race: at 100 the difference wandered by
	// up to 5 allocations, at 500 and at 1 000 by at most 2. 500 is kept because
	// 1 000 doubled the test's cost for no measurable gain in stability.
	boxingRuns = 500

	// boxingRounds interleaved A/B pairs, reduced to their median.
	boxingRounds = 3

	// boxingRatioSlack is a band on a DIMENSIONLESS ratio whose predicted value
	// is 1. It is not a tolerance on an absolute figure, and it does not grow
	// with the regression it must catch: the two hypotheses it separates sit at
	// ratio 0 (the per-row allocation is not the id box) and ratio 2 (the scan
	// boxes twice), so any band strictly inside (0, 2) discriminates. 5% is 20x
	// clear of the nearer of them, and 3x the largest deviation measured under
	// -race (1.6% at rows=64).
	boxingRatioSlack = 0.05
)

// boxingArmRows are the row counts the differential is measured at. Every arm
// must fit inside the 256-wide free window, because the :Lo arm's whole point is
// that none of its ids allocates; 256 is therefore the largest arm possible and
// is kept as the sharpest one.
var boxingArmRows = []int{64, 128, 256}

func boxingLoLabel(rows int) string { return fmt.Sprintf("LoBox%d", rows) }
func boxingHiLabel(rows int) string { return fmt.Sprintf("HiBox%d", rows) }

// boxingQuery is the scanning shape, per arm label. The WHERE puts a Filter
// between the scan and the aggregate — the documented reason
// tryBuildLabelCountScan declines — so the scan really does emit one row per
// labelled node instead of being answered from the label index in O(1).
// p.v is i%100, below 256, so reading it is itself allocation-free and cannot
// contribute a second per-row mechanism.
func boxingQuery(label string) string {
	return `MATCH (p:` + label + `) WHERE p.v >= 0 RETURN count(p) AS c`
}

// boxingEngine caches the differential fixture: one graph, one engine, both arms.
var boxingEngine *cypher.Engine

// boxingArms builds the graph the differential is read from and labels the two
// arms of every row count.
//
// Node ids in this engine are hash-derived from the key, not dense in insertion
// order, so the arms cannot be built by choosing which nodes to create. They are
// built by MEASURING the ids first — id(p) against a base label — sorting them,
// and then labelling the two ends of that order.
func boxingArms(tb testing.TB) *cypher.Engine {
	tb.Helper()
	if boxingEngine != nil {
		return boxingEngine
	}
	key := func(i int) string { return fmt.Sprintf("b%d", i) }

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < boxingTotal; i++ {
		k := key(i)
		if err := g.AddNode(k); err != nil {
			tb.Fatalf("boxingArms AddNode(%s): %v", k, err)
		}
		if err := g.SetNodeLabel(k, boxingBaseLabel); err != nil {
			tb.Fatalf("boxingArms SetNodeLabel(%s): %v", k, err)
		}
		// v < 256: the predicate's read is allocation-free (see boxingQuery).
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i%100))); err != nil {
			tb.Fatalf("boxingArms SetNodeProperty(%s, v): %v", k, err)
		}
		// idx carries the key back out of the engine alongside id(p), so the id
		// order can be mapped to nodes without assuming anything about ids.
		if err := g.SetNodeProperty(k, "idx", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("boxingArms SetNodeProperty(%s, idx): %v", k, err)
		}
	}

	type nodeID struct {
		id  int64
		idx int64
	}
	probe := cypher.NewEngineWithOptions(g, cypher.EngineOptions{})
	res, err := probe.Run(context.Background(),
		`MATCH (p:`+boxingBaseLabel+`) RETURN id(p) AS i, p.idx AS x`, nil)
	if err != nil {
		tb.Fatalf("boxingArms id probe: %v", err)
	}
	ids := make([]nodeID, 0, boxingTotal)
	for res.Next() {
		iv, ok := res.ValueAt(0).(expr.IntegerValue)
		if !ok {
			tb.Fatalf("id(p) is %T, want expr.IntegerValue", res.ValueAt(0))
		}
		xv, ok := res.ValueAt(1).(expr.IntegerValue)
		if !ok {
			tb.Fatalf("p.idx is %T, want expr.IntegerValue", res.ValueAt(1))
		}
		ids = append(ids, nodeID{int64(iv), int64(xv)})
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("boxingArms id probe: %v", err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("boxingArms id probe close: %v", err)
	}
	if len(ids) != boxingTotal {
		tb.Fatalf("boxingArms id probe returned %d rows, want %d", len(ids), boxingTotal)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].id < ids[j].id })

	var below, atOrAbove []nodeID
	for _, n := range ids {
		if n.id < boxingFreeWindow {
			below = append(below, n)
		} else {
			atOrAbove = append(atOrAbove, n)
		}
	}
	maxRows := boxingArmRows[len(boxingArmRows)-1]
	if len(below) < maxRows || len(atOrAbove) < maxRows {
		tb.Fatalf("boxingArms: %d ids below %d and %d at or above it, need %d of each; "+
			"raise boxingTotal", len(below), boxingFreeWindow, len(atOrAbove), maxRows)
	}
	for _, rows := range boxingArmRows {
		for j := 0; j < rows; j++ {
			if err := g.SetNodeLabel(key(int(below[j].idx)), boxingLoLabel(rows)); err != nil {
				tb.Fatalf("boxingArms label lo: %v", err)
			}
			if err := g.SetNodeLabel(key(int(atOrAbove[j].idx)), boxingHiLabel(rows)); err != nil {
				tb.Fatalf("boxingArms label hi: %v", err)
			}
		}
	}

	boxingEngine = cypher.NewEngineWithOptions(g, cypher.EngineOptions{})
	return boxingEngine
}

// boxingFreeWindow is len(runtime.staticuint64s): the runtime serves interface
// conversions of integers in [0, 256) from that static table, so converting them
// to expr.Value allocates nothing. It is the constant the whole differential
// turns on.
const boxingFreeWindow = 256

// boxingIDSpread reads one arm's id distribution back out of the engine.
func boxingIDSpread(tb testing.TB, e *cypher.Engine, label string) (below, atOrAbove int, minID, maxID int64) {
	tb.Helper()
	res, err := e.Run(context.Background(), `MATCH (p:`+label+`) RETURN id(p) AS i`, nil)
	if err != nil {
		tb.Fatalf("boxingIDSpread(%s): %v", label, err)
	}
	minID, maxID = int64(1<<62), int64(-1)
	for res.Next() {
		iv, ok := res.ValueAt(0).(expr.IntegerValue)
		if !ok {
			tb.Fatalf("id(p) is %T, want expr.IntegerValue", res.ValueAt(0))
		}
		id := int64(iv)
		if id < boxingFreeWindow {
			below++
		} else {
			atOrAbove++
		}
		if id < minID {
			minID = id
		}
		if id > maxID {
			maxID = id
		}
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("boxingIDSpread(%s): %v", label, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("boxingIDSpread(%s) close: %v", label, err)
	}
	return below, atOrAbove, minID, maxID
}
