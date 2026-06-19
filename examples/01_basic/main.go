// Example 01_basic — build a weighted directed transport network, freeze
// it to an immutable CSR snapshot, and run a single-source Dijkstra
// shortest-paths query with route reconstruction.
//
// This is the minimal end-to-end GoGraph routing flow, scaled to a size
// where the search work is observable: a seeded generator lays down a
// realistic road-style network, the mutable [adjlist.AdjList] builder
// ingests it, [csr.BuildFromAdjList] freezes it into an immutable
// snapshot, and [search.Dijkstra] computes shortest paths from a single
// source over the whole graph. The parent chain of the result is resolved
// back to node coordinates through the [graph.Mapper] to print a concrete
// multi-hop route.
//
// # Model
//
// The network is a directed random geometric graph (RGG). N junctions are
// placed at seeded integer coordinates in a [0, span) x [0, span) square.
// Two junctions within Euclidean radius are joined by a road in BOTH
// directions; the road's weight is the integer-rounded straight-line
// distance between them (always >= 1). The radius is tuned above the RGG
// connectivity threshold (r ~ span*sqrt(ln N / (pi*N))) so the giant
// component spans the whole graph, and an id-ordered backbone (junction
// i <-> i+1, carrying its true geometric weight) is laid down as a
// synthetic connectivity guarantee so that EVERY junction is reachable
// from the source for any seed and scale. The backbone roads are long and
// the local roads are short, so Dijkstra almost always routes through the
// short local roads — the backbone only guarantees reachability, it does
// not dominate the shortest paths.
//
// Because every road's weight is at most the radius, a route between two
// distant junctions must traverse many short hops: the shortest paths are
// genuinely multi-hop, which is what makes this a meaningful Dijkstra
// exercise rather than a one-edge lookup.
//
// # Determinism
//
// The data shape is reproducible for a fixed -seed: junction placement is
// drawn from a seeded math/rand, the adjacency is emitted in a fixed order
// (ascending source, then ascending destination), and edge weights are
// computed with integer-only arithmetic — ceil(sqrt(dx*dx + dy*dy)) — so
// the weights, the reachable-junction count, and the shortest distances to
// fixed target junctions are bit-identical across machines, OS and
// architecture. (math.Hypot/math.Sqrt are deliberately avoided in the
// weight path: their last-bit result can differ between amd64 and arm64
// because of FMA fusion, which could flip an integer weight and so a
// pinned fact.) Lines prefixed with "# " carry volatile telemetry
// (durations, throughput, live heap) that varies per run and per machine.
//
// # Scale
//
// Run with no flags, the example builds a small deterministic default
// (a few thousand junctions) that the regression test pins and that stays
// well under the short-test budget. Every dimension is a flag, so the same
// binary scales up to where the search cost becomes interesting:
//
//	go run ./examples/01_basic                       # small deterministic default
//	go run ./examples/01_basic -nodes 1000000 -seed 7 # observable-scale run
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/bits"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// config captures every scale and shape knob of the example. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	nodes  int     // number of junctions to place
	span   int     // side length of the square the junctions are placed in
	radius float64 // connection radius as a multiple of the auto threshold
	seed   int64   // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns a small, deterministic network: a few thousand
// junctions in a 4000x4000 square at the auto-tuned connection radius.
// This is the shape the regression test pins; it builds and queries well
// under the short-layer 60 s package budget.
func defaultConfig() config {
	return config{
		nodes:  5000,
		span:   4000,
		radius: 1.0,
		seed:   1,
	}
}

// maxSpan bounds the coordinate square so that a coordinate delta squared
// and summed cannot overflow int64 in the weight computation: with
// span <= 2^30, |dx|, |dy| < 2^30, so dx*dx + dy*dy < 2^61.
const maxSpan = 1 << 30

// validate rejects a configuration that cannot produce a sensible network.
// It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.nodes <= 1:
		return fmt.Errorf("nodes must be > 1, got %d", c.nodes)
	case c.span <= 0:
		return fmt.Errorf("span must be > 0, got %d", c.span)
	case c.span > maxSpan:
		return fmt.Errorf("span must be <= %d (overflow guard), got %d", maxSpan, c.span)
	case c.radius <= 0:
		return fmt.Errorf("radius multiplier must be > 0, got %g", c.radius)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of junctions to generate")
	flag.IntVar(&cfg.span, "span", cfg.span, "side length of the coordinate square")
	flag.Float64Var(&cfg.radius, "radius", cfg.radius,
		"connection radius as a multiple of the auto-tuned connectivity threshold")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the transport network described by cfg, freezes it to a
