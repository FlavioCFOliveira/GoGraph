package audit352_test

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// --- CPU instrument -------------------------------------------------------
//
// The peer comparison harness (bench/comparison/cpu_test.go) reads
// usage_usec from a container's cgroup v2 cpu.stat. Darwin has no cgroup
// hierarchy, so that instrument cannot run on this host at all. This is the
// in-process equivalent: getrusage(RUSAGE_SELF) totals user+system CPU
// across every thread of this process, so it tracks parallelism the same way
// usage_usec does and does not clamp at one core.
//
// It measures the MODULE embedded in a process — no Bolt, no protocol, no
// container. Absolute values are therefore NOT comparable with the
// container-measured peer figures; the fixed-vs-marginal SHAPE is.

// cpuMicros returns total process CPU (user+system) in microseconds.
func cpuMicros() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		// A harness must never substitute a sentinel drawn from the answer's
		// own value space for an unmeasurable quantity.
		panic("getrusage failed: " + err.Error())
	}
	u := int64(ru.Utime.Sec)*1_000_000 + int64(ru.Utime.Usec)
	s := int64(ru.Stime.Sec)*1_000_000 + int64(ru.Stime.Usec)
	return u + s
}

// TestCPUInstrumentSanity validates the CPU counter before any result derived
// from it is believed. Two properties are asserted, and both are RATIOS measured
// in the same conditions rather than absolute figures:
//
//   - it meters WORK, not wall time and not nothing: doubling the work on one
//     goroutine doubles the reading.
//   - it AGGREGATES ACROSS THREADS: the same work spread over N goroutines reads
//     N times what it reads on one. A counter clamped at a single core reads ~1x
//     whenever the machine really did run the goroutines in parallel (measured:
//     1.007); a counter scoped to the calling thread trips one of the two bounds
//     depending on scheduler placement (see busyCPU).
//
// # Why it is not "N busy goroutines for D seconds must read N*D" (rmp #2666)
//
// That is what it used to assert, and it is a claim about the MACHINE, not about
// the counter: it holds only while this process owns N whole cores. `make ci`
// runs the short layer as `go test -race ./...`, which runs packages in
// parallel, so it does not. Measured on the reference host (Apple M4, 10 cores,
// darwin/arm64, go1.27.0) with 20 competing busy processes:
//
//	threads=1 busy=0.40s measured CPU=0.199s (ratio 0.50)
//	CPU counter under-reads: 0.199s measured for 0.400s of busy work on 1 threads
//
// Nothing was wrong with the counter. The process got half a core, and a gate
// written as an absolute could not tell that apart from a broken instrument —
// which is exactly the failure mode this file exists to prevent.
//
// A fixed amount of WORK costs the same CPU however long it has to wait for a
// core, so every quantity here is a ratio of two work-defined windows. Contention
// changes the WALL time of each window and cancels out of the ratio. The unit of
// work is calibrated at run time, so the test does not carry a hard-coded
// iteration count fitted to one machine.
//
// # Falsification (measured, rmp #2666)
//
// Two mutations of cpuMicros were run against this test on the reference host:
//
//	one-core clamp (return wall-clock micros): work linearity still 2.021 (it
//	  is linear on one thread), thread aggregation n=2 -> 1.007, FAIL.
//	dead counter (return 0): FAIL in calibration, "could not reach 30ms of CPU
//	  in 1073741824 iterations".
//
// # Honest limits
//
//   - A counter scoped to the CALLING THREAD would read ~0 for the spawned arms
//     and so trip the same lower bound the one-core clamp tripped at 1.007. That
//     is REASONED, not measured: Darwin's syscall package exposes no
//     RUSAGE_THREAD to mutate cpuMicros into.
//   - A counter that under-reports by a CONSTANT FACTOR is invisible here,
//     because a constant factor cancels out of every ratio. It was equally
//     invisible to the absolute form this replaced, which could not tell a
//     halved counter from a half-loaded machine — it only appeared to catch it.
//   - If the host is so saturated that the N goroutines never actually run at
//     the same time, a wall-clock-like counter would also read ~N and the second
//     property loses its power against that particular alternative (it keeps full
//     power against a thread-scoped counter). The observed parallelism is
//     therefore measured and logged, so a reader can see whether that arm was
//     discriminating.
func TestCPUInstrumentSanity(t *testing.T) {
	unit := calibrateCPUUnit(t, cpuUnitTargetCPU)

	type round struct {
		single, double float64         // CPU seconds, 1 goroutine, U and 2U units
		multi          map[int]float64 // CPU seconds, N goroutines x U units
		wall           map[int]float64 // wall seconds of the same windows
	}
	rounds := make([]round, 0, cpuSanityRounds)
	for r := 0; r < cpuSanityRounds; r++ {
		// Interleaved within the round: an arm is never measured as a block, so
		// drift between blocks cannot enter a ratio.
		c1, w1 := busyCPU(1, 1, unit)
		c2, _ := busyCPU(1, 2, unit)
		rd := round{single: c1, double: c2, multi: map[int]float64{}, wall: map[int]float64{}}
		rd.wall[1] = w1
		for _, n := range cpuSanityThreads {
			c, w := busyCPU(n, 1, unit)
			rd.multi[n] = c
			rd.wall[n] = w
		}
		rounds = append(rounds, rd)
		t.Logf("round %d  1x1U=%.4fs  1x2U=%.4fs  %s (unit=%d iters, wall 1x1U=%.4fs)",
			r, rd.single, rd.double, fmtMulti(rd.multi, rd.wall), unit, w1)
	}

	single := medianOf(pick(rounds, func(r round) float64 { return r.single }))
	double := medianOf(pick(rounds, func(r round) float64 { return r.double }))
	if single <= 0 {
		t.Fatalf("the CPU counter did not move at all across %d units of busy work "+
			"(median %.6fs): every figure derived from it is void", cpuSanityRounds, single)
	}

	// Property 1 — it meters work. Predicted ratio 2; a dead counter gives 0 and
	// a counter that lost half the work gives 1.
	lin := double / single
	t.Logf("work linearity: 2U/1U = %.3f (want 2, tolerance %.2f..%.2f)", lin, cpuLinMin, cpuLinMax)
	if lin < cpuLinMin || lin > cpuLinMax {
		t.Fatalf("the CPU counter is not proportional to work: twice the work read %.4fs "+
			"against %.4fs (ratio %.3f, want %.2f..%.2f). A counter that does not meter work "+
			"cannot attribute CPU to a query.", double, single, lin, cpuLinMin, cpuLinMax)
	}

	// Property 2 — it aggregates across threads. Predicted ratio N.
	for _, n := range cpuSanityThreads {
		multi := medianOf(pick(rounds, func(r round) float64 { return r.multi[n] }))
		wallN := medianOf(pick(rounds, func(r round) float64 { return r.wall[n] }))
		ratio := multi / single
		// How much parallelism the host actually granted, so the reader can see
		// whether the one-core-clamp alternative was discriminated at all.
		clamp := multi / wallN
		t.Logf("thread aggregation n=%d: %.4fs / %.4fs = %.3f (want %d, tolerance %.2f..%.2f); "+
			"CPU/wall in that window = %.2f (>1 means the host really did run them in parallel)",
			n, multi, single, ratio, n, cpuAggMin*float64(n), cpuAggMax*float64(n), clamp)
		if clamp <= 1.05 {
			t.Logf("  note: the host granted little or no parallelism in that window, so this " +
				"arm has reduced power against a counter clamped at one core; it retains full " +
				"power against a counter scoped to the calling thread.")
		}
		if ratio < cpuAggMin*float64(n) || ratio > cpuAggMax*float64(n) {
			t.Fatalf("the CPU counter does not aggregate across threads: %d goroutines each "+
				"doing the SAME work as the single-goroutine arm read %.4fs against %.4fs "+
				"(ratio %.3f, want %d +%.0f%%/-%.0f%%). A ratio near 1 means it clamps at one "+
				"core; a ratio near 0 means it counts only the calling thread. Either way every "+
				"CPU figure in this package would be understated.",
				n, multi, single, ratio, n, (cpuAggMax-1)*100, (1-cpuAggMin)*100)
		}
	}
}

