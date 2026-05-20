// cypher_rw.go — Cypher RW mixed-workload harness for the soak binary.
//
// Activated via -cypher-mode. Runs R reader goroutines issuing
// "MATCH (n) RETURN n" in a tight loop, one writer goroutine issuing
// CREATE / MERGE writes (serialised by a sync.Mutex), and one
// ctx-cancellation injector that punctures child contexts every
// cancelInterval to exercise context.Done() paths in the executor.
//
// Goroutine count, heap alloc, and FD count are logged on the
// -sample-interval cadence. The run exits cleanly on ctx cancellation
// (i.e. when -duration elapses).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// Flags defined here so main() can reference *flagCypherMode.
var (
	flagCypherMode     = flag.Bool("cypher-mode", false, "run Cypher RW mixed workload instead of BFS/Dijkstra soak")
	flagCypherWritePct = flag.Int("cypher-write-pct", 20, "percentage of writer-goroutine iterations that perform a write (0–100)")
)

const cancelInterval = 1 * time.Second

// runCypherRW is the entry point for the Cypher RW mixed-workload soak.
// It blocks until ctx is cancelled (i.e. -duration elapses), then
// returns so main() can exit cleanly.
func runCypherRW(ctx context.Context, outDir string) {
	log.Printf("cypher-rw: start readers=%d write-pct=%d sample-interval=%v duration=%v",
		*flagConcurrent, *flagCypherWritePct, *flagSampleN, *flagDuration)

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	// Seed with a handful of nodes so the initial MATCH scan is non-trivial.
	for i := range 32 {
		g.AddNode(fmt.Sprintf("seed_%d", i))
	}

	eng := cypher.NewEngine(g)
	var writeMu sync.Mutex // single-writer contract

	var (
		reads     atomic.Uint64
		writes    atomic.Uint64
		cancels   atomic.Uint64
		startTime = time.Now()
	)

	var wg sync.WaitGroup

	// Reader goroutines — each runs MATCH (n) RETURN n in a tight loop.
	for i := range *flagConcurrent {
		wg.Add(1)
		go cypherReader(ctx, &wg, eng, &reads, i)
	}

	// Single writer goroutine — serialised writes via writeMu.
	wg.Add(1)
	go cypherWriter(ctx, &wg, eng, &writeMu, &writes, *flagCypherWritePct)

	// Cancellation injector — cancels a child ctx every cancelInterval.
	wg.Add(1)
	go cypherCancelInjector(ctx, &wg, eng, &writeMu, &cancels)

	// Sampler — logs goroutine / heap metrics on -sample-interval.
	wg.Add(1)
	go cypherSampler(ctx, &wg, &reads, &writes, &cancels, startTime, outDir)

	wg.Wait()
	log.Printf("cypher-rw: complete reads=%d writes=%d cancels=%d elapsed=%v",
		reads.Load(), writes.Load(), cancels.Load(), time.Since(startTime).Truncate(time.Second))
}

// cypherReader repeatedly executes MATCH (n) RETURN n, drains the result,
// and yields the scheduler between iterations.
func cypherReader(ctx context.Context, wg *sync.WaitGroup, eng *cypher.Engine, reads *atomic.Uint64, id int) {
	defer wg.Done()
	pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("cypher-reader", fmt.Sprintf("%d", id))))
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		res, err := eng.Run(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			// ctx cancelled or parse error — both are expected during teardown.
			runtime.Gosched()
			continue
		}
		for res.Next() {
		}
		_ = res.Close() // drain + release; ignore error (may be ctx cancelled)
		reads.Add(1)
		runtime.Gosched()
	}
}

// writeQueries is the pool of confirmed-working write queries used by the
// writer and cancellation-injector goroutines. Labels are varied to exercise
// the label-index hot path.
var writeQueries = [...]string{
	"CREATE (n:Person)",
	"CREATE (n:City)",
	"MERGE (n:Person)",
	"MERGE (n:City)",
}

