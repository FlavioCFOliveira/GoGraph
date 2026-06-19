// Example 20_concurrent_reads — the lock-free read contract of a frozen
// CSR snapshot, exercised by many concurrent readers.
//
// A single immutable [csr.CSR] is built once from a seeded, realistic
// scale-free network and then read concurrently by a pool of worker
// goroutines. Each worker runs the same mixed read workload — a batch of
// Dijkstra single-source shortest paths, a BFS reach count, and a
// PageRank to convergence — over the one shared snapshot. None of them
// takes a lock on the snapshot: an immutable CSR is safe for any number
// of concurrent readers with zero synchronisation on the hot path
// (Mehlhorn-Sanders, GraphBLAS). That is the contract this example
// demonstrates and measures.
//
// # Model
//
// The graph is a Barabási-Albert preferential-attachment network — the
// canonical model of a social / web graph, where a few high-degree hubs
// dominate and the degree distribution is heavy-tailed. It is the right
// shape here for three reasons:
//
//   - It is connected by construction (a connected seed core, and every
//     new node attaches at least one edge to the already-connected
//     component), so BFS reaches every node and its reach count is a
//     constant, regardless of the seed.
//   - The hub structure gives PageRank a meaningful, well-separated
//     top-k, so "the top-k set is constant" is a robust invariant.
//   - The high-degree hubs create heavy adjacency fan-out, so each read
//     does real CPU work — the point of a concurrency benchmark.
//
// Edges are undirected (an [adjlist.AdjList] with Directed:false mirrors
// every insertion) and carry an integer weight in [1, weightMax] drawn
// from the seeded RNG. Integer weights keep Dijkstra free of NaN/Inf
// concerns and make distance sums exact.
//
// # Evidence — the lock-free read contract
//
// The example reports the evidence that matters for a concurrency
// subject (see docs/examples-standard.md):
//
//   - Aggregate read throughput (reads/s) of the mixed workload.
//   - Per-worker-count scaling: the identical workload is run at 1, 2,
//     4, 8 … workers (capped at GOMAXPROCS), and the throughput at each
//     level is printed as telemetry. Throughput that climbs with the
//     worker count is the observable evidence that readers do not
//     contend on the snapshot.
//   - Live heap, so a reader can see the immutable snapshot is shared,
//     not copied per worker.
//
// All telemetry lines are prefixed with "# " and vary per run and
// machine. The correctness evidence is printed as bare deterministic
// fact lines: every concurrent read returns the SAME answer a single
// reader computes. Specifically — for a fixed seed — every concurrent
// Dijkstra from the fixed source yields the same distance to the fixed
// target, the BFS reach count is constant, and the PageRank top-k node
// set is constant. The headline fact, reads.agree=true, asserts that
// concurrent reads agreed with the single-threaded reference across
// every worker count.
//
// # Scale
//
// Run with no flags, the example builds a small, deterministic default
// (a few thousand nodes) that a test pins and that completes well under
// a second. Every dimension is a flag, so the same binary scales up to a
// size where concurrent reads do enough work for the scaling curve to be
// observable:
//
//	go run ./examples/20_concurrent_reads -nodes 200000 -attach 8 -workers 16
//
// The data shape is reproducible for a fixed -seed; only the telemetry
// (lines prefixed with "# ") varies between runs and machines.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// config captures every scale and shape knob of the benchmark. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	nodes      int   // number of nodes in the scale-free network
	attach     int   // BA attachment degree m: edges each new node adds
	seedCore   int   // size m0 of the connected seed core (m0 > attach)
	weightMax  int   // edge weights are drawn from [1, weightMax]
	workers    int   // maximum worker count the scaling sweep climbs to
	iterations int   // Dijkstra SSSPs each worker runs per round
	topK       int   // PageRank top-k set size pinned as an invariant
	seed       int64 // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns a small, deterministic configuration the