// --- CPU instrument calibration and busy work --------------------------------

const (
	// cpuUnitTargetCPU is how much CPU one unit of busy work must consume before
	// it is used as the ratios' denominator. Large enough that getrusage's
	// microsecond granularity and the goroutine-launch overhead are noise;
	// small enough that the whole test costs a fraction of a CPU-second.
	cpuUnitTargetCPU = 30 * time.Millisecond

	// cpuSanityRounds interleaved rounds, reduced to medians, so one window that
	// happened to be descheduled does not decide the verdict.
	cpuSanityRounds = 3

	// cpuLinMin/cpuLinMax bound the work-linearity ratio, whose predicted value
	// is exactly 2. The alternatives it separates are 0 (a dead counter) and 1
	// (a counter losing half the work), so the band is far from both.
	cpuLinMin = 1.6
	cpuLinMax = 2.6

	// cpuAggMin/cpuAggMax are multipliers on N for the thread-aggregation ratio,
	// whose predicted value is exactly N.
	//
	// The LOWER bound is the gate: under-reading is the failure this test exists
	// to catch, and the alternatives sit at 0 and at 1, i.e. at or below N/2 for
	// every N used here.
	//
	// The UPPER bound is only a runaway guard and is deliberately loose, because
	// the same work legitimately costs MORE CPU-time when it runs on more
	// threads: on this host the efficiency cores execute it more slowly than the
	// performance cores, and contention adds system time to every window. An
	// upper bound tight enough to be interesting would fail on a busy machine
	// while detecting nothing the lower bound does not already detect.
	cpuAggMin = 0.7
	cpuAggMax = 2.5
)