// cypherWriter issues CREATE / MERGE writes on a probabilistic write-pct split.
// All writes are serialised via writeMu (single-writer contract).
func cypherWriter(ctx context.Context, wg *sync.WaitGroup, eng *cypher.Engine, writeMu *sync.Mutex, writes *atomic.Uint64, writePct int) {
	defer wg.Done()
	pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("cypher-writer", "0")))
	r := rand.New(rand.NewPCG(17, 31)) //nolint:gosec // deterministic seed
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if r.IntN(100) >= writePct {
			// Read turn: read path is handled by the reader goroutines.
			continue
		}

		q := writeQueries[r.IntN(len(writeQueries))]
		writeMu.Lock()
		res, err := eng.RunInTx(ctx, q, nil)
		if err == nil {
			for res.Next() {
			}
			_ = res.Close()
		}
		writeMu.Unlock()
		if err == nil {
			writes.Add(1)
		}
	}
}

// cypherCancelInjector fires a child context cancellation every cancelInterval
// to exercise the context.Done() paths in the executor. It issues one read and
// one write with a short-lived derived context that is immediately cancelled
// after the call returns, simulating a client timeout.
func cypherCancelInjector(ctx context.Context, wg *sync.WaitGroup, eng *cypher.Engine, writeMu *sync.Mutex, cancels *atomic.Uint64) {
	defer wg.Done()
	pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("cypher-cancel-injector", "0")))
	ticker := time.NewTicker(cancelInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Inject cancel on a read.
		childCtx, cancel := context.WithCancel(ctx)
		cancel() // cancel immediately — executor should handle ctx.Done()
		res, err := eng.Run(childCtx, "MATCH (n) RETURN n", nil)
		if err == nil {
			for res.Next() {
			}
			_ = res.Close()
		}

		// Inject cancel on a write.
		childCtx2, cancel2 := context.WithCancel(ctx)
		cancel2()
		writeMu.Lock()
		res2, err2 := eng.RunInTx(childCtx2, "CREATE (n:Person)", nil)
		if err2 == nil {
			for res2.Next() {
			}
			_ = res2.Close()
		}
		writeMu.Unlock()

		cancels.Add(1)
	}
}

// cypherSampler logs goroutine count and heap alloc on the -sample-interval
// cadence. On macOS /proc is unavailable so FD count falls back to
// runtime.NumGoroutine() as a proxy (documented in the report).
func cypherSampler(ctx context.Context, wg *sync.WaitGroup, reads, writes, cancels *atomic.Uint64, startTime time.Time, outDir string) {
	defer wg.Done()
	pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("cypher-sampler", "0")))
	ticker := time.NewTicker(*flagSampleN)
	defer ticker.Stop()

	idx := 0
	dumpCypherHeap(idx, startTime, outDir)
	idx++

	for {
		select {
		case <-ctx.Done():
			dumpCypherHeap(idx, startTime, outDir)
			return
		case <-ticker.C:
		}

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		goroutines := runtime.NumGoroutine()
		fdCount := countFDs()

		log.Printf("cypher-rw: t=%v reads=%d writes=%d cancels=%d goroutines=%d heap_alloc=%d fd=%d",
			time.Since(startTime).Truncate(time.Second),
			reads.Load(), writes.Load(), cancels.Load(),
			goroutines, ms.HeapAlloc, fdCount)

		dumpCypherHeap(idx, startTime, outDir)
		idx++
	}
}

// countFDs returns the number of open file descriptors for the current
// process by counting entries in /proc/self/fd. On platforms where /proc
// is unavailable (e.g. macOS) it returns -1.
func countFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1 // not available on macOS / non-Linux
	}
	return len(entries)
}

// dumpCypherHeap writes a heap profile snapshot to outDir, following the
// same naming convention as the BFS/Dijkstra soak's dumpHeap.
func dumpCypherHeap(idx int, startTime time.Time, outDir string) {
	path := fmt.Sprintf("%s/cypher-heap-%03d.pb.gz", outDir, idx)
	f, err := os.Create(path) //nolint:gosec // path constructed from -out flag + numeric index
	if err != nil {
		log.Printf("cypher-rw: cannot create heap profile: %v", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("cypher-rw: heap profile close: %v", err)
		}
	}()
	runtime.GC()
	if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
		log.Printf("cypher-rw: heap profile write: %v", err)
		return
	}
	log.Printf("cypher-rw: heap snapshot %s @ t=%v", path, time.Since(startTime).Truncate(time.Second))
}
