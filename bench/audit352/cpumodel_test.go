package audit352_test

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
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

// TestCPUInstrumentSanity validates the CPU counter against a known amount
// of busy work before any result derived from it is believed. One busy
// goroutine for D should read ~D CPU-seconds; N busy goroutines ~N*D. A
// counter that clamped at one core, or that did not move at all, would be
// caught here rather than silently halving every number in the report.
func TestCPUInstrumentSanity(t *testing.T) {
	const d = 400 * time.Millisecond
	for _, threads := range []int{1, 2, 4} {
		start := cpuMicros()
		wall := time.Now()
		done := make(chan struct{})
		for i := 0; i < threads; i++ {
			go func() {
				x := 0.0
				for time.Since(wall) < d {
					for j := 0; j < 100000; j++ {
						x += float64(j)
					}
				}
				_ = x
				done <- struct{}{}
			}()
		}
		for i := 0; i < threads; i++ {
			<-done
		}
		got := float64(cpuMicros()-start) / 1e6
		want := d.Seconds() * float64(threads)
		t.Logf("threads=%d busy=%.2fs measured CPU=%.3fs (ratio %.2f)", threads, d.Seconds(), got, got/want)
		if got < want*0.7 {
			t.Fatalf("CPU counter under-reads: %.3fs measured for %.3fs of busy work on %d threads", got, want, threads)
		}
	}
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

	var unwind, agg []fitPoint
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
