//go:build soak

// Package main_test — latency p99 stability soak test.
//
// TestLatencyP99_Stable measures round-trip latency for
// "MATCH (n) RETURN count(n)" via direct Bolt TCP, collecting per-request
// timings and computing p99 in 30-second histogram windows.
//
// After a warm-up (first 2 windows) it fits a least-squares linear
// regression on p99-over-time and asserts |slope| < 5 ms/window.
//
// Smoke (default / -short): 30 s with 8 goroutines.
// Full soak (SOAK_FULL=1, no -short): 4 h with 64 goroutines.
//
// p99-over-time is written as CSV to bench/soak/soak-artefacts/p99-<ts>.csv.
package main_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

const (
	p99WindowSize       = 30 * time.Second
	p99WarmupWindows    = 2   // windows excluded from regression
	p99EpsilonLatencyMs = 5.0 // ms per window
)

// p99Window accumulates per-request latency samples for one 30-second window.
type p99Window struct {
	windowIdx int
	p99Ms     float64 // p99 latency in milliseconds
}

// p99Histogram is a concurrent accumulator for latency samples within a window.
type p99Histogram struct {
	mu      sync.Mutex
	samples []time.Duration
}

func (h *p99Histogram) Record(d time.Duration) {
	h.mu.Lock()
	h.samples = append(h.samples, d)
	h.mu.Unlock()
}

// P99Ms returns the p99 of recorded samples in milliseconds and resets the
// accumulator for the next window. Returns -1 if no samples were recorded.
func (h *p99Histogram) P99Ms() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) == 0 {
		return -1
	}
	sorted := make([]time.Duration, len(h.samples))
	copy(sorted, h.samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(float64(len(sorted))*0.99)) - 1
	if idx < 0 {
		idx = 0
	}
	p99 := float64(sorted[idx]) / float64(time.Millisecond)
	h.samples = h.samples[:0] // reset for next window
	return p99
}

// TestLatencyP99_Stable runs the p99 stability soak.
func TestLatencyP99_Stable(t *testing.T) {
	// Smoke: 10 s with 8 goroutines (collects latency samples, skips regression
	// when fewer than 2 post-warm-up windows exist — that is expected for smoke).
	// Full soak (SOAK_FULL=1, no -short): 4 h with 64 goroutines.
	nGoroutines := 8
	dur := 10 * time.Second
	if !testing.Short() && os.Getenv("SOAK_FULL") == "1" {
		nGoroutines = 64
		dur = 4 * time.Hour
	}

	// ── Build server ──────────────────────────────────────────────────────────
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for i := range 16 {
		res, err := eng.RunInTx(context.Background(),
			fmt.Sprintf(`CREATE (n:P99Seed {id: %d})`, i), nil)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("seed close %d: %v", i, err)
		}
	}

	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: nGoroutines + 64,
		ConnTimeout:    15 * time.Second,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
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
	time.Sleep(20 * time.Millisecond)

	// ── Workload loop ─────────────────────────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	histo := &p99Histogram{}
	var successes, failures atomic.Uint64
	const query = "MATCH (n) RETURN count(n)"

	var wg sync.WaitGroup
	wg.Add(nGoroutines)
	for i := range nGoroutines {
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				start := time.Now()
				done := metrics.Time(fmt.Sprintf("bench.soak.p99.goroutine.%d", id))
				dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
				dialErr := boltDial(dialCtx, addr, query)
				dialCancel()
				done()
				if dialErr != nil {
					failures.Add(1)
					runtime.Gosched()
					continue
				}
				histo.Record(time.Since(start))
				successes.Add(1)
				runtime.Gosched()
			}
		}(i)
	}

	// ── Window collector ──────────────────────────────────────────────────────
	var windows []p99Window
	windowTicker := time.NewTicker(p99WindowSize)
	defer windowTicker.Stop()
	windowIdx := 0
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		for {
			select {
			case <-ctx.Done():
				// Collect final partial window.
				if p := histo.P99Ms(); p >= 0 {
					windows = append(windows, p99Window{windowIdx, p})
				}
				return
			case <-windowTicker.C:
				p := histo.P99Ms()
				if p >= 0 {
					windows = append(windows, p99Window{windowIdx, p})
					t.Logf("p99_stable: window=%d p99=%.2fms successes=%d failures=%d",
						windowIdx, p, successes.Load(), failures.Load())
				}
				windowIdx++
			}
		}
	}()

	wg.Wait()
	<-collectDone

	// ── Write CSV artefact ────────────────────────────────────────────────────
	p99WriteCSV(t, windows)

	// ── Shutdown ──────────────────────────────────────────────────────────────
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Errorf("p99_stable: shutdown: %v", err)
	}
	srvCancel()
	select {
	case <-serveErr:
	case <-time.After(10 * time.Second):
		t.Error("p99_stable: Serve goroutine did not exit after shutdown")
	}

	// ── Regression ───────────────────────────────────────────────────────────
	t.Logf("p99_stable: successes=%d failures=%d windows=%d", successes.Load(), failures.Load(), len(windows))
	if successes.Load() == 0 {
		t.Error("p99_stable: zero successful round-trips")
	}

	// Only include post-warm-up windows in the regression.
	postWarmup := windows
	if len(windows) > p99WarmupWindows {
		postWarmup = windows[p99WarmupWindows:]
	}
	if len(postWarmup) < 2 {
		t.Log("p99_stable: insufficient post-warmup windows for regression; skipping slope check")
		goleak.VerifyNone(t)
		return
	}

	xs := make([]float64, len(postWarmup))
	for i := range postWarmup {
		xs[i] = float64(i)
	}
	slope := linRegSlope(xs, func(i int) float64 { return postWarmup[i].p99Ms })
	t.Logf("p99_stable: p99 slope=%.3f ms/window (epsilon=%.1f)", slope, p99EpsilonLatencyMs)
	if math.Abs(slope) >= p99EpsilonLatencyMs {
		t.Errorf("p99_stable: p99 slope %.3f ms/window >= epsilon %.1f",
			slope, p99EpsilonLatencyMs)
	}

	goleak.VerifyNone(t)
}

// p99WriteCSV writes the per-window p99 trace to soak-artefacts/p99-<ts>.csv.
func p99WriteCSV(t *testing.T, windows []p99Window) {
	t.Helper()
	const outDir = "soak-artefacts"
	if err := os.MkdirAll(outDir, 0o750); err != nil { //nolint:gosec // output dir
		t.Logf("p99_stable: mkdir soak-artefacts: %v", err)
		return
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	path := filepath.Join(outDir, fmt.Sprintf("p99-%s.csv", ts))
	f, err := os.Create(path) //nolint:gosec // path from fixed dir + timestamp
	if err != nil {
		t.Logf("p99_stable: create csv: %v", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Logf("p99_stable: close csv: %v", err)
		}
	}()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"window_idx", "p99_ms"})
	for _, win := range windows {
		_ = w.Write([]string{
			strconv.Itoa(win.windowIdx),
			strconv.FormatFloat(win.p99Ms, 'f', 3, 64),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Logf("p99_stable: csv flush: %v", err)
		return
	}
	t.Logf("p99_stable: CSV written to %s (%d windows)", path, len(windows))
}
