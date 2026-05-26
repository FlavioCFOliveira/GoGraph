//go:build soak

// Package main_test — Cypher RW analytics extension soak test.
//
// TestCypherRW_Analytics_30m (smoke alias: TestCypherRW_Analytics_Smoke) runs
// a mixed workload of R concurrent readers, one CREATE/MERGE writer, one
// PageRank analytics goroutine, and one context-cancellation injector for
// either 5 seconds (smoke) or 30 minutes (SOAK_FULL=1).
//
// Verified invariants:
//   - No panic under concurrent analytics + write pressure.
//   - No goroutine leak at teardown (goleak.VerifyNone).
//   - Heap alloc does not double between start and end.
package main_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
	"gograph/search/centrality"
)

const (
	analyticsReaders       = 4
	analyticsWriteInterval = 50 * time.Millisecond
	analyticsPageRankEvery = 10 * time.Second
	analyticsCancelEvery   = 1 * time.Second
)

// TestCypherRW_Analytics_Smoke is the smoke alias (also used as the full-soak
// entry point when SOAK_FULL=1). Under testing.Short() it runs for 5 s;
// without -short it runs for 5 s unless SOAK_FULL=1, in which case 30 min.
func TestCypherRW_Analytics_Smoke(t *testing.T) {
	dur := 5 * time.Second
	if !testing.Short() && os.Getenv("SOAK_FULL") == "1" {
		dur = 30 * time.Minute
	}
	runCypherAnalytics(t, dur)
}

// TestCypherRW_Analytics_30m is an explicit 30-minute alias, activated only
// when SOAK_FULL=1 and -short is not set.
func TestCypherRW_Analytics_30m(t *testing.T) {
	if testing.Short() || os.Getenv("SOAK_FULL") != "1" {
		t.Skip("TestCypherRW_Analytics_30m: requires SOAK_FULL=1 and no -short")
	}
	runCypherAnalytics(t, 30*time.Minute)
}

// runCypherAnalytics is the shared harness.
func runCypherAnalytics(t *testing.T, dur time.Duration) {
	t.Helper()

	// ── Resource accounting ───────────────────────────────────────────────────
	runtime.GC()
	var msStart runtime.MemStats
	runtime.ReadMemStats(&msStart)
	goroutinesStart := runtime.NumGoroutine()
	t.Logf("analytics_soak: start heap=%d goroutines=%d dur=%v",
		msStart.HeapAlloc, goroutinesStart, dur)

	// ── Build graph + engine ──────────────────────────────────────────────────
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	for i := range 100 {
		res, err := eng.RunInTx(context.Background(),
			fmt.Sprintf(`CREATE (n:Seed {id: %d})`, i), nil)
		if err != nil {
			t.Fatalf("seed node %d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("seed close %d: %v", i, err)
		}
	}

	// ── Shared adjlist snapshot for CSR rebuild ───────────────────────────────
	// The analytics goroutine works on an atomic CSR snapshot to avoid
	// holding the engine lock during PageRank iteration.
	al := adjlist.New[string, float64](adjlist.Config{Directed: true})
	for i := range 100 {
		if err := al.AddNode(fmt.Sprintf("seed_%d", i)); err != nil {
			t.Fatalf("adjlist seed %d: %v", i, err)
		}
	}
	// Build an initial dense graph so PageRank has edges to traverse.
	for i := range 100 {
		for j := range 5 {
			_ = al.AddEdge(
				fmt.Sprintf("seed_%d", i),
				fmt.Sprintf("seed_%d", (i+j+1)%100),
				1.0,
			)
		}
	}
	var snapPtr atomic.Pointer[csr.CSR[float64]]
	snapPtr.Store(csr.BuildFromAdjList(al))

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	var (
		reads    atomic.Uint64
		writes   atomic.Uint64
		analyses atomic.Uint64
		cancels  atomic.Uint64
		wg       sync.WaitGroup
	)

	// ── Readers ───────────────────────────────────────────────────────────────
	for i := range analyticsReaders {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			pprof.SetGoroutineLabels(pprof.WithLabels(ctx,
				pprof.Labels("analytics-reader", fmt.Sprintf("%d", id))))
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				stop := metrics.Time("bench.soak.analytics.read")
				res, err := eng.Run(ctx, "MATCH (n) RETURN n", nil)
				stop()
				if err != nil {
					runtime.Gosched()
					continue
				}
				for res.Next() {
				}
				_ = res.Close()
				reads.Add(1)
				runtime.Gosched()
			}
		}(i)
	}

	// ── Writer ────────────────────────────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("analytics-writer", "0")))
		ticker := time.NewTicker(analyticsWriteInterval)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			q := fmt.Sprintf(`CREATE (n:Dynamic {id: %d})`, i)
			i++
			stop := metrics.Time("bench.soak.analytics.write")
			res, err := eng.RunInTx(ctx, q, nil)
			stop()
			if err != nil {
				continue
			}
			for res.Next() {
			}
			_ = res.Close()
			writes.Add(1)
		}
	}()

	// ── Analytics goroutine (PageRank on CSR snapshot) ────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("analytics-pagerank", "0")))
		ticker := time.NewTicker(analyticsPageRankEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			snap := snapPtr.Load()
			if snap == nil || snap.MaxNodeID() == 0 {
				continue
			}
			stopPR := metrics.Time("bench.soak.analytics.pagerank")
			_, _, err := centrality.PageRankCtx(ctx, snap, centrality.DefaultPageRankOptions())
			stopPR()
			if err != nil {
				// ctx cancelled or empty graph — both are expected during teardown.
				log.Printf("analytics_soak: PageRankCtx: %v", err)
			}
			analyses.Add(1)
		}
	}()

	// ── Cancellation injector ─────────────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("analytics-cancel-injector", "0")))
		ticker := time.NewTicker(analyticsCancelEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			child, childCancel := context.WithCancel(ctx)
			childCancel() // cancel immediately
			res, err := eng.Run(child, "MATCH (n) RETURN n", nil)
			if err == nil {
				for res.Next() {
				}
				_ = res.Close()
			}
			cancels.Add(1)
		}
	}()

	wg.Wait()

	// ── Post-soak accounting ──────────────────────────────────────────────────
	runtime.GC()
	var msEnd runtime.MemStats
	runtime.ReadMemStats(&msEnd)
	goroutinesEnd := runtime.NumGoroutine()
	t.Logf("analytics_soak: end heap=%d goroutines=%d reads=%d writes=%d analyses=%d cancels=%d",
		msEnd.HeapAlloc, goroutinesEnd, reads.Load(), writes.Load(), analyses.Load(), cancels.Load())

	if msStart.HeapAlloc > 0 && msEnd.HeapAlloc > msStart.HeapAlloc*2 {
		t.Errorf("analytics_soak: heap doubled start=%d end=%d", msStart.HeapAlloc, msEnd.HeapAlloc)
	}

	goleak.VerifyNone(t)
}
