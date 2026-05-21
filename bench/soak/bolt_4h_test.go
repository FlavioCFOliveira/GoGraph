// Package main_test — 4-hour / 1024-connection Bolt soak test.
//
// TestBoltSoak_1024_4h is the full soak gate mandated by the reliability
// acceptance criteria. It is excluded from normal CI runs; activate it with:
//
//	SOAK_FULL=1 go test -run=TestBoltSoak_1024_4h -timeout=5h ./bench/soak/...
//
// CI uses TestBoltSoak_60s (32 goroutines × 10 s) instead.
package main_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"gograph/bolt/server"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// soakSnapshot records heap and goroutine state at one point in time.
type soakSnapshot struct {
	elapsed    time.Duration
	heapAlloc  uint64
	goroutines int
}

// TestBoltSoak_1024_4h runs a 1024-connection, 4-hour soak test.
// Skipped unless the SOAK_FULL=1 environment variable is set.
// The test emits heap/goroutine snapshots every 30 s.
// CI uses TestBoltSoak_60s instead.
func TestBoltSoak_1024_4h(t *testing.T) {
	if os.Getenv("SOAK_FULL") != "1" {
		t.Skip("set SOAK_FULL=1 to run full 4-hour soak")
	}

	const (
		nConns           = 1024
		duration         = 4 * time.Hour
		snapshotInterval = 30 * time.Second
	)

	// ── Build graph and engine ────────────────────────────────────────────────
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Pre-seed the graph with two Person nodes so MATCH returns non-empty results.
	seedNodes(t, eng)

	// ── Start server ─────────────────────────────────────────────────────────
	srv := server.NewServer(eng, server.Options{
		MaxConnections: nConns + 64, // 64-slot headroom above the soak concurrency
		ConnTimeout:    15 * time.Second,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	srvCtx, srvCancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(srvCtx, ln)
	}()

	// Allow the server a moment to start accepting connections.
	time.Sleep(20 * time.Millisecond)

	// ── Soak loop ─────────────────────────────────────────────────────────────
	// Start connection goroutines FIRST so the baseline is measured under
	// steady-state load, not before any goroutines are running.
	var (
		successes atomic.Uint64
		failures  atomic.Uint64
	)

	deadline := time.Now().Add(duration)

	var wg sync.WaitGroup
	wg.Add(nConns)
	for i := range nConns {
		go func(id int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				dialErr := boltDial(ctx, addr, "MATCH (n:Person) RETURN n")
				cancel()
				if dialErr != nil {
					failures.Add(1)
					// Transient errors (e.g. server backpressure) are tolerated;
					// they are counted but do not abort the goroutine.
					continue
				}
				successes.Add(1)
				runtime.Gosched()
			}
		}(i)
	}

	// ── Warmup ───────────────────────────────────────────────────────────────
	// Wait 60 s under full load so transient start-up allocations and goroutine
	// creation settle before the baseline snapshot is taken.
	time.Sleep(60 * time.Second)

	// ── Baseline heap measurement ─────────────────────────────────────────────
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	baselineGoroutines := runtime.NumGoroutine()
	t.Logf("soak_1024_4h: baseline heap=%d goroutines=%d", baseline.HeapAlloc, baselineGoroutines)

	// ── Snapshot collector ────────────────────────────────────────────────────
	var (
		snapshotMu sync.Mutex
		snapshots  []soakSnapshot
	)

	// Snapshot goroutine: fires every snapshotInterval until the deadline.
	snapshotDone := make(chan struct{})
	go func() {
		defer close(snapshotDone)
		ticker := time.NewTicker(snapshotInterval)
		defer ticker.Stop()
		start := time.Now()
		for range ticker.C {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			ng := runtime.NumGoroutine()
			elapsed := time.Since(start)
			t.Logf("soak_1024_4h: t=%v heap=%d goroutines=%d", elapsed.Truncate(time.Second), ms.HeapAlloc, ng)
			snapshotMu.Lock()
			snapshots = append(snapshots, soakSnapshot{elapsed: elapsed, heapAlloc: ms.HeapAlloc, goroutines: ng})
			snapshotMu.Unlock()
			if time.Now().After(deadline) {
				return
			}
		}
	}()

	wg.Wait()

	// Stop the snapshot goroutine (it exits once deadline has passed).
	<-snapshotDone

	t.Logf("soak_1024_4h: successes=%d failures=%d dur=%v goroutines=%d",
		successes.Load(), failures.Load(), duration, runtime.NumGoroutine())

	if successes.Load() == 0 {
		t.Error("soak_1024_4h: zero successful round-trips — server may not be responding")
	}

	// ── Heap stability check ──────────────────────────────────────────────────
	snapshotMu.Lock()
	allSnaps := snapshots
	snapshotMu.Unlock()

	// Heap stability: last-quartile average must not exceed 1.5× first-quartile
	// average. This detects monotonic growth without false-positives from GC
	// oscillation, which can cause peak-vs-baseline comparisons to fail spuriously.
	// The 1.5× threshold (tightened from 2×) catches slower leaks; the most recent
	// 4-hour run showed a ratio of 0.90× for heap, well within the tighter band.
	if len(allSnaps) >= 8 {
		q := len(allSnaps) / 4
		var sumFirst, sumLast uint64
		for _, s := range allSnaps[:q] {
			sumFirst += s.heapAlloc
		}
		for _, s := range allSnaps[len(allSnaps)-q:] {
			sumLast += s.heapAlloc
		}
		avgFirst := sumFirst / uint64(q)
		avgLast := sumLast / uint64(q)
		// Compare avgLast*2 > avgFirst*3 to express the 1.5× threshold without
		// floating-point arithmetic (avgLast > avgFirst*1.5 ⇔ avgLast*2 > avgFirst*3).
		if avgLast*2 > avgFirst*3 {
			t.Errorf("soak_1024_4h: heap leak: last-quartile avg %d > 1.5x first-quartile avg %d", avgLast, avgFirst)
		} else {
			t.Logf("soak_1024_4h: heap stable: first-quartile avg=%d last-quartile avg=%d", avgFirst, avgLast)
		}
	}

	// ── Goroutine stability check ─────────────────────────────────────────────
	// Goroutine stability: last-quartile average must not exceed 1.5× first-quartile
	// average. This detects goroutine leaks without triggering on the transient
	// burst at connection start-up. The most recent 4-hour run showed a ratio of
	// 0.98× for goroutines, well within the tighter band.
	if len(allSnaps) >= 8 {
		q := len(allSnaps) / 4
		var sumFirst, sumLast int
		for _, s := range allSnaps[:q] {
			sumFirst += s.goroutines
		}
		for _, s := range allSnaps[len(allSnaps)-q:] {
			sumLast += s.goroutines
		}
		avgFirst := sumFirst / q
		avgLast := sumLast / q
		if avgLast*2 > avgFirst*3 {
			t.Errorf("soak_1024_4h: goroutine leak: last-quartile avg %d > 1.5x first-quartile avg %d", avgLast, avgFirst)
		} else {
			t.Logf("soak_1024_4h: goroutine stable: first-quartile avg=%d last-quartile avg=%d", avgFirst, avgLast)
		}
	}

	// ── Write soak artefact ───────────────────────────────────────────────────
	if dir := os.Getenv("SOAK_ARTEFACTS"); dir != "" {
		writeSoakArtefact(t, dir, allSnaps, baseline.HeapAlloc, successes.Load(), failures.Load(), duration)
	}

	// ── Shutdown and goleak check ─────────────────────────────────────────────
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Errorf("soak_1024_4h shutdown: %v", err)
	}
	srvCancel()
	select {
	case <-serveErr:
	case <-time.After(5 * time.Second):
		t.Error("soak_1024_4h: Serve goroutine did not exit after shutdown")
	}

	goleak.VerifyNone(t)
}