// cpuSanityThreads are the goroutine counts the aggregation property is asserted
// at. Kept at or below half the host's cores on the reference machine so the
// window has some chance of real parallelism even under a parallel package run.
var cpuSanityThreads = []int{2, 4}

// cpuBusySink keeps every accumulator observable outside the loop that produced
// it, so the compiler cannot delete the work being measured. It is written only
// from the test goroutine, after every worker has been joined.
var cpuBusySink float64

// cpuBusy performs iters units of deterministic floating-point work.
//
// Floating-point addition is not associative, so the compiler may not
// strength-reduce the loop to a closed form, and the returned accumulator keeps
// it from being eliminated. The work touches no memory beyond one register, so
// its CPU cost depends on the core it runs on and on nothing else in the process.
func cpuBusy(iters int) float64 {
	x := 0.0
	for j := 0; j < iters; j++ {
		x += float64(j)
	}
	return x
}

// calibrateCPUUnit returns an iteration count for cpuBusy whose measured CPU cost
// is at least want.
//
// It is a measurement, not a constant, because the same iteration count costs
// different CPU on different cores and different machines — and because a
// hard-coded count fitted to one host is precisely the kind of absolute this
// test was rewritten to stop making. Contention does not disturb it: waiting for
// a core costs wall time, not CPU time.
func calibrateCPUUnit(tb testing.TB, want time.Duration) int {
	tb.Helper()
	const maxIters = 1 << 30
	iters := 1 << 20
	for {
		c0 := cpuMicros()
		cpuBusySink += cpuBusy(iters)
		got := time.Duration(cpuMicros()-c0) * time.Microsecond
		if got >= want {
			return iters
		}
		if iters >= maxIters {
			tb.Fatalf("calibration could not reach %v of CPU in %d iterations of cpuBusy "+
				"(got %v): the CPU counter is not moving with work", want, iters, got)
		}
		iters *= 2
	}
}

// busyCPU runs threads goroutines, each performing units*iters of cpuBusy work,
// and returns the process CPU and the wall time consumed by that window.
//
// threads == 1 still spawns a goroutine so that the multi-threaded arms differ
// from it in ONE variable only — the number of goroutines — and so that the
// ratio's denominator is measured on the same kind of thread as its numerator.
//
// A counter scoped to the CALLING thread is caught either way, but by different
// assertions depending on where the scheduler put the single worker: if it runs
// on the parked caller's M the denominator is full and the N-goroutine arms read
// ~1/N of it, tripping the aggregation bound; if it runs elsewhere the
// denominator itself reads ~0 and the "did not move at all" floor trips first.
func busyCPU(threads, units, iters int) (cpuSec, wallSec float64) {
	out := make([]float64, threads)
	var wg sync.WaitGroup
	c0 := cpuMicros()
	t0 := time.Now()
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			s := 0.0
			for u := 0; u < units; u++ {
				s += cpuBusy(iters)
			}
			out[slot] = s
		}(i)
	}
	wg.Wait()
	wallSec = time.Since(t0).Seconds()
	cpuSec = float64(cpuMicros()-c0) / 1e6
	for _, v := range out {
		cpuBusySink += v
	}
	return cpuSec, wallSec
}