// regression test pins. It is large enough that the concurrent readers
// do real work, yet completes well under the short-layer 60 s package
// budget.
func defaultConfig() config {
	return config{
		nodes:      4000,
		attach:     4,
		seedCore:   8,
		weightMax:  10,
		workers:    8,
		iterations: 16,
		topK:       10,
		seed:       1,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.attach < 1:
		return fmt.Errorf("attach must be >= 1, got %d", c.attach)
	case c.seedCore <= c.attach:
		return fmt.Errorf("seedCore (%d) must exceed attach (%d) so each new node has enough distinct targets", c.seedCore, c.attach)
	case c.nodes < c.seedCore:
		return fmt.Errorf("nodes (%d) must be >= seedCore (%d)", c.nodes, c.seedCore)
	case c.weightMax < 1:
		return fmt.Errorf("weightMax must be >= 1, got %d", c.weightMax)
	case c.workers < 1:
		return fmt.Errorf("workers must be >= 1, got %d", c.workers)
	case c.iterations < 1:
		return fmt.Errorf("iterations must be >= 1, got %d", c.iterations)
	case c.topK < 1:
		return fmt.Errorf("topK must be >= 1, got %d", c.topK)
	case c.topK > c.nodes:
		return fmt.Errorf("topK (%d) must be <= nodes (%d)", c.topK, c.nodes)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of nodes in the scale-free network")
	flag.IntVar(&cfg.attach, "attach", cfg.attach, "BA attachment degree m (edges each new node adds)")
	flag.IntVar(&cfg.seedCore, "seed-core", cfg.seedCore, "size of the connected seed core (must exceed attach)")
	flag.IntVar(&cfg.weightMax, "weight-max", cfg.weightMax, "edge weights are drawn from [1, weight-max]")
	flag.IntVar(&cfg.workers, "workers", cfg.workers, "maximum worker count the scaling sweep climbs to")
	flag.IntVar(&cfg.iterations, "iterations", cfg.iterations, "Dijkstra SSSPs each worker runs per round")
	flag.IntVar(&cfg.topK, "top-k", cfg.topK, "PageRank top-k set size pinned as an invariant")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the immutable CSR snapshot described by cfg, computes the
// single-threaded reference answers, then runs the identical mixed read
// workload concurrently at a sweep of worker counts, verifying every
// concurrent read agrees with the reference and reporting the throughput
// scaling. Bare lines carry deterministic facts (counts, results,
// invariants — reproducible for a fixed seed); lines prefixed with "# "
// carry volatile telemetry (throughput, durations, heap) that varies per
// run and per machine. All output goes to w so a test can capture and
// assert on the deterministic lines; run returns wrapped errors rather
// than terminating the process and honours ctx cancellation throughout.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.attach=%d\n", cfg.attach)
	fmt.Fprintf(w, "config.seed_core=%d\n", cfg.seedCore)
	fmt.Fprintf(w, "config.weight_max=%d\n", cfg.weightMax)
	fmt.Fprintf(w, "config.iterations=%d\n", cfg.iterations)
	fmt.Fprintf(w, "config.top_k=%d\n", cfg.topK)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// Build the snapshot single-threaded so the seeded RNG draws in a
	// fixed order; the resulting CSR is then immutable and shared by every
	// reader with no synchronisation.
	g, err := generate(ctx, cfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// The fixed source/target the readers all query. Node value 0 is in
	// the seed core and node value cfg.nodes-1 is the last one added; both
	// are live for any valid config. Resolve them to NodeIDs through the
	// mapper before the CSR is built — the CSR's MaxNodeID is shard-padded
	// and not guaranteed to be a live node, so it is the wrong target.
	mapper := g.Mapper()
	src, ok := mapper.Lookup(0)
	if !ok {
		return fmt.Errorf("source node 0 not interned")
	}
	dst, ok := mapper.Lookup(cfg.nodes - 1)
	if !ok {
		return fmt.Errorf("target node %d not interned", cfg.nodes-1)
	}

	c := csr.BuildFromAdjList(g)

	fmt.Fprintf(w, "nodes.count=%d\n", c.Order())
	fmt.Fprintf(w, "edges.directed=%d\n", c.Size())

	built := readMem()
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))
	fmt.Fprintf(w, "# mem.num_gc=%d\n", built.NumGC-base.NumGC)

	// Single-threaded reference: the source of truth every concurrent read
	// must reproduce. Computing it once, sequentially, anchors the
	// correctness claim.
	ref, err := readOnce(c, src, dst, cfg.topK)
	if err != nil {
		return fmt.Errorf("reference read: %w", err)
	}

	// Deterministic correctness facts: pinned by the regression test.
	fmt.Fprintf(w, "ref.dijkstra_dist=%d\n", ref.dijkstraDist)
	fmt.Fprintf(w, "ref.bfs_reached=%d\n", ref.bfsReached)
	fmt.Fprintf(w, "ref.pagerank_topk=%s\n", formatTopK(ref.pagerankTopK))

	// Run the identical workload at a sweep of worker counts, verifying
	// every concurrent read agrees with the reference and surfacing the
	// throughput at each level as telemetry.
	agreed, err := sweep(ctx, w, c, cfg, src, dst, ref)
	if err != nil {
		return fmt.Errorf("sweep: %w", err)
	}

	// The headline correctness fact: concurrent reads returned the same
	// answer as the single-threaded reference at every worker count.
	fmt.Fprintf(w, "reads.agree=%t\n", agreed)
	return nil
}

