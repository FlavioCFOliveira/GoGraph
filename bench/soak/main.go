// Command soak runs the long-form mixed-workload reliability test
// mandated by the project's reliability acceptance gate. It exercises
// concurrent search reads (BFS, Dijkstra), graph mutation (AddEdge),
// CSR rebuild + snapshot pointer-swap, and checkpoint/recovery cycles,
// and emits heap / FD / goroutine snapshots on a fixed cadence so the
// run can be audited for steady-state behaviour.
//
// The acceptance gate is: post-warmup heap delta < 5 %, FD count
// steady, goroutine count steady — see docs/benchmarks/SOAK.md for
// how the artefacts are inspected.
//
// Default duration is 4 hours; use -duration to shorten the run for
// smoke-testing the harness itself.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
)

var (
	flagDuration   = flag.Duration("duration", 4*time.Hour, "total run duration")
	flagSampleN    = flag.Duration("sample-interval", 30*time.Minute, "interval between heap/FD/goroutine snapshots")
	flagConcurrent = flag.Int("readers", 8, "number of concurrent reader goroutines")
	flagSize       = flag.Int("graph-size", 1<<14, "initial graph node count")
	flagOutDir     = flag.String("out", "soak-artefacts", "directory for heap-profile snapshots")
)

// minHeapProfileBytes is the minimum acceptable size for a heap snapshot
// written by pprof.Lookup("heap").WriteTo. A real heap profile (gzipped pprof
// protobuf) is at least a few KiB; anything below this threshold indicates
// truncation, a write error swallowed by the runtime, or an empty profile —
// none of which is acceptable for archived soak evidence.
const minHeapProfileBytes = 1024

// invalidHeapSnapshots counts truncated/empty heap profile writes during the
// soak. The soak binary exits non-zero if any invalid snapshot is detected so
// the operator and CI cannot mistake a corrupted run for a clean one.
var invalidHeapSnapshots atomic.Uint64

func main() {
	os.Exit(run())
}

// run executes the soak harness and returns the process exit code so that
// deferred cleanup (context cancel, timer stops) runs before the process
// terminates. Returns 1 if any heap snapshot was below the minimum size
// threshold; 0 otherwise.
func run() int {
	flag.Parse()
	if err := os.MkdirAll(*flagOutDir, 0o750); err != nil { //nolint:gosec // owner-visible profile dir
		log.Printf("mkdir out: %v", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *flagDuration)
	defer cancel()

	if *flagCypherMode {
		runCypherRW(ctx, *flagOutDir)
		if n := invalidHeapSnapshots.Load(); n > 0 {
			log.Printf("cypher-rw: %d invalid heap snapshot(s) detected — see INVALID HEAP SNAPSHOT log lines above", n)
			return 1
		}
		return 0
	}
	log.Printf("soak: duration=%v readers=%d size=%d sample-interval=%v out=%s",
		*flagDuration, *flagConcurrent, *flagSize, *flagSampleN, *flagOutDir)

	a := buildSeedGraph(*flagSize)
	var snap atomic.Pointer[csr.CSR[int64]]
	snap.Store(csr.BuildFromAdjList(a))

	var (
		reads     atomic.Uint64
		writes    atomic.Uint64
		rebuilds  atomic.Uint64
		startTime = time.Now()
	)

	var wg sync.WaitGroup
	for i := 0; i < *flagConcurrent; i++ {
		wg.Add(1)
		go reader(ctx, &wg, &snap, &reads, i)
	}
	wg.Add(1)
	go writer(ctx, &wg, a, &snap, &writes, &rebuilds)
	wg.Add(1)
	go sampler(ctx, &wg, &reads, &writes, &rebuilds, startTime)

	wg.Wait()
	log.Printf("soak: complete reads=%d writes=%d rebuilds=%d elapsed=%v",
		reads.Load(), writes.Load(), rebuilds.Load(), time.Since(startTime))
	if n := invalidHeapSnapshots.Load(); n > 0 {
		log.Printf("soak: %d invalid heap snapshot(s) detected — see INVALID HEAP SNAPSHOT log lines above", n)
		return 1
	}
	return 0
}

func buildSeedGraph(n int) *adjlist.AdjList[int, int64] {
	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	r := rand.New(rand.NewPCG(53, 59)) //nolint:gosec // deterministic seed
	for i := 0; i < n; i++ {
		if err := a.AddNode(i); err != nil {
			log.Fatalf("soak: seed AddNode(%d): %v", i, err)
		}
	}
	for i := 0; i < 4*n; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(100)+1)); err != nil {
			log.Fatalf("soak: seed AddEdge[%d]: %v", i, err)
		}
	}
	return a
}