// CSR snapshot, runs single-source Dijkstra from junction 0, and writes a
// report to w. Bare lines carry deterministic facts (counts, distances,
// the reconstructed route — reproducible for a fixed seed); lines prefixed
// with "# " carry volatile telemetry (durations, throughput, heap) that
// vary per run and per machine. All output goes to w so a test can capture
// and assert on the deterministic lines. run honours ctx cancellation on a
// coarse interval during generation.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	r := autoRadius(cfg) * cfg.radius

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.span=%d\n", cfg.span)
	fmt.Fprintf(w, "config.radius=%d\n", int64(r))
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	gen, err := build(ctx, a, cfg, r)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	fmt.Fprintf(w, "nodes.junctions=%d\n", gen.nodes)
	fmt.Fprintf(w, "edges.roads=%d\n", gen.edges)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", gen.elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "# build.node_rate=%.0f nodes/s\n", rate(gen.nodes, gen.elapsed))
	fmt.Fprintf(w, "# build.edge_rate=%.0f edges/s\n", rate(gen.edges, gen.elapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))

	if err := ctx.Err(); err != nil {
		return err
	}

	// Freeze the mutable builder into an immutable CSR snapshot — the
	// lock-free query surface Dijkstra runs against.
	freezeStart := time.Now()
	c := csr.BuildFromAdjList(a)
	freezeElapsed := time.Since(freezeStart)
	fmt.Fprintf(w, "# freeze.elapsed=%s\n", freezeElapsed.Round(time.Microsecond))

	mapper := a.Mapper()
	src, ok := mapper.Lookup(sourceJunction)
	if !ok {
		return fmt.Errorf("source junction %d not found in graph", sourceJunction)
	}

	// Single-source shortest paths from junction 0 over the whole graph.
	queryStart := time.Now()
	d, err := search.Dijkstra(c, src)
	if err != nil {
		return fmt.Errorf("dijkstra: %w", err)
	}
	queryElapsed := time.Since(queryStart)

	// Reachability is a deterministic fact: count the junctions Dijkstra
	// settled. The id-ordered backbone guarantees this equals the total
	// junction count for any seed and scale. The Mapper assigns NodeIDs
	// per shard, so they are not a dense 0..Order() range — walk the real
	// interned ids rather than assuming density.
	reachable := 0
	mapper.Walk(func(id graph.NodeID, _ int) bool {
		if _, ok := d.Distance(id); ok {
			reachable++
		}
		return true
	})
	fmt.Fprintf(w, "query.reachable=%d\n", reachable)
	fmt.Fprintf(w, "# query.dijkstra.elapsed=%s\n", queryElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# query.dijkstra.node_rate=%.0f nodes/s\n", rate(reachable, queryElapsed))

	// Deterministic shortest distances to a handful of FIXED target
	// junctions, spread across the id range. The distance value is
	// tie-independent, so it is reproducible across machines.
	for _, t := range targetJunctions(cfg.nodes) {
		id, ok := mapper.Lookup(t)
		if !ok {
			return fmt.Errorf("target junction %d not found in graph", t)
		}
		dist, reach := d.Distance(id)
		if !reach {
			fmt.Fprintf(w, "dist.to_%d=unreachable\n", t)
			continue
		}
		fmt.Fprintf(w, "dist.to_%d=%d\n", t, dist)
	}

	// One reconstructed multi-hop route, resolved from the parent chain
	// back to junction ids through the mapper. The route is a deterministic
	// fact for a fixed seed; the test asserts its invariants (it starts at
	// the source, ends at the target, every hop is a real road, and the
	// hop weights sum to the reported distance).
	targets := targetJunctions(cfg.nodes)
	routeTarget := targets[len(targets)-1]
	tid, ok := mapper.Lookup(routeTarget)
	if !ok {
		return fmt.Errorf("route target junction %d not found in graph", routeTarget)
	}
	path := d.Path(tid)
	route, err := routeIDs(mapper, path)
	if err != nil {
		return fmt.Errorf("resolve route to %d: %w", routeTarget, err)
	}
	hops := 0
	if len(path) > 0 {
		hops = len(path) - 1
	}
	fmt.Fprintf(w, "route.to_%d.hops=%d\n", routeTarget, hops)
	fmt.Fprintf(w, "route.to_%d=%s\n", routeTarget, route)

	return nil
}