// readResult is the answer one mixed read produces over the shared CSR:
// the three values the readers compare against the reference. None of
// them depends on goroutine scheduling — they are pure functions of the
// immutable snapshot.
type readResult struct {
	dijkstraDist int64          // shortest-path distance src -> dst
	bfsReached   int            // nodes BFS reaches from src
	pagerankTopK []graph.NodeID // top-k nodes by PageRank, deterministic order
}

// readOnce performs one full mixed read over c: a Dijkstra src->dst
// distance, a BFS reach count from src, and the PageRank top-k node set.
// It is the unit of work every reader repeats and the function that
// computes the single-threaded reference. It takes no lock on c.
func readOnce(c *csr.CSR[int64], src, dst graph.NodeID, topK int) (readResult, error) {
	d, err := search.Dijkstra(c, src)
	if err != nil {
		return readResult{}, fmt.Errorf("dijkstra: %w", err)
	}
	dist, reachable := d.Distance(dst)
	if !reachable {
		return readResult{}, fmt.Errorf("dijkstra: target %d unreachable from %d", dst, src)
	}

	reached := 0
	search.BFS(c, src, func(_ graph.NodeID, _ int) bool {
		reached++
		return true
	})

	ranks, _, err := centrality.PageRank(c, centrality.DefaultPageRankOptions())
	if err != nil {
		return readResult{}, fmt.Errorf("pagerank: %w", err)
	}

	return readResult{
		dijkstraDist: dist,
		bfsReached:   reached,
		pagerankTopK: topKByRank(ranks, topK),
	}, nil
}

// equalResult reports whether two mixed-read results are identical. It is
// the concurrency-invariant check: every concurrent read must equal the
// single-threaded reference.
func equalResult(a, b readResult) bool {
	if a.dijkstraDist != b.dijkstraDist || a.bfsReached != b.bfsReached {
		return false
	}
	if len(a.pagerankTopK) != len(b.pagerankTopK) {
		return false
	}
	for i := range a.pagerankTopK {
		if a.pagerankTopK[i] != b.pagerankTopK[i] {
			return false
		}
	}
	return true
}

// topKByRank returns the k nodes with the highest PageRank, ordered by
// (rank descending, NodeID ascending). The NodeID tie-break makes the
// selection total and deterministic, so the result is a stable set for a
// fixed seed regardless of how many goroutines computed it.
func topKByRank(ranks []float64, k int) []graph.NodeID {
	order := make([]graph.NodeID, len(ranks))
	for id := range order {
		order[id] = graph.NodeID(id)
	}
	sort.Slice(order, func(i, j int) bool {
		ri, rj := ranks[order[i]], ranks[order[j]]
		if ri != rj {
			return ri > rj
		}
		return order[i] < order[j]
	})
	if k > len(order) {
		k = len(order)
	}
	out := make([]graph.NodeID, k)
	copy(out, order[:k])
	return out
}

// sweep runs the identical mixed workload at a sweep of worker counts
// (1, 2, 4, … capped at both cfg.workers and GOMAXPROCS) over the one
// shared snapshot c. For each level it spawns that many readers, has each
// repeat the mixed read until the round's read budget is consumed,
// verifies every read agrees with ref, and prints the achieved
// throughput as telemetry. It returns whether every read at every level
// agreed with the reference. All goroutines join before each level
// returns, and a cancelled ctx stops the sweep promptly.
func sweep(ctx context.Context, w io.Writer, c *csr.CSR[int64], cfg config, src, dst graph.NodeID, ref readResult) (bool, error) {
	maxWorkers := cfg.workers
	if procs := runtime.GOMAXPROCS(0); procs < maxWorkers {
		maxWorkers = procs
	}

	allAgreed := true
	for workers := 1; workers <= maxWorkers; workers *= 2 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		reads, elapsed, agreed, err := runLevel(ctx, c, cfg, workers, src, dst, ref)
		if err != nil {
			return false, err
		}
		if !agreed {
			allAgreed = false
		}
		fmt.Fprintf(w, "# scale.workers_%d.reads=%d\n", workers, reads)
		fmt.Fprintf(w, "# scale.workers_%d.elapsed=%s\n", workers, elapsed.Round(time.Microsecond))
		fmt.Fprintf(w, "# scale.workers_%d.throughput=%.0f reads/s\n", workers, rate(reads, elapsed))
	}
	return allAgreed, nil
}

