//go:build soak || nightly

// Package main_test — heap / FD / goroutine growth soak test.
//
// TestNoGrowth_HeapFDGoroutine samples three resource metrics at 10-second
// intervals during a measurement window. After a warm-up phase it fits a
// least-squares linear regression over each metric's post-warm-up samples
// and asserts that the absolute slope is below the documented epsilons:
//
//   - heap: 1 MB / sample  (10 s)
//   - goroutines: 1.0 / sample
//   - file descriptors: 2.0 / sample
//
// # Which configurations actually assert the slope
//
//   - Short layer (plain -tags=soak): 5 s warm-up + 20 s measurement, which at a
//     10 s sample interval yields 2 samples — below minRegressionPoints. The
//     slope check therefore CANNOT run, and the test SKIPS rather than passing
//     silently. The short layer exercises the workload; it does not certify
//     no-growth. See rmp #2396.
//   - Intermediate (documented): SOAK_NOGROWTH_MEASURE=90s gives 9 samples, so
//     the slope check runs and asserts, in about 95 s.
//   - Full soak (SOAK_FULL=1, no -short): 5 min warm-up + 55 min measurement,
//     ~330 samples. This is the variant that meets CLAUDE.md's soak criterion.
//
// Because SOAK_NOGROWTH_* and SOAK_FULL express an expectation that the
// criterion be evaluated, too few samples under any of them is a FAILURE rather
// than a skip (see requireRegressionPoints).
//
// Samples are written as CSV to bench/soak/soak-artefacts/no-growth-<ts>.csv.
package main_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

const (
	noGrowthSampleInterval    = 10 * time.Second
	noGrowthEpsilonHeap       = 1e6 // bytes per sample
	noGrowthEpsilonGoroutines = 1.0 // goroutines per sample
	noGrowthEpsilonFD         = 2.0 // file descriptors per sample
)

// ngSample records a single resource snapshot.
type ngSample struct {
	elapsed    time.Duration
	heapAlloc  float64
	goroutines float64
	fds        float64
}

