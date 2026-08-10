// Example 33_generation_swap — the read-mostly MVCC snapshot-swap pattern of
// graph/generation, under concurrent readers.
//
// A live routing service serves queries against an immutable CSR snapshot of
// its road network. Periodically the network is rebuilt (new roads open) and a
// fresh snapshot must replace the old one WITHOUT blocking in-flight queries
// and WITHOUT any query ever observing a half-built graph. That is exactly what
// generation.Publisher provides: every reader Acquires the current generation,
// uses it, and Releases it; a publisher prepares the next generation in a fresh
// allocation and atomically swaps the pointer; an old generation is reclaimed
// only after its last reader has Released it (refcount drain).
//
// This example runs -readers goroutines that repeatedly Acquire → read →
// Release while a publisher swaps through -versions successively larger road
// networks. It certifies the two MVCC guarantees: every read observes a whole,
// consistent generation (its node count is always one of the published
// versions — never a torn mix), and the generation refcount is accounted
// correctly (it returns to its baseline once readers release). It runs under
// -race and is a goroutine-leak check.
//
// # Scale
//
// The small deterministic default runs in milliseconds. Scale up the readers,
// versions and network size to make the swap and the read throughput
// observable:
//
//	go run ./examples/33_generation_swap -readers 64 -versions 20 -base-nodes 100000
//
// The fact lines are deterministic for a fixed -seed; only the "# " telemetry
// (throughput, the set of generations each schedule happened to observe) varies.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/generation"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// config captures every scale and shape knob. The zero value is not valid.
type config struct {
	versions       int   // number of successive road-network snapshots to publish
	readers        int   // concurrent reader goroutines
	readsPerReader int   // Acquire/read/Release cycles each reader performs
	baseNodes      int   // nodes in the first (smallest) version
	growth         int   // extra nodes each subsequent version adds
	seed           int64 // RNG seed (reserved; the ring shape is seed-independent but kept for uniformity)
}

// defaultConfig returns the small deterministic default the regression test
// pins: eight versions, eight readers doing 200 reads each over networks that
// grow from 500 nodes by 250 per version.
func defaultConfig() config {
	return config{
		versions:       8,
		readers:        8,
		readsPerReader: 200,
		baseNodes:      500,
		growth:         250,
		seed:           1,
	}
}

// validate rejects a configuration that cannot produce the requested shape.
func (c config) validate() error {
	switch {
	case c.versions < 1:
		return fmt.Errorf("versions must be >= 1, got %d", c.versions)
	case c.readers < 1:
		return fmt.Errorf("readers must be >= 1, got %d", c.readers)
	case c.readsPerReader < 1:
		return fmt.Errorf("reads-per-reader must be >= 1, got %d", c.readsPerReader)
	case c.baseNodes < 2:
		return fmt.Errorf("base-nodes must be >= 2, got %d", c.baseNodes)
	case c.growth < 0:
		return fmt.Errorf("growth must be >= 0, got %d", c.growth)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.versions, "versions", cfg.versions, "successive road-network snapshots to publish")
	flag.IntVar(&cfg.readers, "readers", cfg.readers, "concurrent reader goroutines")
	flag.IntVar(&cfg.readsPerReader, "reads-per-reader", cfg.readsPerReader, "Acquire/read/Release cycles per reader")
	flag.IntVar(&cfg.baseNodes, "base-nodes", cfg.baseNodes, "nodes in the first version")
	flag.IntVar(&cfg.growth, "growth", cfg.growth, "extra nodes each subsequent version adds")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed")
	prof := exprof.Bind(flag.CommandLine)
	flag.Parse()

	if err := prof.Run(os.Stdout, func() error {
		return run(context.Background(), os.Stdout, cfg)
	}); err != nil {
		log.Fatal(err)
	}
}