// sourceJunction is the fixed single-source junction every query is
// anchored at. Junction ids are dense integers in [0, nodes).
const sourceJunction = 0

// targetJunctions returns the fixed target junction ids whose shortest
// distance the example reports, spread across the id range so the routes
// are non-trivial. The last entry is the one whose full route is printed.
func targetJunctions(nodes int) []int {
	return []int{nodes / 4, nodes / 2, nodes - 1}
}

// checkEvery bounds how often the build polls ctx for cancellation: often
// enough that a cancelled large build stops promptly, rare enough that the
// check is free relative to the surrounding work.
const checkEvery = 4096

// point is a junction's integer position in the coordinate square.
type point struct {
	x, y int64
}

// genStats reports the realised shape of a build (the edge count depends
// on the random placement and the radius) plus the wall-clock cost.
type genStats struct {
	nodes   int
	edges   int
	elapsed time.Duration
}

// autoRadius returns the connection radius that puts the random geometric
// graph just above its connectivity threshold: in a span x span square
// with N points, the giant component spans the graph w.h.p. once
// N*pi*r^2/span^2 ~ ln N, i.e. r ~ span*sqrt(ln N / (pi*N)). A small
// constant factor keeps the mean degree modest (road-like) while staying
// safely connected.
func autoRadius(cfg config) float64 {
	const safety = 1.5
	n := float64(cfg.nodes)
	threshold := float64(cfg.span) * sqrtFloat(lnApprox(n)/(3.14159265358979*n))
	if threshold < 1 {
		threshold = 1
	}
	return threshold * safety
}

// build materialises the network described by cfg into a. It places every
// junction at a seeded integer coordinate, buckets the junctions into a
// spatial grid, then for each junction emits directed roads to every other
// junction within radius r (in both directions) plus an id-ordered
// backbone road to its successor. Roads are emitted in a fixed order
// (ascending source, then ascending destination) so the adjacency — and
// thus the frozen CSR — is a deterministic function of the seed alone. The
// build honours ctx cancellation on a periodic check.
func build(ctx context.Context, a *adjlist.AdjList[int, int64], cfg config, r float64) (genStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	// Place junctions. Coordinates are drawn from the seeded RNG so the
	// whole dataset is reproducible.
	pts := make([]point, cfg.nodes)
	for i := range pts {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return genStats{}, err
			}
		}
		pts[i] = point{x: int64(rng.Intn(cfg.span)), y: int64(rng.Intn(cfg.span))}
		if err := a.AddNode(i); err != nil {
			return genStats{}, fmt.Errorf("AddNode %d: %w", i, err)
		}
	}

	// Bucket junctions into a spatial grid whose cell side is the radius,
	// so a junction's within-radius neighbours can only lie in its own cell
	// or the eight adjacent cells (the 3x3 stencil). This turns the
	// near-neighbour search from O(N^2) into expected O(N) for uniformly
	// placed points.
	cell := int64(r)
	if cell < 1 {
		cell = 1
	}
	cols := int64(cfg.span)/cell + 1
	grid := make(map[int64][]int, cfg.nodes)
	cellOf := func(p point) int64 { return (p.y/cell)*cols + p.x/cell }
	for i, p := range pts {
		k := cellOf(p)
		grid[k] = append(grid[k], i)
	}

	r2 := int64(r) * int64(r)
	edges := 0

	// Emit roads in ascending source order so the adjacency is a
	// deterministic function of the seed (Go map iteration order is
	// randomised, so we never iterate the grid map to drive output —
	// we iterate junctions by id and look candidates up in the grid).
	for i := 0; i < cfg.nodes; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return genStats{}, err
			}
		}
		pi := pts[i]
		ci := pi.x / cell
		cj := pi.y / cell

		// Gather within-radius neighbours from the 3x3 cell stencil, then
		// sort by id so each junction's road list is emitted in ascending
		// destination order regardless of grid bucket layout.
		var nbrs []int
		for dj := int64(-1); dj <= 1; dj++ {
			for di := int64(-1); di <= 1; di++ {
				k := (cj+dj)*cols + (ci + di)
				for _, j := range grid[k] {
					if j == i {
						continue
					}
					dx := pi.x - pts[j].x
					dy := pi.y - pts[j].y
					if dx*dx+dy*dy <= r2 {
						nbrs = append(nbrs, j)
					}
				}
			}
		}
		sort.Ints(nbrs)
		for _, j := range nbrs {
			if err := a.AddEdge(i, j, euclidWeight(pi, pts[j])); err != nil {
				return genStats{}, fmt.Errorf("AddEdge %d->%d: %w", i, j, err)
			}
			edges++
		}

		// Backbone: a road to the next junction by id (and back), carrying
		// its true geometric weight. This is a synthetic connectivity
		// guarantee — it makes every junction reachable from the source for
		// any seed and scale, without distorting the shortest paths (the
		// backbone roads are long, so Dijkstra routes around them).
		if i+1 < cfg.nodes {
			wgt := euclidWeight(pi, pts[i+1])
			if err := a.AddEdge(i, i+1, wgt); err != nil {
				return genStats{}, fmt.Errorf("AddEdge backbone %d->%d: %w", i, i+1, err)
			}
			if err := a.AddEdge(i+1, i, wgt); err != nil {
				return genStats{}, fmt.Errorf("AddEdge backbone %d->%d: %w", i+1, i, err)
			}
			edges += 2
		}
	}

	return genStats{nodes: cfg.nodes, edges: edges, elapsed: time.Since(start)}, nil
}

