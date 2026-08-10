//go:build soak || nightly

// Package main_test — GC pause stability soak test.
//
// TestGCPause_Stable runs a BFS/Dijkstra background workload and samples the
// runtime.MemStats.PauseNs ring buffer every 5 seconds. After a warm-up it
// fits a least-squares linear regression on recent GC pauses vs. time and
// asserts:
//
//   - |slope| < 10 ms / sample
//   - max pause < 200 ms
//
// Smoke run (default / -short): 30 s total measurement window.
// Full soak (SOAK_FULL=1, no -short): 30 min measurement window.
//
// GC pause samples are written as CSV to
// bench/soak/soak-artefacts/gc-pause-<ts>.csv.
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
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

const (
	gcPauseSampleInterval = 5 * time.Second
	gcPauseEpsilonMs      = 10.0  // ms per sample
	gcPauseMaxCeilingMs   = 200.0 // absolute max pause ceiling in ms
)

// gcPauseSample records one GC sampling event.
type gcPauseSample struct {
	idx    int
	maxNs  uint64 // max pause in the most recent GC round (ns)
	meanNs float64
}

// TestGCPause_Stable runs the GC pause stability soak.
func TestGCPause_Stable(t *testing.T) {
	// Short layer: 20 s, i.e. ~4 samples at the 5 s interval — enough for the
	// max-pause ceiling, below minRegressionPoints for the slope.
	// Full soak (SOAK_FULL=1, no -short): 30 min.
	measurement := 20 * time.Second
	if soakFullRun() {
		measurement = 30 * time.Minute
	}
	measurement = soakEnvDuration("SOAK_GCPAUSE_MEASURE", measurement)
	// Unlike no_growth, this loop is bounded by the context itself rather than by
	// a separate measureEnd, so the final tick races the deadline and the sample
	// count is the window divided by the interval, minus 0 or 1. That is
	// immaterial here: the ceiling needs one sample and the slope needs
	// minRegressionPoints, and neither bound sits at the boundary. Adding grace
	// would lengthen the short layer's runtime for no gain.
	t.Logf("gc_pause: config measurement=%v interval=%v (expect %d samples, need %d for the slope)",
		measurement, gcPauseSampleInterval,
		int(measurement/gcPauseSampleInterval), minRegressionPoints)

	// ── Build seed graph for background workload ──────────────────────────────
	const graphN = 1024
	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	rng := rand.New(rand.NewPCG(13, 17)) //nolint:gosec // deterministic
	for i := range graphN {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("seed AddNode %d: %v", i, err)
		}
	}
	for range graphN * 4 {
		_ = a.AddEdge(rng.IntN(graphN), rng.IntN(graphN), int64(rng.IntN(100)+1))
	}
	var snapPtr gcPauseSnapPtr
	snapPtr.store(csr.BuildFromAdjList(a))

	// ── Background workload ───────────────────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), measurement)
	defer cancel()

	const bgReaders = 4
	var wg sync.WaitGroup
	wg.Add(bgReaders)
	for i := range bgReaders {
		go func(id int) {
			defer wg.Done()
			rr := rand.New(rand.NewPCG(uint64(id)+200, 41)) //nolint:gosec // deterministic
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				done := metrics.Time(fmt.Sprintf("bench.soak.gcpause.reader.%d", id))
				snap := snapPtr.load()
				src := graph.NodeID(rr.IntN(int(snap.MaxNodeID())))
				if rr.IntN(2) == 0 {
					search.BFS(snap, src, func(_ graph.NodeID, _ int) bool { return true })
				} else {
					_, _ = search.Dijkstra(snap, src)
				}
				done.Stop()
				runtime.Gosched()
			}
		}(i)
	}

	// ── Measurement: poll PauseNs ring buffer ─────────────────────────────────
	// PauseNs is a circular buffer of the last 256 GC pauses.
	// To avoid double-counting, we track which NumGC values we've already seen.
	var samples []gcPauseSample
	ticker := time.NewTicker(gcPauseSampleInterval)
	defer ticker.Stop()
	idx := 0
	var prevNumGC uint32

	for {
		select {
		case <-ctx.Done():
			goto done
		case <-ticker.C:
		}

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		if ms.NumGC == prevNumGC {
			// No GC occurred since last sample — record a zero pause.
			samples = append(samples, gcPauseSample{idx: idx, maxNs: 0, meanNs: 0})
			idx++
			continue
		}

		// Find new pauses since last sample.
		// PauseNs is indexed by (NumGC+255) % 256 for the most recent pause.
		var maxPause uint64
		var totalPause uint64
		var pauseCount uint32
		newGCs := ms.NumGC - prevNumGC
		if newGCs > 256 {
			newGCs = 256
		}
		for i := uint32(0); i < newGCs; i++ {
			ringIdx := (ms.NumGC - 1 - i + 256) % 256
			p := ms.PauseNs[ringIdx]
			if p > maxPause {
				maxPause = p
			}
			totalPause += p
			pauseCount++
		}
		var meanNs float64
		if pauseCount > 0 {
			meanNs = float64(totalPause) / float64(pauseCount)
		}
		prevNumGC = ms.NumGC

		samples = append(samples, gcPauseSample{idx: idx, maxNs: maxPause, meanNs: meanNs})
		t.Logf("gc_pause: sample=%d max_pause=%.2fms mean_pause=%.2fms num_gc=%d",
			idx,
			float64(maxPause)/float64(time.Millisecond),
			meanNs/float64(time.Millisecond),
			ms.NumGC)
		idx++
	}

