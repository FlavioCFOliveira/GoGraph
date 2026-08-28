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
const (
	planPushdown = "Project\n└─ LabelCountScan"
	planScan     = "Project\n" +
		"└─ GlobalAggregateAdapter\n" +
		"   └─ EagerAggregation\n" +
		"      └─ Project\n" +
		"         └─ Filter\n" +
		"            └─ NodeByLabelScan [" + pushdownLabel + "]"
)

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
// exec.NodeByLabelScan's per-row allocation.
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
// # Why the query changed
//
// This test used to drive the bare labelled count. Since #2654 that shape plans
// LabelCountScan and never scans, so it can no longer carry the measurement. It
// now drives scanQuery, whose WHERE puts a Filter between the scan and the
// aggregate — the documented reason tryBuildLabelCountScan declines — so the scan
// still emits one row per labelled node. The plan is asserted, because a boxing
// measurement on an unasserted plan is void.
//
// The shape was chosen by measurement, not by preference. `MATCH (p:Item) RETURN
// p.v` plans ColumnarProject over the scan and drives NodeByLabelScan.FillChunk,
// which appends UNBOXED int64 into a typed Chunk column: its allocs/op is a flat
// 53 from n=200 to n=2 000, so it exhibits none of the boxing and a test built on
// it would pass forever without measuring anything. scanQuery's allocs/op tracks
// #{id >= 256} exactly.
//
// # The prediction
//
// If the per-row allocation is convT64 boxing of the node id, then
//
//	allocs/op == #{ scanned node ids >= 256 } + F,   F constant in n
//
// An earlier version of this test assumed ids were dense from 0 in insertion
// order and predicted max(0, n-256). That assumption is FALSE in this engine —
// measured, the first 256 inserted nodes span the id range [0, 764] — and the
// prediction failed below n = 1 000 while holding exactly above it. The
// assumption is therefore replaced by the measured id distribution, read out of
// the engine with id(n). Nothing but boxing predicts those counts.
func TestLabelCountBoxingAttribution(t *testing.T) {
	fixed := make([]int, 0, 5)
	for _, n := range []int{200, 256, 300, 1_000, 2_000} {
		e := pushdownEngine(t, n)
		assertPlan(t, e, scanQuery, planScan, n)

		drainQuery(t, e, scanQuery)
		allocs := testing.AllocsPerRun(100, func() { drainQuery(t, e, scanQuery) })

		// Read the actual id distribution out of the engine rather than assuming it.
		res, err := e.Run(context.Background(),
			fmt.Sprintf(`MATCH (p:%s) RETURN id(p) AS i`, pushdownLabel), nil)
		if err != nil {
			t.Fatal(err)
		}
		rows, below, minID, maxID := 0, 0, int64(1<<62), int64(-1)
		for res.Next() {
			iv, ok := res.ValueAt(0).(expr.IntegerValue)
			if !ok {
				t.Fatalf("id(p) is %T, want expr.IntegerValue", res.ValueAt(0))
			}
			id := int64(iv)
			rows++
			if id < 256 {
				below++
			}
			if id < minID {
				minID = id
			}
			if id > maxID {
				maxID = id
			}
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		if err := res.Close(); err != nil {
			t.Fatal(err)
		}
		if rows != n {
			t.Fatalf("n=%d: id(p) returned %d rows", n, rows)
		}

		ge := n - below
		f := int(allocs) - ge
		fixed = append(fixed, f)
		t.Logf("n=%5d  allocs/op=%6.0f  id range [%d,%d]  #{id>=256}=%5d  F=allocs-#{id>=256}=%4d",
			n, allocs, minID, maxID, ge, f)
	}

	// The mechanism's signature is that F — what is left after subtracting one
	// allocation per boxed id — does not depend on n.
	//
	// Tolerance: a spread of 2. Measured, F is 72 at n=200 and 73 at every larger
	// size, i.e. a spread of exactly 1, from one chunk-growth step that differs
	// between 200 and 2 000 rows. The tolerance is deliberately NOT set to that
	// measured 1: a bound fitted to the run it was measured on has no headroom and
	// goes red on the next unrelated allocation. Nor is n=200 dropped to make the
	// spread zero — it is the size where most ids fall inside the free window, so
	// removing it would weaken the very contrast the model is read from.
	//
	// A spread of 2 still discriminates by ~850x: if the per-row allocation were
	// NOT the id box, subtracting #{id>=256} would leave the whole per-row term in
	// F, whose spread across n=200..2 000 would then be of order 1 700.
	const fixedTermSpreadMax = 2
	lo, hi := fixed[0], fixed[0]
	for _, f := range fixed {
		if f < lo {
			lo = f
		}
		if f > hi {
			hi = f
		}
	}
	if hi-lo > fixedTermSpreadMax {
		t.Errorf("F = allocs/op - #{id>=256} ranges over %v (spread %d > %d): the per-row "+
			"allocation is NOT one convT64 box per scanned node id",
			fixed, hi-lo, fixedTermSpreadMax)
	}
	t.Logf("F across sizes = %v (spread %d, tolerance %d)", fixed, hi-lo, fixedTermSpreadMax)
}