// TestNoGrowth_HeapFDGoroutine runs the growth-regression soak.
func TestNoGrowth_HeapFDGoroutine(t *testing.T) {
	// Short layer: 5 s warm-up + 20 s measurement, inside the 60 s per-test CI
	// budget. Full soak (SOAK_FULL=1, no -short): 5 min + 55 min.
	//
	// Both are overridable so an intermediate window can be requested without
	// editing the file — which is what makes the slope check reachable outside
	// the hour-long variant. The sample INTERVAL is deliberately not
	// overridable: shrinking it is the one adjustment that would manufacture
	// samples without lengthening the observation, turning the regression into a
	// noise detector on a handful of adjacent points.
	warmup := 5 * time.Second
	measurement := 20 * time.Second
	if soakFullRun() {
		warmup = 5 * time.Minute
		measurement = 55 * time.Minute
	}
	warmup = soakEnvDuration("SOAK_NOGROWTH_WARMUP", warmup)
	measurement = soakEnvDuration("SOAK_NOGROWTH_MEASURE", measurement)
	t.Logf("no_growth: config warmup=%v measurement=%v interval=%v (expect %d samples, need %d to assert)",
		warmup, measurement, noGrowthSampleInterval,
		int(measurement/noGrowthSampleInterval), minRegressionPoints)

	// ── Build seed graph for background workload ──────────────────────────────
	const graphN = 512
	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic
	for i := range graphN {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("seed AddNode %d: %v", i, err)
		}
	}
	for range graphN * 4 {
		_ = a.AddEdge(rng.IntN(graphN), rng.IntN(graphN), int64(rng.IntN(100)+1))
	}
	var snapPtr atomic.Pointer[csr.CSR[int64]]
	snapPtr.Store(csr.BuildFromAdjList(a))

	// ── Background workload ───────────────────────────────────────────────────
	// One sample interval of grace beyond the window. Without it the final
	// ticker tick and the context deadline fall on the same instant, so the last
	// sample is decided by a race between two timers and the sample count is
	// nondeterministic — the 2026-08-10 run collected 1 sample where the
	// arithmetic says 2. The measurement loop below ends the window on
	// measureEnd; the deadline is only a backstop. Cancel is explicit at `done`,
	// so the grace does not lengthen the run.
	ctx, cancel := context.WithTimeout(context.Background(), warmup+measurement+noGrowthSampleInterval)
	defer cancel()

	const bgReaders = 4
	var wg sync.WaitGroup
	wg.Add(bgReaders)
	for i := range bgReaders {
		go func(id int) {
			defer wg.Done()
			rr := rand.New(rand.NewPCG(uint64(id)+100, 29)) //nolint:gosec // deterministic
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				snap := snapPtr.Load()
				src := graph.NodeID(rr.IntN(int(snap.MaxNodeID())))
				if rr.IntN(2) == 0 {
					search.BFS(snap, src, func(_ graph.NodeID, _ int) bool { return true })
				} else {
					_, _ = search.Dijkstra(snap, src)
				}
				runtime.Gosched()
			}
		}(i)
	}

	// ── The fd instrument must be able to measure before we rely on it ────────
	// ngCountFDs reports -1 when neither /proc/self/fd nor /dev/fd can be read.
	// That -1 was previously recorded as a sample VALUE, and a metric that is
	// constant across every sample has slope 0.000 — so on a host where fd
	// counting is unavailable the fd criterion passed while measuring nothing,
	// exactly the failure mode rmp #2396 is about. Establish up front that the
	// count is real, and refuse to certify no-fd-growth otherwise.
	if n := ngCountFDs(); n < 0 {
		t.Fatalf("no_growth: cannot count file descriptors (neither /proc/self/fd nor "+
			"/dev/fd is readable); the fd no-growth criterion cannot be evaluated on this "+
			"host, and a slope over a constant sentinel would pass without measuring anything (got %d)", n)
	}

	// ── Warm-up ───────────────────────────────────────────────────────────────
	t.Logf("no_growth: warm-up started (%v)", warmup)
	select {
	case <-time.After(warmup):
	case <-ctx.Done():
		t.Fatal("no_growth: context cancelled during warm-up")
	}
	t.Log("no_growth: warm-up complete; starting measurement window")

	// ── Measurement ───────────────────────────────────────────────────────────
	var samples []ngSample
	ticker := time.NewTicker(noGrowthSampleInterval)
	defer ticker.Stop()
	measureEnd := time.Now().Add(measurement)
	start := time.Now()
	for time.Now().Before(measureEnd) {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			goto done
		}
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fds := float64(ngCountFDs())
		g := float64(runtime.NumGoroutine())
		elapsed := time.Since(start)
		samples = append(samples, ngSample{
			elapsed:    elapsed,
			heapAlloc:  float64(ms.HeapAlloc),
			goroutines: g,
			fds:        fds,
		})
		t.Logf("no_growth: t=%v heap=%.0f goroutines=%.0f fds=%.0f",
			elapsed.Truncate(time.Second), float64(ms.HeapAlloc), g, fds)
	}