func reader(ctx context.Context, wg *sync.WaitGroup, snap *atomic.Pointer[csr.CSR[int64]], reads *atomic.Uint64, id int) {
	defer wg.Done()
	pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("soak-reader", fmt.Sprintf("%d", id))))
	r := rand.New(rand.NewPCG(uint64(id)+1, 13)) //nolint:gosec // deterministic
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		c := snap.Load()
		src := graph.NodeID(r.IntN(int(c.MaxNodeID())))
		switch r.IntN(2) {
		case 0:
			search.BFS(c, src, func(_ graph.NodeID, _ int) bool { return true })
		case 1:
			_, _ = search.Dijkstra(c, src)
		}
		reads.Add(1)
	}
}

func writer(ctx context.Context, wg *sync.WaitGroup, a *adjlist.AdjList[int, int64], snap *atomic.Pointer[csr.CSR[int64]], writes, rebuilds *atomic.Uint64) {
	defer wg.Done()
	pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("soak-writer", "0")))
	r := rand.New(rand.NewPCG(91, 97)) //nolint:gosec // deterministic
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	rebuildTicker := time.NewTicker(2 * time.Second)
	defer rebuildTicker.Stop()
	n := int(a.MaxNodeID())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(100)+1)); err != nil {
				// Soak is uncapped; an error here is a programmer bug.
				log.Fatalf("soak: writer AddEdge: %v", err)
			}
			writes.Add(1)
		case <-rebuildTicker.C:
			snap.Store(csr.BuildFromAdjList(a))
			rebuilds.Add(1)
		}
	}
}

func sampler(ctx context.Context, wg *sync.WaitGroup, reads, writes, rebuilds *atomic.Uint64, startTime time.Time) {
	defer wg.Done()
	pprof.SetGoroutineLabels(pprof.WithLabels(ctx, pprof.Labels("soak-sampler", "0")))
	ticker := time.NewTicker(*flagSampleN)
	defer ticker.Stop()
	idx := 0
	dumpHeap(idx, startTime)
	idx++
	for {
		select {
		case <-ctx.Done():
			dumpHeap(idx, startTime)
			return
		case <-ticker.C:
			dumpHeap(idx, startTime)
			idx++
			log.Printf("soak: t=%v reads=%d writes=%d rebuilds=%d goroutines=%d",
				time.Since(startTime).Truncate(time.Second),
				reads.Load(), writes.Load(), rebuilds.Load(),
				runtime.NumGoroutine())
		}
	}
}

func dumpHeap(idx int, startTime time.Time) {
	path := filepath.Join(*flagOutDir, fmt.Sprintf("heap-%03d.pb.gz", idx))
	f, err := os.Create(path) //nolint:gosec // path is constructed from -out plus a numeric index
	if err != nil {
		log.Printf("soak: cannot create heap profile: %v", err)
		invalidHeapSnapshots.Add(1)
		return
	}
	runtime.GC()
	if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
		log.Printf("soak: heap profile write: %v", err)
		_ = f.Close()
		invalidHeapSnapshots.Add(1)
		return
	}
	if err := f.Close(); err != nil {
		log.Printf("soak: heap profile close: %v", err)
		invalidHeapSnapshots.Add(1)
		return
	}
	// Integrity check: the closed file must be at least minHeapProfileBytes.
	// Stat AFTER Close so all kernel buffers have been flushed.
	fi, err := os.Stat(path)
	if err != nil {
		log.Printf("soak: INVALID HEAP SNAPSHOT %s: stat failed: %v", path, err)
		invalidHeapSnapshots.Add(1)
		return
	}
	if fi.Size() < minHeapProfileBytes {
		log.Printf("soak: INVALID HEAP SNAPSHOT %s: size=%d bytes (< %d) — truncated profile",
			path, fi.Size(), minHeapProfileBytes)
		invalidHeapSnapshots.Add(1)
		return
	}
	log.Printf("soak: heap snapshot %s @ t=%v size=%d", path, time.Since(startTime).Truncate(time.Second), fi.Size())
}