done:
	cancel()
	wg.Wait()

	// ── Write CSV artefact ────────────────────────────────────────────────────
	gcPauseWriteCSV(t, samples)

	// ── Assertions ────────────────────────────────────────────────────────────
	goleak.VerifyNone(t)

	// The max-pause CEILING is checked first and independently of the sample
	// count, because a ceiling needs no regression: one sample is enough to
	// exceed 200 ms. Until rmp #2396 this check sat *after* an early return taken
	// when fewer than 2 samples existed, so a run that happened to collect one
	// sample skipped the ceiling too — and "max GC pause 0.19 ms against a 200 ms
	// ceiling" is the strongest single figure the 2026-08-10 certification
	// quotes. A ceiling assertion must not be contingent on a regression's
	// preconditions.
	if len(samples) == 0 {
		t.Fatal("gc_pause: no samples collected at all; neither the ceiling nor the slope was evaluated")
	}
	var overallMax uint64
	for _, s := range samples {
		if s.maxNs > overallMax {
			overallMax = s.maxNs
		}
	}
	maxPauseMs := float64(overallMax) / float64(time.Millisecond)
	t.Logf("gc_pause: overall max pause=%.2fms (ceiling=%.1fms) ASSERTED over %d samples",
		maxPauseMs, gcPauseMaxCeilingMs, len(samples))
	if maxPauseMs >= gcPauseMaxCeilingMs {
		t.Errorf("gc_pause: max pause %.2fms >= ceiling %.1fms", maxPauseMs, gcPauseMaxCeilingMs)
	}

	// The slope, by contrast, does depend on having enough points.
	if !requireRegressionPoints(t, "gc_pause", len(samples),
		"run with SOAK_GCPAUSE_MEASURE=60s for 12 samples, or SOAK_FULL=1 for the 30-minute variant",
		"SOAK_GCPAUSE_MEASURE") {
		return
	}

	xs := make([]float64, len(samples))
	for i := range samples {
		xs[i] = float64(i)
	}
	slope := linRegSlope(xs, func(i int) float64 {
		return float64(samples[i].maxNs) / float64(time.Millisecond)
	})
	t.Logf("gc_pause: pause_max slope=%.3f ms/sample (epsilon=%.1f)", slope, gcPauseEpsilonMs)
	if math.Abs(slope) >= gcPauseEpsilonMs {
		t.Errorf("gc_pause: pause slope %.3f ms/sample >= epsilon %.1f",
			slope, gcPauseEpsilonMs)
	}
	t.Logf("gc_pause: SLOPE CHECK ASSERTED over %d samples", len(samples))
}

// gcPauseWriteCSV writes the pause samples to soak-artefacts/gc-pause-<ts>.csv.
func gcPauseWriteCSV(t *testing.T, samples []gcPauseSample) {
	t.Helper()
	const outDir = "soak-artefacts"
	if err := os.MkdirAll(outDir, 0o750); err != nil { //nolint:gosec // output dir
		t.Logf("gc_pause: mkdir soak-artefacts: %v", err)
		return
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	path := filepath.Join(outDir, fmt.Sprintf("gc-pause-%s.csv", ts))
	f, err := os.Create(path) //nolint:gosec // path from fixed dir + timestamp
	if err != nil {
		t.Logf("gc_pause: create csv: %v", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("gc_pause: close csv: %v", err)
		}
	}()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"sample_idx", "max_pause_ns", "mean_pause_ns"})
	for _, s := range samples {
		_ = w.Write([]string{
			strconv.Itoa(s.idx),
			strconv.FormatUint(s.maxNs, 10),
			strconv.FormatFloat(s.meanNs, 'f', 0, 64),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Logf("gc_pause: csv flush: %v", err)
		return
	}
	t.Logf("gc_pause: CSV written to %s (%d rows)", path, len(samples))
}

// gcPauseSnapPtr is a thin wrapper around atomic.Pointer[csr.CSR[int64]].
type gcPauseSnapPtr struct {
	inner atomic.Pointer[csr.CSR[int64]]
}

func (s *gcPauseSnapPtr) store(c *csr.CSR[int64]) { s.inner.Store(c) }
func (s *gcPauseSnapPtr) load() *csr.CSR[int64]   { return s.inner.Load() }