// pick projects one field out of every round, for medianOf.
func pick[T any](rs []T, f func(T) float64) []float64 {
	out := make([]float64, 0, len(rs))
	for _, r := range rs {
		out = append(out, f(r))
	}
	return out
}

// fmtMulti renders the multi-goroutine arms of one round.
func fmtMulti(cpu, wall map[int]float64) string {
	ns := make([]int, 0, len(cpu))
	for n := range cpu {
		ns = append(ns, n)
	}
	sort.Ints(ns)
	var sb strings.Builder
	for i, n := range ns {
		if i > 0 {
			sb.WriteString("  ")
		}
		fmt.Fprintf(&sb, "%dx1U=%.4fs(wall %.4fs)", n, cpu[n], wall[n])
	}
	return sb.String()
}

// --- linear fit -----------------------------------------------------------

type fitPoint struct {
	k   int
	cpu float64 // microseconds of CPU per op
}

// olsFit fits cpu = a + b*k by ordinary least squares and returns a, b, r2.
// It refuses fewer than three points: a two-point "fit" is a subtraction
// wearing a hat.
func olsFit(pts []fitPoint) (a, b, r2 float64) {
	if len(pts) < 3 {
		panic("olsFit needs >= 3 points")
	}
	n := float64(len(pts))
	var sx, sy, sxx, sxy float64
	for _, p := range pts {
		x, y := float64(p.k), p.cpu
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := n*sxx - sx*sx
	b = (n*sxy - sx*sy) / den
	a = (sy - b*sx) / n
	mean := sy / n
	var ssTot, ssRes float64
	for _, p := range pts {
		pred := a + b*float64(p.k)
		ssRes += (p.cpu - pred) * (p.cpu - pred)
		ssTot += (p.cpu - mean) * (p.cpu - mean)
	}
	if ssTot == 0 {
		return a, b, math.NaN()
	}
	return a, b, 1 - ssRes/ssTot
}

// --- measured arm ---------------------------------------------------------

type armResult struct {
	name      string
	k         int
	ops       int
	wallSec   float64
	cpuPerOp  float64 // gross microseconds of CPU per op
	netPerOp  float64 // idle-corrected
	nsPerOp   float64
	rowsPerOp int
}

// measureIdleMicrosPerSec measures what this process burns while doing no
// queries at all, immediately before the measured window, so background
// runtime work is not billed to the engine. It is bracketed exactly like the
// load window it corrects.
func measureIdleMicrosPerSec(d time.Duration) float64 {
	c0 := cpuMicros()
	t0 := time.Now()
	time.Sleep(d)
	el := time.Since(t0).Seconds()
	return float64(cpuMicros()-c0) / el
}

// runArm drives query for at least d, after an untimed warm-up, and returns
// gross and idle-corrected CPU per op. wantRows is asserted on EVERY op:
// an arm that stops producing the rows it is supposed to produce fails the
// run instead of reporting a cheap number.
func runArm(tb testing.TB, engine *cypher.Engine, name string, k int, query string, params map[string]expr.Value, wantRows int, warm, idle, d time.Duration) armResult {
	tb.Helper()
	ctx := context.Background()
	drain := func() int {
		res, err := engine.Run(ctx, query, params)
		if err != nil {
			tb.Fatalf("%s: Run: %v", name, err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if e := res.Err(); e != nil {
			tb.Fatalf("%s: Err: %v", name, e)
		}
		if err := res.Close(); err != nil {
			tb.Fatalf("%s: Close: %v", name, err)
		}
		return n
	}
	// Warm-up: same shape, same concurrency, untimed.
	wEnd := time.Now().Add(warm)
	for time.Now().Before(wEnd) {
		if n := drain(); n != wantRows {
			tb.Fatalf("%s: warm-up shipped %d rows, want %d", name, n, wantRows)
		}
	}
	idleRate := measureIdleMicrosPerSec(idle)

	c0 := cpuMicros()
	t0 := time.Now()
	ops := 0
	for time.Since(t0) < d {
		if n := drain(); n != wantRows {
			tb.Fatalf("%s: shipped %d rows, want %d", name, n, wantRows)
		}
		ops++
	}
	wall := time.Since(t0).Seconds()
	cpu := float64(cpuMicros() - c0)

	net := cpu - idleRate*wall
	if net < 0 {
		net = 0
	}
	return armResult{
		name: name, k: k, ops: ops, wallSec: wall,
		cpuPerOp:  cpu / float64(ops),
		netPerOp:  net / float64(ops),
		nsPerOp:   wall * 1e9 / float64(ops),
		rowsPerOp: wantRows,
	}
}

// cpuModelKs are the row counts swept for the fixed/marginal fit.
var cpuModelKs = []int{1, 10, 100, 1000, 10000}

// TestCPUModel_FixedAndMarginal re-derives, entirely at HEAD and entirely
// in this process, the two numbers that describe the engine's cost curve:
// the fixed CPU of accepting a query at all, and the marginal CPU of one
// more row.
//
// Two families are swept:
//
//	unwind      UNWIND range(1,k) AS i RETURN i        -- k rows produced AND shipped
//	unwind_agg  UNWIND range(1,k) AS i RETURN count(i) -- k rows produced, 1 shipped
//
// Their difference at the same k isolates SHIPPING a row from PRODUCING it,
// which is the whole question this audit is pointed at.
//
// Run explicitly:
//
//	go test -run '^TestCPUModel_FixedAndMarginal$' -v -timeout 30m ./bench/audit352/
func TestCPUModel_FixedAndMarginal(t *testing.T) {
	if testing.Short() {
		t.Skip("cpu model sweep is not a short-layer test")
	}
	// The model is a SERIAL cost model: pin to one P so a parallel operator
	// cannot inflate CPU-per-op relative to work done. GOMAXPROCS is restored
	// on exit.
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)

	// A tiny graph: these shapes must not touch it, and a large fixture
	// would only add GC pressure to a measurement about query overhead.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	must(g.AddNode("seed"))
	must(g.SetNodeLabel("seed", "Seed"))
	engine := cypher.NewEngine(g)

	const (
		warm = 300 * time.Millisecond
		idle = 200 * time.Millisecond
		dur  = 1200 * time.Millisecond
	)

	unwind := make([]fitPoint, 0, len(cpuModelKs))
	agg := make([]fitPoint, 0, len(cpuModelKs))
	t.Logf("%-14s %8s %10s %12s %12s %12s", "arm", "k", "ops", "gross us/op", "net us/op", "ns/op")
	for _, k := range cpuModelKs {
		q := fmt.Sprintf(`UNWIND range(1,%d) AS i RETURN i`, k)
		r := runArm(t, engine, "unwind", k, q, nil, k, warm, idle, dur)
		t.Logf("%-14s %8d %10d %12.2f %12.2f %12.0f", r.name, r.k, r.ops, r.cpuPerOp, r.netPerOp, r.nsPerOp)
		unwind = append(unwind, fitPoint{k: k, cpu: r.netPerOp})

		qa := fmt.Sprintf(`UNWIND range(1,%d) AS i RETURN count(i) AS c`, k)
		ra := runArm(t, engine, "unwind_agg", k, qa, nil, 1, warm, idle, dur)
		t.Logf("%-14s %8d %10d %12.2f %12.2f %12.0f", ra.name, ra.k, ra.ops, ra.cpuPerOp, ra.netPerOp, ra.nsPerOp)
		agg = append(agg, fitPoint{k: k, cpu: ra.netPerOp})
	}

	noop := runArm(t, engine, "noop", 1, `RETURN 1 AS x`, nil, 1, warm, idle, dur)
	t.Logf("%-14s %8d %10d %12.2f %12.2f %12.0f", noop.name, noop.k, noop.ops, noop.cpuPerOp, noop.netPerOp, noop.nsPerOp)

	au, bu, r2u := olsFit(unwind)
	aa, ba, r2a := olsFit(agg)
	t.Logf("FIT unwind      (produce+ship): fixed a=%.2f us/query  marginal b=%.4f us/row  r2=%.4f", au, bu, r2u)
	t.Logf("FIT unwind_agg  (produce only): fixed a=%.2f us/query  marginal b=%.4f us/row  r2=%.4f", aa, ba, r2a)
	t.Logf("SHIP COST per row (unwind.b - unwind_agg.b) = %.4f us/row", bu-ba)
	t.Logf("NOOP floor (RETURN 1) = %.2f us/query", noop.netPerOp)
}

// TestCPUModel_ScanShapes measures the scan-family shapes on a graph the
// size the peer harness used, so the per-node scan primitive and the cost of
// one predicate evaluation per node can be read off directly.
//
//	scan_count   MATCH (n:Person) RETURN count(n)                  -- visit N, ship 1
//	scan_filter  MATCH (n:Person) WHERE n.age > $a RETURN count(n)  -- visit N, filter N, ship 1
//
// scan_filter - scan_count is one expression evaluation per node.
func TestCPUModel_ScanShapes(t *testing.T) {
	if testing.Short() {
		t.Skip("scan shape sweep is not a short-layer test")
	}
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)

	const (
		warm = 300 * time.Millisecond
		idle = 200 * time.Millisecond
		dur  = 1500 * time.Millisecond
	)
	for _, n := range []int{5000, 50000} {
		n := n
		t.Run(fmt.Sprintf("nodes=%d", n), func(t *testing.T) {
			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			for i := 0; i < n; i++ {
				k := fmt.Sprintf("p%d", i)
				must(g.AddNode(k))
				must(g.SetNodeLabel(k, "Person"))
				must(g.SetNodeProperty(k, "age", lpg.Int64Value(int64(1000+i%65000))))
				must(g.SetNodeProperty(k, "name", lpg.StringValue(k)))
			}
			engine := cypher.NewEngine(g)
			// Median of the ACTUAL age range this n produces (ages are
			// 1000+i%65000), so the predicate selects ~half the nodes at
			// every n. A fixed threshold selected 0 rows at n=5000 and made
			// the "shipping" delta meaningless.
			span := n
			if span > 65000 {
				span = 65000
			}
			mid := int64(1000 + span/2)

			// scan_count: the count store may answer this without scanning.
			// The plan is logged so the reader can tell which happened.
			qCount := `MATCH (n:Person) RETURN count(n) AS c`
			qCountProp := `MATCH (n:Person) RETURN count(n.age) AS c`
			qFilter := fmt.Sprintf(`MATCH (n:Person) WHERE n.age > %d RETURN count(n) AS c`, mid)
			qShip := fmt.Sprintf(`MATCH (n:Person) WHERE n.age > %d RETURN n.name`, mid)
			wantShip := 0
			for i := 0; i < n; i++ {
				if int64(1000+i%65000) > mid {
					wantShip++
				}
			}
			for _, q := range []string{qCount, qCountProp, qFilter, qShip} {
				p, err := engine.Explain(q, nil)
				if err != nil {
					t.Fatalf("Explain(%q): %v", q, err)
				}
				t.Logf("PLAN %s\n%s", q, p)
			}

			rc := runArm(t, engine, "scan_count", n, qCount, nil, 1, warm, idle, dur)
			rcp := runArm(t, engine, "scan_count_prop", n, qCountProp, nil, 1, warm, idle, dur)
			rf := runArm(t, engine, "scan_filter", n, qFilter, nil, 1, warm, idle, dur)
			rs := runArm(t, engine, "scan_filter_ship", n, qShip, nil, wantShip, warm, idle, dur)

			t.Logf("%-18s %10s %12s %12s %14s", "arm", "ops", "net us/op", "ns/op", "ns per node")
			for _, r := range []armResult{rc, rcp, rf, rs} {
				t.Logf("%-18s %10d %12.2f %12.0f %14.2f", r.name, r.ops, r.netPerOp, r.nsPerOp, r.nsPerOp/float64(n))
			}
			t.Logf("ships: scan_filter_ship shipped %d rows/op", wantShip)
			t.Logf("DELTA scan_count_prop - scan_count = %.2f us/op (%.2f ns/node)  [cost of reading ONE property per scanned node; SAME columnar path]",
				rcp.netPerOp-rc.netPerOp, (rcp.nsPerOp-rc.nsPerOp)/float64(n))
			t.Logf("RATIO scan_filter / scan_filter_ship = %.2fx  [NOT a matched pair: the aggregating form plans on the ROW path (Filter+Project), the shipping form on the COLUMNAR path (ColumnarFilter). Compare the PLANS above before reading this.]",
				rf.nsPerOp/rs.nsPerOp)
		})
	}
}

// medianOf returns the median of xs (xs is sorted in place).
func medianOf(xs []float64) float64 {
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}