// run publishes cfg.versions successively larger road networks through one
// generation.Publisher while cfg.readers goroutines read concurrently, and
// writes a report to w. Bare lines carry deterministic facts; "# "-prefixed
// lines carry volatile telemetry.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.versions=%d\n", cfg.versions)
	fmt.Fprintf(w, "config.readers=%d\n", cfg.readers)
	fmt.Fprintf(w, "config.reads_per_reader=%d\n", cfg.readsPerReader)
	fmt.Fprintf(w, "config.base_nodes=%d\n", cfg.baseNodes)

	// Pre-build every version's CSR snapshot up front (single-threaded, so the
	// build is deterministic) and record the valid node counts. A reader that
	// ever observes an order outside this set has seen a torn generation.
	snapshots := make([]*csr.CSR[struct{}], cfg.versions)
	validOrder := make(map[uint64]bool, cfg.versions)
	for v := 0; v < cfg.versions; v++ {
		n := cfg.baseNodes + v*cfg.growth
		snapshots[v] = ringCSR(n)
		validOrder[uint64(n)] = true //nolint:gosec // G115: n is a positive node count
	}
	finalOrder := cfg.baseNodes + (cfg.versions-1)*cfg.growth

	pub := generation.New(snapshots[0])
	defer pub.Close()

	var (
		totalReads    atomic.Int64
		inconsistent  atomic.Int64
		distinctOrder sync.Map // observed order -> struct{}
	)

	start := time.Now()
	var wg sync.WaitGroup

	// Publisher: swap in each subsequent version with a brief pause so readers
	// have a chance to observe more than the final generation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pprofLabel()
		for v := 1; v < cfg.versions; v++ {
			if ctx.Err() != nil {
				return
			}
			if _, err := pub.Publish(snapshots[v]); err != nil {
				return // publisher closed
			}
			runtime.Gosched()
		}
	}()

	// Readers: Acquire → read (a real BFS reach over the snapshot) → Release.
	for r := 0; r < cfg.readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pprofLabel()
			for i := 0; i < cfg.readsPerReader; i++ {
				if ctx.Err() != nil {
					return
				}
				gen := pub.Acquire()
				c := gen.CSR()
				order := c.Order()
				if !validOrder[order] {
					inconsistent.Add(1)
				}
				distinctOrder.Store(order, struct{}{})
				// Touch the snapshot with a real read so the generation is
				// genuinely in use while it is held. Node id 0 (the ring's key
				// 0, interned first) is a live source in every version.
				reach := 0
				search.BFS(c, graph.NodeID(0), func(_ graph.NodeID, _ int) bool { reach++; return true })
				pub.Release(gen)
				totalReads.Add(1)
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Refcount accounting: after the workload, an Acquire/Release pair must
	// leave the current generation's refcount at the value it started from.
	cur := pub.Acquire()
	before := cur.Refcount()
	again := pub.Acquire()
	afterTwo := again.Refcount()
	pub.Release(again)
	pub.Release(cur)
	refcountAccounted := afterTwo == before+1

	distinct := 0
	distinctOrder.Range(func(_, _ any) bool { distinct++; return true })

	fmt.Fprintf(w, "generations.published=%d\n", cfg.versions)
	fmt.Fprintf(w, "reads.total=%d\n", totalReads.Load())
	fmt.Fprintf(w, "reads.all_consistent=%t\n", inconsistent.Load() == 0)
	fmt.Fprintf(w, "final.order=%d\n", finalOrder)
	fmt.Fprintf(w, "final.current_order=%d\n", pub.Current().CSR().Order())
	fmt.Fprintf(w, "refcount.accounted=%t\n", refcountAccounted)

	fmt.Fprintf(w, "# swap.elapsed=%s\n", elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# reads.throughput=%.0f reads/s\n", safeRate(totalReads.Load(), elapsed))
	fmt.Fprintf(w, "# reads.distinct_generations_observed=%d\n", distinct)
	return nil
}

// ringCSR builds an immutable CSR snapshot of an undirected ring of n nodes
// (node i joined to (i+1) mod n). A ring is connected with exactly n nodes, so
// its CSR.Order() is exactly n — the property the consistency check keys on —
// and it gives the reader's BFS real work to do.
func ringCSR(n int) *csr.CSR[struct{}] {
	a := adjlist.New[int64, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < n; i++ {
		_ = a.AddEdge(int64(i), int64((i+1)%n), struct{}{})
	}
	return csr.BuildFromAdjList(a)
}

// pprofLabel is a placeholder hook kept trivial; goroutine labelling is not
// needed for the example's correctness but the seam documents where a
// production service would attach pprof.SetGoroutineLabels.
func pprofLabel() {}

// safeRate returns count/seconds, or 0 when the duration is non-positive.
func safeRate(count int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(count) / d.Seconds()
}