done:
	cancel()
	wg.Wait()

	// ── Write CSV artefact ────────────────────────────────────────────────────
	ngWriteCSV(t, samples)

	// ── Regression assertions ─────────────────────────────────────────────────
	// goleak runs first and unconditionally: the workload has stopped, so no
	// goroutine of ours may survive it, and that check is independent of how many
	// samples the window produced. Ordering it ahead of the slope gate keeps it
	// live on the paths where the gate skips or fails.
	goleak.VerifyNone(t)

	if !requireRegressionPoints(t, "no_growth", len(samples),
		"run with SOAK_NOGROWTH_MEASURE=90s for 9 samples (~95 s), or SOAK_FULL=1 for the full 60-minute gate",
		"SOAK_NOGROWTH_MEASURE", "SOAK_NOGROWTH_WARMUP") {
		return
	}

	xs := make([]float64, len(samples))
	for i := range samples {
		xs[i] = float64(i)
	}

	heapSlope := linRegSlope(xs, func(i int) float64 { return samples[i].heapAlloc })
	gorSlope := linRegSlope(xs, func(i int) float64 { return samples[i].goroutines })
	fdSlope := linRegSlope(xs, func(i int) float64 { return samples[i].fds })

	t.Logf("no_growth: slope heap=%.0f goroutines=%.3f fds=%.3f (per 10-s sample)",
		heapSlope, gorSlope, fdSlope)

	if math.Abs(heapSlope) >= noGrowthEpsilonHeap {
		t.Errorf("no_growth: heap slope %.0f bytes/sample >= epsilon %.0f",
			heapSlope, noGrowthEpsilonHeap)
	}
	if math.Abs(gorSlope) >= noGrowthEpsilonGoroutines {
		t.Errorf("no_growth: goroutine slope %.3f/sample >= epsilon %.3f",
			gorSlope, noGrowthEpsilonGoroutines)
	}
	if math.Abs(fdSlope) >= noGrowthEpsilonFD {
		t.Errorf("no_growth: fd slope %.3f/sample >= epsilon %.3f",
			fdSlope, noGrowthEpsilonFD)
	}
	t.Logf("no_growth: SLOPE CHECK ASSERTED over %d samples", len(samples))
}

// linRegSlope computes the ordinary least-squares slope of y=yf(i) vs xs[i].
// Returns 0 when fewer than 2 points are provided.
func linRegSlope(xs []float64, yf func(int) float64) float64 {
	n := len(xs)
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumX2 float64
	for i, x := range xs {
		y := yf(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}
	fn := float64(n)
	denom := fn*sumX2 - sumX*sumX
	if denom == 0 {
		return 0
	}
	return (fn*sumXY - sumX*sumY) / denom
}

// ngCountFDs returns the number of open file descriptors for the current
// process, using /proc/self/fd (Linux) or /dev/fd (Darwin).
func ngCountFDs() int {
	if n, ok := ngReaddirCount("/proc/self/fd"); ok {
		return n
	}
	if n, ok := ngReaddirCount("/dev/fd"); ok {
		return n
	}
	return -1
}

func ngReaddirCount(path string) (int, bool) {
	d, err := os.Open(path) //nolint:gosec // system-managed FD directory
	if err != nil {
		return 0, false
	}
	defer func() { _ = d.Close() }() //nolint:errcheck // read-only
	names, err := d.Readdirnames(-1)
	if err != nil {
		return 0, false
	}
	return len(names), true
}

// ngWriteCSV writes samples to bench/soak/soak-artefacts/no-growth-<ts>.csv.
func ngWriteCSV(t *testing.T, samples []ngSample) {
	t.Helper()
	const outDir = "soak-artefacts"
	if err := os.MkdirAll(outDir, 0o750); err != nil { //nolint:gosec // output dir
		t.Logf("no_growth: mkdir soak-artefacts: %v", err)
		return
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	path := filepath.Join(outDir, fmt.Sprintf("no-growth-%s.csv", ts))
	f, err := os.Create(path) //nolint:gosec // path from fixed dir + timestamp
	if err != nil {
		t.Logf("no_growth: create csv: %v", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("no_growth: close csv: %v", err)
		}
	}()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"elapsed_s", "heap_alloc_bytes", "goroutines", "fds"})
	for _, s := range samples {
		_ = w.Write([]string{
			strconv.FormatFloat(s.elapsed.Seconds(), 'f', 1, 64),
			strconv.FormatFloat(s.heapAlloc, 'f', 0, 64),
			strconv.FormatFloat(s.goroutines, 'f', 0, 64),
			strconv.FormatFloat(s.fds, 'f', 0, 64),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Logf("no_growth: csv flush: %v", err)
		return
	}
	t.Logf("no_growth: CSV written to %s (%d rows)", path, len(samples))
}
