// Example 29_all_pairs — compute all-pairs shortest paths (APSP) over one
// shared, immutable CSR snapshot with all three APSP algorithms the module
// ships — search.DijkstraAPSP, search.FloydWarshall, and search.JohnsonAPSP —
// cross-check that the three distance matrices are bit-identical, and derive
// the classical graph metrics (radius, diameter, per-node eccentricity) from
// the result.
//
// # Model — a regional road network
//
// The graph models a regional road network. cfg.nodes towns are scattered by
// a seeded RNG across an elongated rectangular region (width x height). Two
// families of road segment connect them:
//
//   - A Euclidean minimum spanning tree over the complete point set. The MST
//     touches every town, so it alone guarantees the graph is CONNECTED for
//     any seed — every pair of towns therefore has a finite shortest-path
//     distance and radius / diameter / eccentricity are well defined.
//   - The cfg.knn nearest neighbours of every town, added as extra segments.
//     Real road networks are not trees; the k-nearest-neighbour edges add the
//     redundant short links that give the network realistic detours and a
//     smooth eccentricity gradient rather than a tree's brittle one.
//
// Every segment is UNDIRECTED (a road runs both ways) and carries a strictly
// positive int64 weight — the Euclidean length rounded to the nearest unit,
// floored at 1. The elongated region (width >> height by default) stretches
// the network along one axis so eccentricity varies smoothly from a distinct
// central cluster of towns (low eccentricity — the radius) out to the towns at
// the two far ends (high eccentricity — the diameter). This topology was
// chosen on the advice of the graph-theory-expert sub-agent: a geometric
// proximity graph yields a high-cardinality eccentricity gradient with a
// genuine centre and periphery, where a hub-and-spoke network would collapse
// the diameter toward the degenerate ratio D = 2r.
//
// # The three-way cross-check
//
// The three algorithms reach the same all-pairs distances by completely
// different routes: Floyd-Warshall runs the textbook O(V^3) dynamic program
// over a dense V x V matrix; DijkstraAPSP runs one Dijkstra per source in
// O(V * (V + E) * log V); Johnson reweights once with Bellman-Ford and then
// also runs one Dijkstra per source. Because every edge weight is a positive
// integer the shortest-path distances are exact and unique, so on this graph
// the three distance matrices must be BIT-IDENTICAL. The example asserts that
// three-way agreement (apsp_three_way_agree=1) — a strong correctness oracle:
// any divergence would be a module bug, and the example would surface the
// first differing pair. It also checks each parallel variant
// (FloydWarshallParallel, JohnsonAPSPParallel) against its serial counterpart,
// which the module guarantees to be bit-identical by construction.
//
// # Evidence
//
// The example reports two kinds of line:
//
//   - Deterministic facts (bare key=value lines, pinned by the test): the node
//     and edge counts; the radius and diameter of the network; the centre town
//     (an eccentricity minimiser) and a peripheral town (an eccentricity
//     maximiser); the eccentricity of a fixed sample town; the count of
//     distinct eccentricity values (evidence that the gradient is not
//     degenerate); and the three-way / parallel agreement flags. All are
//     reproducible for a fixed (-nodes, -knn, -width, -height, -seed).
//   - Volatile telemetry (lines prefixed with "# "): the O(V^2) dense
//     distance-matrix footprint in bytes, and the per-algorithm wall-clock —
//     resource-efficiency evidence showing Floyd-Warshall's dense O(V^3) fill
//     against Johnson's and Dijkstra's sparse O(V * (V + E) * log V) passes —
//     plus the live Go heap. These vary per run and per machine and are never
//     pinned by the regression test.
//
// # Scale
//
// Run with no flags, the example builds a 200-town network (4 nearest
// neighbours each) in a 360x120 region. Floyd-Warshall's O(V^3) is then ~8e6
// cell-updates — microseconds — well under the 60 s short-test budget, yet the
// eccentricity gradient is already rich. Every dimension is a flag, so the same
// binary scales up to where the O(V^3) vs O(V*(V+E)*logV) gap is stark:
//
//	go run ./examples/29_all_pairs -nodes 800 -knn 5 -width 1440 -height 480 -seed 7
//
// At V=800 Floyd-Warshall does ~5e8 cell-updates against Johnson's ~800 sparse
// Dijkstra runs; the telemetry shows the widening wall-clock gap while every
// deterministic fact stays fixed for a given (-nodes, -knn, -width, -height,
// -seed).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"runtime"
	"slices"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// sampleTown is the fixed town whose eccentricity the example reports as a