// euclidWeight returns ceil(sqrt(dx*dx + dy*dy)) as a strictly positive
// int64 road weight, using integer-only arithmetic so the result is
// bit-identical on every GOOS/GOARCH (no float rounding mode, no FMA
// fusion, no platform variance — the property the pinned facts depend on).
// The span overflow guard in validate keeps dx*dx + dy*dy within int64.
func euclidWeight(a, b point) int64 {
	dx := a.x - b.x
	dy := a.y - b.y
	w := isqrtCeil(dx*dx + dy*dy)
	if w < 1 {
		w = 1 // never emit a zero-weight road (endpoints are distinct ids)
	}
	return w
}

// isqrtCeil returns ceil(sqrt(n)) for n >= 0 using integer Newton's method,
// deterministically on every platform. The initial guess is an
// over-estimate so the iteration converges monotonically down to
// floor(sqrt(n)); a final correction rounds up when n is not a perfect
// square.
func isqrtCeil(n int64) int64 {
	if n < 2 {
		return n
	}
	x := int64(1) << ((bits.Len64(uint64(n)) + 1) / 2)
	for {
		y := (x + n/x) >> 1
		if y >= x {
			break
		}
		x = y
	}
	// x == floor(sqrt(n)); round up unless n is a perfect square.
	if x*x < n {
		x++
	}
	return x
}

// routeIDs resolves a path of NodeIDs back to a human-readable
// "0 -> 12 -> 45" string of junction ids through the mapper. It returns an
// error if any id cannot be resolved, which would indicate a corrupted
// result.
func routeIDs(mapper *graph.Mapper[int], path []graph.NodeID) (string, error) {
	parts := make([]string, len(path))
	for i, id := range path {
		j, ok := mapper.Resolve(id)
		if !ok {
			return "", fmt.Errorf("unresolved node id %d", id)
		}
		parts[i] = fmt.Sprintf("%d", j)
	}
	return strings.Join(parts, " -> "), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
func rate(count int, elapsed time.Duration) float64 {
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

// ─────────────────────────────────────────────────────────────────────────────
// Integer-friendly math helpers (kept float-free in the weight path; these
// shape only the connection radius, which is reported as a rounded fact).
// ─────────────────────────────────────────────────────────────────────────────

// sqrtFloat returns a float64 square root via Newton's method. It is used
// only to size the connection radius (reported as an int64 fact), never in
// the per-road weight path, so its last-bit behaviour does not affect the
// pinned distances.
func sqrtFloat(x float64) float64 {
	if x <= 0 {
		return 0
	}
	g := x
	for i := 0; i < 60; i++ {
		g = 0.5 * (g + x/g)
	}
	return g
}

// lnApprox returns an approximation of the natural logarithm, used only to
// size the connection radius. Like sqrtFloat it never touches the weight
// path, so it cannot perturb a pinned distance.
func lnApprox(x float64) float64 {
	if x <= 1 {
		return 0
	}
	// ln(x) = 2*atanh((x-1)/(x+1)), expanded as a series. Reduce x into a
	// small range by counting factors of e first for fast convergence.
	const e = 2.718281828459045
	k := 0.0
	for x > e {
		x /= e
		k++
	}
	t := (x - 1) / (x + 1)
	t2 := t * t
	sum := 0.0
	term := t
	for i := 1; i < 40; i += 2 {
		sum += term / float64(i)
		term *= t2
	}
	return k + 2*sum
}