// writeSoakArtefact writes a plain-text snapshot log to dir/bolt-soak-1024-4h.txt.
// Called only when SOAK_ARTEFACTS env var is set.
//
// Every failure path uses t.Errorf rather than t.Logf so a soak that completes
// the workload but cannot persist its evidence does not silently report PASS.
// When SOAK_ARTEFACTS is set, the caller is asking for archived evidence; if
// we cannot produce it, the run is not acceptable.
func writeSoakArtefact(t *testing.T, dir string, snaps []soakSnapshot, baselineHeap, successes, failures uint64, duration time.Duration) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // user-supplied output dir
		t.Errorf("soak_1024_4h: cannot create artefact dir %s: %v", dir, err)
		return
	}
	path := dir + "/bolt-soak-1024-4h.txt"
	f, err := os.Create(path) //nolint:gosec // path from env + fixed filename
	if err != nil {
		t.Errorf("soak_1024_4h: cannot create artefact file: %v", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("soak_1024_4h: artefact close: %v", err)
		}
	}()

	if _, err := fmt.Fprintf(f, "bolt soak 1024-conn / %v  successes=%d failures=%d baseline_heap=%d\n",
		duration, successes, failures, baselineHeap); err != nil {
		t.Errorf("soak_1024_4h: artefact write: %v", err)
	}
	for _, s := range snaps {
		if _, err := fmt.Fprintf(f, "  t=%-12v heap=%-12d goroutines=%d\n",
			s.elapsed.Truncate(time.Second), s.heapAlloc, s.goroutines); err != nil {
			t.Errorf("soak_1024_4h: artefact write snapshot: %v", err)
		}
	}
	t.Logf("soak_1024_4h: artefact written to %s", path)
}