// deterministic fact. Town 0 always exists (nodes >= 1 is enforced) and is
// interned first, so the sample is well defined at every scale and seed.
const sampleTown = 0

// config captures every scale and shape knob of the example. The zero value is
// not valid; build one with defaultConfig and override fields from flags (see
// main) or construct one directly (see the regression test).
type config struct {
	nodes  int   // number of towns (the APSP dimension V)
	knn    int   // nearest-neighbour road segments added per town
	width  int   // width of the region (the long axis)
	height int   // height of the region (the short axis)
	seed   int64 // RNG seed; fixes the town placement and therefore every fact
}

// defaultConfig returns the small, deterministic default the regression test
// pins: 200 towns with 4 nearest-neighbour segments each, scattered across a
// 360x120 (3:1) region. Floyd-Warshall's O(V^3) is ~8e6 cell-updates, so all
// three algorithms finish in microseconds — well under the short-layer 60 s
// package budget — while the elongated region keeps the eccentricity gradient
// rich.
func defaultConfig() config {
	return config{
		nodes:  200,
		knn:    4,
		width:  360,
		height: 120,
		seed:   1,
	}
}

// validate rejects a configuration that cannot produce a well-defined run. It
// is checked once, at the boundary, before any work. The k-nearest-neighbour
// step needs at least knn other towns to draw from, and a positive region is
// required for the geometric placement.
func (c config) validate() error {
	switch {
	case c.nodes < 3:
		return fmt.Errorf("nodes must be >= 3, got %d", c.nodes)
	case c.knn < 1:
		return fmt.Errorf("knn must be >= 1, got %d", c.knn)
	case c.knn >= c.nodes:
		return fmt.Errorf("knn (%d) must be < nodes (%d)", c.knn, c.nodes)
	case c.width <= 0:
		return fmt.Errorf("width must be > 0, got %d", c.width)
	case c.height <= 0:
		return fmt.Errorf("height must be > 0, got %d", c.height)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of towns (the APSP dimension V)")
	flag.IntVar(&cfg.knn, "knn", cfg.knn, "nearest-neighbour road segments added per town")
	flag.IntVar(&cfg.width, "width", cfg.width, "width of the region (the long axis)")
	flag.IntVar(&cfg.height, "height", cfg.height, "height of the region (the short axis)")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the town placement and every fact)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the regional road network described by cfg, freezes it into a CSR
// snapshot, computes APSP with all three algorithms, cross-checks that the
// three distance matrices agree bit-for-bit, derives the graph metrics, and
// writes a report to w. Bare lines carry deterministic facts (counts, radius,
// diameter, eccentricities, agreement flags — reproducible for a fixed
// configuration); lines prefixed with "# " carry volatile telemetry (matrix
// footprint, per-algorithm wall-clock, heap) that varies per run and machine.
// All output goes to w so a test can capture and assert the deterministic
// lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.knn=%d\n", cfg.knn)
	fmt.Fprintf(w, "config.region=%dx%d\n", cfg.width, cfg.height)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// Build the road network, then freeze it into the single immutable CSR
	// snapshot every algorithm reads. An immutable CSR needs no synchronisation
	// on the read path, so the three APSP passes could even run concurrently.
	gen, err := build(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	c := csr.BuildFromAdjList(gen.adj)
	mapper := gen.adj.Mapper()

	fmt.Fprintf(w, "graph.nodes=%d\n", c.Order())
	fmt.Fprintf(w, "graph.segments=%d\n", gen.segments)

	// Compute APSP three ways and time each pass. The Ctx variants honour
	// cancellation and surface a typed error rather than the nil-on-error of
	// the simple entry points.
	dj, djElapsed, err := timedAPSP(func() (*search.APSP[int64], error) {
		return search.DijkstraAPSPCtx[int64](ctx, c)
	})
	if err != nil {
		return fmt.Errorf("DijkstraAPSP: %w", err)
	}
	fw, fwElapsed, err := timedAPSP(func() (*search.APSP[int64], error) {
		return search.FloydWarshallCtx[int64](ctx, c)
	})
	if err != nil {
		return fmt.Errorf("FloydWarshall: %w", err)
	}
	jn, jnElapsed, err := timedAPSP(func() (*search.APSP[int64], error) {
		return search.JohnsonAPSPCtx[int64](ctx, c)
	})
	if err != nil {
		return fmt.Errorf("JohnsonAPSP: %w", err)
	}

	// Correctness oracle: on a positive-integer-weighted graph the shortest
	// distances are exact and unique, so all three matrices must be identical.
	// A mismatch is a module bug; surface the first differing pair.
	live := c.LiveNodes()
	agree := true
	if mm, ok := firstMismatch(dj, fw, live); ok {
		agree = false
		fmt.Fprintf(w, "# DISAGREE dijkstra-vs-floyd %s\n", mm)
	}
	if mm, ok := firstMismatch(dj, jn, live); ok {
		agree = false
		fmt.Fprintf(w, "# DISAGREE dijkstra-vs-johnson %s\n", mm)
	}
	fmt.Fprintf(w, "apsp_three_way_agree=%d\n", boolToInt(agree))

	// The parallel variants must be bit-identical to their serial counterparts
	// by construction (rmp #1680); verify it.
	workers := runtime.GOMAXPROCS(0)
	fwPar, err := search.FloydWarshallParallelCtx[int64](ctx, c, workers)
	if err != nil {
		return fmt.Errorf("FloydWarshallParallel: %w", err)
	}
	jnPar, err := search.JohnsonAPSPParallelCtx[int64](ctx, c, workers)
	if err != nil {
		return fmt.Errorf("JohnsonAPSPParallel: %w", err)
	}
	fwParAgree := true
	if mm, ok := firstMismatch(fw, fwPar, live); ok {
		fwParAgree = false
		fmt.Fprintf(w, "# DISAGREE floyd-serial-vs-parallel %s\n", mm)
	}
	jnParAgree := true
	if mm, ok := firstMismatch(jn, jnPar, live); ok {
		jnParAgree = false
		fmt.Fprintf(w, "# DISAGREE johnson-serial-vs-parallel %s\n", mm)
	}
	fmt.Fprintf(w, "floyd_parallel_agree=%d\n", boolToInt(fwParAgree))
	fmt.Fprintf(w, "johnson_parallel_agree=%d\n", boolToInt(jnParAgree))

	// Derive the graph metrics from the (agreed) distance matrix.
	m, err := metrics(dj, live, mapper)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	fmt.Fprintf(w, "metric.radius=%d\n", m.radius)
	fmt.Fprintf(w, "metric.diameter=%d\n", m.diameter)
	fmt.Fprintf(w, "metric.center_town=%d\n", m.centerTown)
	fmt.Fprintf(w, "metric.periphery_town=%d\n", m.peripheryTown)
	fmt.Fprintf(w, "metric.sample_town=%d\n", sampleTown)
	fmt.Fprintf(w, "metric.sample_eccentricity=%d\n", m.sampleEcc)
	fmt.Fprintf(w, "metric.distinct_eccentricities=%d\n", m.distinctEcc)

	built := readMem()
	// Dense distance-matrix footprint: V*V cells, each an int64 distance (8 B)
	// plus one reachability bit stored as a bool (1 B). This is the O(V^2)
	// result size every APSP algorithm materialises — the cost intrinsic to
	// all-pairs, independent of how sparse the road graph itself is.
	n := c.Order()
	matrixBytes := n * n * (8 + 1)
	fmt.Fprintf(w, "# matrix.result_footprint=%s\n", humanBytes(matrixBytes))
	fmt.Fprintf(w, "# build.elapsed=%s\n", gen.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# apsp.dijkstra.elapsed=%s\n", djElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# apsp.floyd_warshall.elapsed=%s\n", fwElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# apsp.johnson.elapsed=%s\n", jnElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(saturatingSub(built.HeapAlloc, base.HeapAlloc)))
	return nil
}

// timedAPSP runs one APSP computation, returning the result and its wall-clock
// duration. It keeps the timing boundary uniform across the three algorithms.
func timedAPSP(fn func() (*search.APSP[int64], error)) (*search.APSP[int64], time.Duration, error) {
	start := time.Now()
	out, err := fn()
	return out, time.Since(start), err
}

// ─────────────────────────────────────────────────────────────────────────────
// Correctness oracle
// ─────────────────────────────────────────────────────────────────────────────

// firstMismatch scans every ordered pair of live nodes and returns a
// description of the first cell where two APSP matrices disagree — either on
// reachability or on the distance value. It returns ok=false when the two
// matrices are identical over the live-node set. On this positive-integer graph
// the distances are exact, so any mismatch is a module bug.
func firstMismatch(a, b *search.APSP[int64], live []graph.NodeID) (string, bool) {
	for _, i := range live {
		for _, j := range live {
			av, aok := a.At(i, j)
			bv, bok := b.At(i, j)
			if aok != bok || av != bv {
				return fmt.Sprintf("at (%d,%d): a=(%d,%t) b=(%d,%t)", i, j, av, aok, bv, bok), true
			}
		}
	}
	return "", false
}

// ─────────────────────────────────────────────────────────────────────────────
// Graph metrics
// ─────────────────────────────────────────────────────────────────────────────

// graphMetrics holds the classical distance-based metrics derived from an APSP
// matrix. All fields are deterministic for a fixed configuration.
type graphMetrics struct {
	radius        int64 // minimum eccentricity over all towns
	diameter      int64 // maximum eccentricity over all towns
	centerTown    int   // a town value whose eccentricity equals the radius
	peripheryTown int   // a town value whose eccentricity equals the diameter
	sampleEcc     int64 // eccentricity of the fixed sample town
	distinctEcc   int   // number of distinct eccentricity values
}

// metrics derives radius, diameter, the centre and periphery towns, the sample
// town's eccentricity, and the count of distinct eccentricities from the APSP
// matrix. The eccentricity of a town is the greatest shortest-path distance
// from it to any other town; on a connected graph every town is reachable, so
// an unreachable pair means the connectivity invariant was violated and is
// reported as an error. Ties for centre / periphery are broken by the smallest
// town value so the reported towns are deterministic.
func metrics(a *search.APSP[int64], live []graph.NodeID, mapper *graph.Mapper[int]) (graphMetrics, error) {
	m := graphMetrics{radius: math.MaxInt64, diameter: math.MinInt64, centerTown: -1, peripheryTown: -1}
	distinct := make(map[int64]struct{}, len(live))
	for _, v := range live {
		ecc, err := eccentricity(a, v, live)
		if err != nil {
			return graphMetrics{}, err
		}
		town, ok := mapper.Resolve(v)
		if !ok {
			return graphMetrics{}, fmt.Errorf("unresolved live node id %d", v)
		}
		distinct[ecc] = struct{}{}
		if ecc < m.radius || (ecc == m.radius && town < m.centerTown) {
			m.radius, m.centerTown = ecc, town
		}
		if ecc > m.diameter || (ecc == m.diameter && town < m.peripheryTown) {
			m.diameter, m.peripheryTown = ecc, town
		}
		if town == sampleTown {
			m.sampleEcc = ecc
		}
	}
	m.distinctEcc = len(distinct)
	return m, nil
}

// eccentricity returns the greatest shortest-path distance from town v to any
// other live town. It errors if any town is unreachable from v, which on this
// connected-by-construction graph would indicate a broken invariant.
func eccentricity(a *search.APSP[int64], v graph.NodeID, live []graph.NodeID) (int64, error) {
	var ecc int64
	for _, u := range live {
		d, ok := a.At(v, u)
		if !ok {
			return 0, fmt.Errorf("town %d unreachable from town %d (graph must be connected)", u, v)
		}
		if d > ecc {
			ecc = d
		}
	}
	return ecc, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded generator
// ─────────────────────────────────────────────────────────────────────────────

// genResult reports the realised road network: the frozen builder, the number
// of undirected road segments actually wired (the MST and k-NN edge sets
// overlap, so this is known only after de-duplication), and the wall-clock cost
// of the build.
type genResult struct {
	adj      *adjlist.AdjList[int, int64]
	segments int
	elapsed  time.Duration
}

// point is a town's position in the plane.
type point struct {
	x, y float64
}

// edge is an undirected road segment between two towns, canonicalised so u < v.
type edge struct {
	u, v int
}

// build scatters cfg.nodes towns across the region with the seeded RNG, then
// wires two overlapping edge sets — the Euclidean minimum spanning tree (which
// guarantees connectivity for any seed) and each town's cfg.knn nearest
// neighbours (which add realistic redundancy) — into a fresh undirected
// adjlist. Segments are de-duplicated and inserted in canonical sorted order so
// the CSR build is a pure function of cfg, independent of map iteration order.
// The build honours ctx cancellation on a coarse interval.
func build(ctx context.Context, cfg config) (genResult, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional — the network must
	// reproduce a fixed shape for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	// Place the towns. Coordinates are drawn x-then-y per town in index order,
	// so the placement is a pure function of the seed.
	pts := make([]point, cfg.nodes)
	for i := range pts {
		pts[i] = point{x: rng.Float64() * float64(cfg.width), y: rng.Float64() * float64(cfg.height)}
	}

	segments := make(map[edge]struct{}, cfg.nodes*(cfg.knn+1))
	addSegment := func(a, b int) {
		if a > b {
			a, b = b, a
		}
		segments[edge{a, b}] = struct{}{}
	}

	if err := spanningTree(ctx, pts, addSegment); err != nil {
		return genResult{}, err
	}
	if err := nearestNeighbours(ctx, pts, cfg.knn, addSegment); err != nil {
		return genResult{}, err
	}

	// Materialise the de-duplicated segment set into an undirected adjlist in
	// canonical sorted order.
	ordered := make([]edge, 0, len(segments))
	for e := range segments {
		ordered = append(ordered, e)
	}
	slices.SortFunc(ordered, func(a, b edge) int {
		if a.u != b.u {
			return a.u - b.u
		}
		return a.v - b.v
	})
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	for _, e := range ordered {
		w := weight(pts[e.u], pts[e.v])
		if err := a.AddEdge(e.u, e.v, w); err != nil {
			return genResult{}, fmt.Errorf("AddEdge %d-%d: %w", e.u, e.v, err)
		}
	}

	return genResult{adj: a, segments: len(ordered), elapsed: time.Since(start)}, nil
}

// spanningTree wires a Euclidean minimum spanning tree over the complete point
// set using Prim's algorithm in O(V^2). The MST touches every town, so it alone
// makes the road network connected for any seed. Distance ties are broken by
// the lower node index, so the tree is deterministic.
func spanningTree(ctx context.Context, pts []point, addSegment func(a, b int)) error {
	n := len(pts)
	inTree := make([]bool, n)
	best := make([]float64, n)
	from := make([]int, n)
	for i := range best {
		best[i] = math.Inf(1)
		from[i] = -1
	}
	best[0] = 0
	for iter := 0; iter < n; iter++ {
		if iter&0x3F == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		// Select the closest town not yet in the tree (lowest index on a tie).
		u := -1
		for v := 0; v < n; v++ {
			if !inTree[v] && (u == -1 || best[v] < best[u]) {
				u = v
			}
		}
		inTree[u] = true
		if from[u] != -1 {
			addSegment(u, from[u])
		}
		for v := 0; v < n; v++ {
			if inTree[v] {
				continue
			}
			if d := dist(pts[u], pts[v]); d < best[v] {
				best[v] = d
				from[v] = u
			}
		}
	}
	return nil
}

// nearestNeighbours adds, for every town, an undirected segment to each of its
// k nearest neighbours. Candidates are ranked by ascending distance and then by
// ascending index, so the selection is deterministic even when two neighbours
// are equidistant.
func nearestNeighbours(ctx context.Context, pts []point, k int, addSegment func(a, b int)) error {
	n := len(pts)
	type cand struct {
		j int
		d float64
	}
	cands := make([]cand, 0, n-1)
	for i := 0; i < n; i++ {
		if i&0x3F == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		cands = cands[:0]
		for j := 0; j < n; j++ {
			if j != i {
				cands = append(cands, cand{j: j, d: dist(pts[i], pts[j])})
			}
		}
		slices.SortFunc(cands, func(a, b cand) int {
			switch {
			case a.d < b.d:
				return -1
			case a.d > b.d:
				return 1
			default:
				return a.j - b.j
			}
		})
		for t := 0; t < k && t < len(cands); t++ {
			addSegment(i, cands[t].j)
		}
	}
	return nil
}

// dist is the Euclidean distance between two towns.
func dist(a, b point) float64 {
	return math.Hypot(a.x-b.x, a.y-b.y)
}

// weight is the integer road length of a segment: the Euclidean distance
// rounded to the nearest unit and floored at 1, so every segment carries a
// strictly positive int64 weight (which every APSP algorithm accepts and which
// keeps the three distance matrices bit-identical).
func weight(a, b point) int64 {
	w := int64(math.Round(dist(a, b)))
	if w < 1 {
		return 1
	}
	return w
}

// ─────────────────────────────────────────────────────────────────────────────
// Small helpers and telemetry
// ─────────────────────────────────────────────────────────────────────────────

// boolToInt renders a boolean invariant as the integer 1 (true) or 0 (false)
// so it can be printed as a deterministic key=value fact.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// readMem returns a memory snapshot after forcing a GC so HeapAlloc reflects
// live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// saturatingSub returns a-b, or 0 when b > a (the heap can shrink between the
// two snapshots, which would otherwise underflow the unsigned subtraction).
func saturatingSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
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