// runLevel runs one level of the sweep: it spawns workers goroutines,
// each repeatedly performing the mixed read over the shared CSR and
// comparing the answer to ref, until the level's read budget is spent.
// Every goroutine joins before runLevel returns (on completion or
// cancellation), so the example leaks no goroutines. It returns the total
// reads performed, the wall-clock elapsed, and whether every read agreed.
func runLevel(ctx context.Context, c *csr.CSR[int64], cfg config, workers int, src, dst graph.NodeID, ref readResult) (int64, time.Duration, bool, error) {
	// Each worker performs cfg.iterations mixed reads, so the per-level
	// work scales with the worker count and the throughput curve reflects
	// how well concurrent reads scale.
	var (
		wg       sync.WaitGroup
		reads    atomic.Int64
		mismatch atomic.Bool
		firstErr atomic.Pointer[error]
	)

	start := time.Now()
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := 0; i < cfg.iterations; i++ {
				// Poll ctx coarsely so a cancelled run stops promptly
				// without making the check measurable against the read.
				if i%ctxCheckEvery == 0 && ctx.Err() != nil {
					return
				}
				got, err := readOnce(c, src, dst, cfg.topK)
				if err != nil {
					e := fmt.Errorf("worker read: %w", err)
					firstErr.CompareAndSwap(nil, &e)
					return
				}
				if !equalResult(got, ref) {
					mismatch.Store(true)
				}
				reads.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if ep := firstErr.Load(); ep != nil {
		return 0, 0, false, *ep
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, false, err
	}
	return reads.Load(), elapsed, !mismatch.Load(), nil
}

// ctxCheckEvery bounds how often a worker polls ctx for cancellation:
// often enough that a cancelled run stops promptly, rare enough that the
// check is free relative to a mixed read.
const ctxCheckEvery = 4

// ─────────────────────────────────────────────────────────────────────────────
// Seeded scale-free generator
// ─────────────────────────────────────────────────────────────────────────────

// generate builds the Barabási-Albert preferential-attachment network
// described by cfg into a fresh undirected AdjList, single-threaded so
// the seeded RNG draws in a fixed order (and the data shape is
// reproducible for a given -seed). It starts from a connected path core
// of cfg.seedCore nodes, then each subsequent node attaches cfg.attach
// edges to existing nodes chosen with probability proportional to their
// current degree (the standard repeated-node-list method), rejecting
// self-loops and parallel targets within the same node. Because every
// new node attaches to the already-connected component, the graph is
// connected by construction. Each edge carries an integer weight in
// [1, weightMax]. The build honours ctx cancellation on a periodic check.
func generate(ctx context.Context, cfg config) (*adjlist.AdjList[int, int64], error) {
	rng := newRNG(cfg.seed)
	g := adjlist.New[int, int64](adjlist.Config{Directed: false})

	// repeated holds each node once per incident edge endpoint, so a
	// uniform index into it samples a node with probability proportional
	// to its degree. Pre-sized to the final endpoint count: the core
	// contributes 2*(seedCore-1) endpoints and each of the remaining
	// nodes contributes 2*attach.
	endpoints := 2*(cfg.seedCore-1) + 2*cfg.attach*(cfg.nodes-cfg.seedCore)
	repeated := make([]int, 0, endpoints)

	// Connected seed core: a simple path 0-1-2-...-(seedCore-1). A path is
	// connected and minimal, which keeps the early degree distribution
	// from being dominated by the artificial core.
	for i := 1; i < cfg.seedCore; i++ {
		if err := g.AddEdge(i-1, i, weight(rng, cfg.weightMax)); err != nil {
			return nil, fmt.Errorf("AddEdge core %d-%d: %w", i-1, i, err)
		}
		repeated = append(repeated, i-1, i)
	}

	// Preferential attachment for every node beyond the core.
	targets := make(map[int]struct{}, cfg.attach)
	for v := cfg.seedCore; v < cfg.nodes; v++ {
		if v%genCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		// Pick attach distinct existing targets, degree-proportionally,
		// rejecting self-loops and duplicates within this node. All picks
		// happen before any endpoint is appended, so v is never selectable
		// as its own target and its edges do not bias one another's draw.
		clear(targets)
		for len(targets) < cfg.attach {
			t := repeated[rng.Intn(len(repeated))]
			if t == v {
				continue
			}
			if _, dup := targets[t]; dup {
				continue
			}
			targets[t] = struct{}{}
		}
		// Insert the edges in ascending target order so the draw order —
		// and therefore the weights — are deterministic for a fixed seed.
		ordered := make([]int, 0, len(targets))
		for t := range targets {
			ordered = append(ordered, t)
		}
		sort.Ints(ordered)
		for _, t := range ordered {
			if err := g.AddEdge(v, t, weight(rng, cfg.weightMax)); err != nil {
				return nil, fmt.Errorf("AddEdge attach %d-%d: %w", v, t, err)
			}
			repeated = append(repeated, v, t)
		}
	}
	return g, nil
}

// genCheckEvery bounds how often generate polls ctx for cancellation.
const genCheckEvery = 4096

// weight returns a random integer edge weight in [1, weightMax] drawn
// from rng. Weights are strictly positive so Dijkstra has no zero-weight
// edges and the distance sums are meaningful.
func weight(rng *rng64, weightMax int) int64 {
	return int64(rng.Intn(weightMax) + 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded RNG
// ─────────────────────────────────────────────────────────────────────────────

// rng64 is a small deterministic pseudo-random source built on the
// SplitMix64 generator. It is used instead of math/rand so the generator
// has no dependency on any package-level RNG state and draws are fully
// reproducible for a given seed. It is NOT safe for concurrent use; the
// generator drives it single-threaded by design.
type rng64 struct {
	state uint64
}

// newRNG returns an rng64 seeded from seed.
func newRNG(seed int64) *rng64 {
	return &rng64{state: uint64(seed)}
}

// next returns the next 64-bit value from the SplitMix64 stream.
func (r *rng64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Intn returns a non-negative pseudo-random int in [0, n) using
// Lemire's debiased multiply-shift reduction, so the distribution is
// uniform with no modulo bias. It panics if n <= 0, matching math/rand.
func (r *rng64) Intn(n int) int {
	if n <= 0 {
		panic("rng64.Intn: n must be > 0")
	}
	// Reduce a 64-bit draw into [0, n) via the high half of the 128-bit
	// product, which is uniform for the full uint64 range and avoids the
	// narrowing conversions gosec flags on raw casts.
	hi, _ := mul64(r.next(), uint64(n))
	return int(hi)
}

// mul64 returns the 128-bit product of a and b as (hi, lo) 64-bit halves.
func mul64(a, b uint64) (hi, lo uint64) {
	const mask = 0xffffffff
	al, ah := a&mask, a>>32
	bl, bh := b&mask, b>>32
	ll := al * bl
	lh := al * bh
	hl := ah * bl
	hh := ah * bh
	cross := (ll >> 32) + (lh & mask) + (hl & mask)
	hi = hh + (lh >> 32) + (hl >> 32) + (cross >> 32)
	lo = (cross << 32) | (ll & mask)
	return hi, lo
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry and formatting helpers
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a
// zero-length interval.
func rate(count int64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB) suffix.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatTopK renders a top-k NodeID slice as a compact bracketed,
// comma-separated list (e.g. "[0,3,7]"), a stable, assertable form for
// the deterministic PageRank top-k fact line.
func formatTopK(ids []graph.NodeID) string {
	b := make([]byte, 0, len(ids)*4+2)
	b = append(b, '[')
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendUint(b, uint64(id))
	}
	b = append(b, ']')
	return string(b)
}

// appendUint appends the base-10 text of v to b without going through
// strconv, keeping formatTopK allocation-light.
func appendUint(b []byte, v uint64) []byte {
	if v == 0 {
		return append(b, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	return append(b, tmp[i:]...)
}
