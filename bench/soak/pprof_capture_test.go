//go:build soak || nightly

// Package main_test — pprof CPU + heap profile capture soak test.
//
// TestPprofCapture verifies that the soak harness can periodically capture
// CPU and heap pprof profiles and write them to artefact files.
//
// Smoke (testing.Short() or SOAK_FULL unset): 2 snapshots at 5-second intervals.
// Full soak (SOAK_FULL=1, no -short): 8 snapshots at 30-minute intervals (= 4h total).
//
// For each snapshot:
//  1. CPU profile: pprof.StartCPUProfile → sleep 2s (smoke) / 30s (soak) → pprof.StopCPUProfile
//  2. Heap profile: pprof.WriteHeapProfile
//  3. Both written to bench/soak/soak-artefacts/cpu-NNN-<ts>.pb.gz and heap-NNN-<ts>.pb.gz
//
// After all captures, each file is verified to be > 512 bytes (non-empty profile).
// goleak.VerifyNone(t) is called at the end.
package main_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
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
	pprofMinFileBytes = 512 // minimum acceptable profile file size in bytes
)

// TestPprofCapture verifies periodic pprof artefact capture.
func TestPprofCapture(t *testing.T) {
	snapshots := 2
	cpuDur := 2 * time.Second
	interval := 5 * time.Second
	if !testing.Short() && os.Getenv("SOAK_FULL") == "1" {
		snapshots = 8
		cpuDur = 30 * time.Second
		interval = 30 * time.Minute
	}

	const outDir = "soak-artefacts"
	if err := os.MkdirAll(outDir, 0o750); err != nil { //nolint:gosec // output dir
		t.Fatalf("pprof_capture: mkdir: %v", err)
	}

	// ── Background workload (ensures CPU profiles are non-trivial) ────────────
	const graphN = 512
	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	rng := rand.New(rand.NewPCG(23, 29)) //nolint:gosec // deterministic
	for i := range graphN {
		if err := a.AddNode(i); err != nil {
			t.Fatalf("seed AddNode %d: %v", i, err)
		}
	}
	for range graphN * 4 {
		_ = a.AddEdge(rng.IntN(graphN), rng.IntN(graphN), int64(rng.IntN(100)+1))
	}
	var snapPtr pprofSnapPtr
	snapPtr.store(csr.BuildFromAdjList(a))

	totalDur := time.Duration(snapshots) * (cpuDur + interval)
	ctx, cancel := context.WithTimeout(context.Background(), totalDur)
	defer cancel()

	const bgReaders = 4
	var wg sync.WaitGroup
	wg.Add(bgReaders)
	for i := range bgReaders {
		go func(id int) {
			defer wg.Done()
			rr := rand.New(rand.NewPCG(uint64(id)+300, 53)) //nolint:gosec // deterministic
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				done := metrics.Time(fmt.Sprintf("bench.soak.pprof.reader.%d", id))
				snap := snapPtr.load()
				src := graph.NodeID(rr.IntN(int(snap.MaxNodeID())))
				if rr.IntN(2) == 0 {
					search.BFS(snap, src, func(_ graph.NodeID, _ int) bool { return true })
				} else {
					_, _ = search.Dijkstra(snap, src)
				}
				done()
				runtime.Gosched()
			}
		}(i)
	}

	// ── Capture loop ──────────────────────────────────────────────────────────
	var capturedFiles []string
	ts := time.Now().UTC().Format("20060102T150405Z")

	for i := range snapshots {
		// Wait for the inter-snapshot interval (skip on first snapshot).
		if i > 0 {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				t.Logf("pprof_capture: context cancelled before snapshot %d", i)
				goto done
			}
		}

		snapTS := time.Now().UTC().Format("150405Z")

		// CPU profile ─────────────────────────────────────────────────────────
		cpuPath := filepath.Join(outDir, fmt.Sprintf("cpu-%03d-%s-%s.pb.gz", i, ts, snapTS))
		cpuFile, err := os.Create(cpuPath) //nolint:gosec // path from fixed dir + index + timestamp
		if err != nil {
			t.Errorf("pprof_capture: create cpu file %d: %v", i, err)
			continue
		}
		if err := pprof.StartCPUProfile(cpuFile); err != nil {
			_ = cpuFile.Close()
			t.Errorf("pprof_capture: StartCPUProfile %d: %v", i, err)
			continue
		}
		// Let the profiler run for cpuDur.
		select {
		case <-time.After(cpuDur):
		case <-ctx.Done():
			pprof.StopCPUProfile()
			_ = cpuFile.Close()
			t.Logf("pprof_capture: context cancelled during CPU profile %d", i)
			goto done
		}
		pprof.StopCPUProfile()
		if err := cpuFile.Close(); err != nil {
			t.Errorf("pprof_capture: close cpu file %d: %v", i, err)
		} else {
			capturedFiles = append(capturedFiles, cpuPath)
			metrics.IncCounter("bench.soak.pprof.cpu_captures", 1)
			t.Logf("pprof_capture: cpu snapshot %d written to %s", i, cpuPath)
		}

		// Heap profile ────────────────────────────────────────────────────────
		heapPath := filepath.Join(outDir, fmt.Sprintf("heap-%03d-%s-%s.pb.gz", i, ts, snapTS))
		heapFile, err := os.Create(heapPath) //nolint:gosec // path from fixed dir + index + timestamp
		if err != nil {
			t.Errorf("pprof_capture: create heap file %d: %v", i, err)
			continue
		}
		runtime.GC()
		if err := pprof.WriteHeapProfile(heapFile); err != nil {
			_ = heapFile.Close()
			t.Errorf("pprof_capture: WriteHeapProfile %d: %v", i, err)
			continue
		}
		if err := heapFile.Close(); err != nil {
			t.Errorf("pprof_capture: close heap file %d: %v", i, err)
		} else {
			capturedFiles = append(capturedFiles, heapPath)
			metrics.IncCounter("bench.soak.pprof.heap_captures", 1)
			t.Logf("pprof_capture: heap snapshot %d written to %s", i, heapPath)
		}
	}

done:
	cancel()
	wg.Wait()

	// ── Verify all files are non-empty ────────────────────────────────────────
	var badFiles int
	for _, path := range capturedFiles {
		fi, err := os.Stat(path)
		if err != nil {
			t.Errorf("pprof_capture: stat %s: %v", path, err)
			badFiles++
			continue
		}
		if fi.Size() < pprofMinFileBytes {
			t.Errorf("pprof_capture: file too small: %s size=%d (< %d bytes)",
				path, fi.Size(), pprofMinFileBytes)
			badFiles++
		}
	}

	t.Logf("pprof_capture: captured %d files, %d bad (< %d bytes)",
		len(capturedFiles), badFiles, pprofMinFileBytes)

	if len(capturedFiles) == 0 {
		t.Error("pprof_capture: no profiles captured")
	}

	goleak.VerifyNone(t)
}

// pprofSnapPtr wraps atomic.Pointer[csr.CSR[int64]] for this file.
type pprofSnapPtr struct {
	inner atomic.Pointer[csr.CSR[int64]]
}

func (s *pprofSnapPtr) store(c *csr.CSR[int64]) { s.inner.Store(c) }
func (s *pprofSnapPtr) load() *csr.CSR[int64]   { return s.inner.Load() }
