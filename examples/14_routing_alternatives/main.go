// Example 14_routing_alternatives — compare three flavours of
// shortest-path computation on ONE seeded coordinate routing graph:
// classical single-source Dijkstra, Yen's k-shortest loopless paths for
// ranked alternatives, and A* driven by a coordinate-based Euclidean
// heuristic that expands fewer nodes than Dijkstra for the same optimal
// cost.
//
// The A* section is the focus, and the evidence the example collects is
// the per-algorithm nodes-expanded count: A* with an admissible,
// consistent Euclidean heuristic settles strictly fewer nodes than
// Dijkstra (A* with the zero heuristic) while returning the identical
// optimal cost. The example QUANTIFIES that advantage rather than merely
// asserting it.
//
// # Generator
//
// The dataset is a seeded k-nearest-neighbour (k-NN) spatial graph over
// uniformly random 2D coordinates, made symmetric (each undirected road
// is stored as two opposite directed arcs), with a deterministic
// component-merge repair pass that guarantees a single connected
// component for ANY seed. Fixing -seed fixes the coordinates, the
// neighbour edges, and therefore every shortest path exactly.
//
// Parameters: -nodes points, -neighbours nearest neighbours each, coords
// in [0,scale]^2, seeded by -seed.
//
// # Weights and heuristic (integer A*, int64)
//
// Let D(a,b) be the Euclidean distance between two coordinates.
//
//	edge weight w(u,v) = max(1, ceil(D(u,v)))   // CEIL  -> w >= true distance
//	heuristic   h(u)   = floor(D(u,dst))         // FLOOR -> h <= true distance
//
// Admissible: h(u) = floor(D(u,dst)) <= D(u,dst) <= sum of real edge
// lengths on any u->dst path (a polyline is never shorter than its
// straight chord) <= sum of ceil(...) = the integer path cost. So h
// never overestimates the true remaining cost.
//
// Consistent: for every edge u->v,
//
//	h(u) = floor(D(u,dst)) <= D(u,dst) <= D(u,v) + D(v,dst)
//	     <= ceil(D(u,v)) + D(v,dst) < ceil(D(u,v)) + floor(D(v,dst)) + 1.
//
// All three terms h(u), ceil(D(u,v)) and floor(D(v,dst)) are integers,
// so the strict "< (...) + 1" over integers collapses to "<=", giving
// h(u) <= w(u,v) + h(v). Consistency holds, so A* never re-expands a
// settled node.
//
// Why CEIL the edge and FLOOR the heuristic (not round/round): rounding
// an edge DOWN could make w(u,v) < D(u,v) and break the lower-bound
// chain above. ceil never understates cost; floor never overstates the
// bound. The max(1, .) clamp only raises w, which only relaxes the
// consistency inequality, so it is safe and avoids degenerate zero-cost
// hops on coincident coordinates. h(dst) = floor(0) = 0, as A* requires.
//
// # Why k-NN (topology choice)
//
// Local, short edges (k-NN) force a long corner-to-corner corridor of
// many small hops, so A*'s straight-line heuristic prunes the lateral
// and backward fan that Dijkstra settles, producing a large, real
// nodes-expanded gap. A complete graph or a large-radius random
// geometric graph has a near-one-hop optimum, so the gap collapses to
// zero. k-NN also guarantees a bounded out-degree and no isolated
// vertices by construction, which a fixed-radius RGG does not. The
// source is the node nearest the (0,0) corner and the destination the
// node nearest the (scale,scale) corner: this maximises the corridor
// length, and therefore the absolute number of nodes A* skips, while
// staying fully deterministic for the seed.
//
// This design was settled with the graph-theory-expert sub-agent.
// References: Hart, Nilsson & Raphael (1968) — A* optimality under an
// admissible heuristic; Pearl, "Heuristics" (1984) — consistency implies
// no re-expansion; Penrose, "Random Geometric Graphs" (OUP 2003) — the
// RGG connectivity threshold the repair pass deterministically sidesteps.
//
// # Scale
//
// With no flags the example builds a small deterministic default (a few
// hundred nodes) whose result facts the regression test pins. Every
// dimension is a flag, so the same binary scales up to where the timing
// and expansion-count evidence becomes interesting:
//
//	go run ./examples/14_routing_alternatives                       # small deterministic default
//	go run ./examples/14_routing_alternatives -nodes 200000 -seed 7 # observable-scale run
//
// The deterministic facts (the optimal cost, the A* cost, and Yen's k
// path costs) are reproducible for a fixed -seed; only the telemetry
// (lines prefixed with "# ") varies between runs and machines.
package main

import (
	"container/heap"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// config captures every scale and shape knob of the routing benchmark.
// The zero value is not valid; build one with defaultConfig and override
// fields from flags (see main) or construct one directly (see the
// regression test).
type config struct {
	nodes      int     // number of coordinate nodes to generate
	neighbours int     // k: nearest neighbours each node links to (k-NN degree)
	scale      float64 // coordinates are drawn uniformly from [0, scale]^2
	k          int     // Yen's k: number of ranked shortest-path alternatives
	seed       int64   // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins. It is large enough that A* visibly prunes the frontier
// Dijkstra settles, yet small enough to build and query well under the
// short-layer 60 s package budget.
func defaultConfig() config {
	return config{
		nodes:      400,
		neighbours: 8,
		scale:      1000,
		k:          3,
		seed:       1,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape — for instance fewer nodes than the requested neighbour degree.
// It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.nodes < 2:
		return fmt.Errorf("nodes must be >= 2, got %d", c.nodes)
	case c.neighbours < 1:
		return fmt.Errorf("neighbours must be >= 1, got %d", c.neighbours)
	case c.neighbours >= c.nodes:
		return fmt.Errorf("neighbours (%d) must be < nodes (%d): not enough distinct neighbours", c.neighbours, c.nodes)
	case c.scale <= 0:
		return fmt.Errorf("scale must be > 0, got %g", c.scale)
	case c.k < 1:
		return fmt.Errorf("k (Yen alternatives) must be >= 1, got %d", c.k)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of coordinate nodes to generate")
	flag.IntVar(&cfg.neighbours, "neighbours", cfg.neighbours, "k-NN degree: nearest neighbours each node links to")
	flag.Float64Var(&cfg.scale, "scale", cfg.scale, "coordinate extent; coords are drawn from [0,scale]^2")
	flag.IntVar(&cfg.k, "k", cfg.k, "Yen's k: number of ranked shortest-path alternatives")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the seeded coordinate routing graph, runs the three
// shortest-path queries, and writes a report to w. Bare lines carry
// deterministic facts (path costs, reproducible for a fixed seed); lines
// prefixed with "# " carry volatile telemetry (durations, expansion
// counts and heap figures) that vary per run and per machine. All output
// goes to w so a test can capture and assert on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.neighbours=%d\n", cfg.neighbours)
	fmt.Fprintf(w, "config.scale=%g\n", cfg.scale)
	fmt.Fprintf(w, "config.k=%d\n", cfg.k)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	net, buildElapsed, err := buildNetwork(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Build-then-query workload: the graph is fully assembled, so compact
	// the adjacency backing arrays before freezing the CSR snapshot the
	// queries run against.
	if err := ctx.Err(); err != nil {
		return err
	}
	net.adj.Compact(ctx)
	c := csr.BuildFromAdjList(net.adj)

	fmt.Fprintf(w, "graph.nodes=%d\n", c.Order())
	fmt.Fprintf(w, "graph.edges=%d\n", c.Size())
	// Source and destination are reported as their stable coordinate
	// indices (deterministic for the seed), not the hash-scattered NodeIDs
	// the mapper assigns, which are an implementation detail.
	fmt.Fprintf(w, "graph.src=%d\n", net.srcIdx)
	fmt.Fprintf(w, "graph.dst=%d\n", net.dstIdx)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", buildElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# build.edge_rate=%.0f edges/s\n", rate(float64(c.Size()), buildElapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))
	fmt.Fprintf(w, "# mem.bytes_per_edge=%.1f\n",
		safeDiv(float64(built.HeapAlloc-base.HeapAlloc), float64(c.Size())))

	// The heuristic is reused by both the A* query and the expansion
	// counter, so it is built once here from the same coordinate array
	// the edge weights were derived from (see the doc comment: edges and
	// heuristic must share one coordinate set and one scale).
	h := net.heuristic(net.dstIdx)

	dijkstraCost, err := reportDijkstra(ctx, w, c, net.src, net.dst)
	if err != nil {
		return err
	}
	if err := reportYen(ctx, w, c, net.src, net.dst, cfg.k); err != nil {
		return err
	}
	astarCost, err := reportAStar(ctx, w, c, net.src, net.dst, h)
	if err != nil {
		return err
	}
	return reportExpansions(ctx, w, c, net.src, net.dst, h, dijkstraCost, astarCost)
}

// reportDijkstra runs a single-source Dijkstra from src, prints the
// optimal cost to dst as a deterministic fact and the wall-clock as
// telemetry, and returns the optimal cost for the later A* comparison.
func reportDijkstra(ctx context.Context, w io.Writer, c *csr.CSR[int64], src, dst graph.NodeID) (int64, error) {
	start := time.Now()
	d, err := search.DijkstraCtx(ctx, c, src)
	if err != nil {
		return 0, fmt.Errorf("dijkstra: %w", err)
	}
	elapsed := time.Since(start)
	cost, reachable := d.Distance(dst)
	if !reachable {
		return 0, fmt.Errorf("dst %d unreachable from src %d", dst, src)
	}
	fmt.Fprintf(w, "dijkstra.cost=%d\n", cost)
	fmt.Fprintf(w, "# dijkstra.latency=%s\n", elapsed.Round(time.Microsecond))
	return cost, nil
}

// reportYen prints the k ranked loopless shortest paths from src to dst
// computed with Yen's algorithm. Each path's cost is a deterministic
// fact; the costs are emitted in the ascending order Yen guarantees, so
// the regression test can assert the ordering. The query latency is
// telemetry.
func reportYen(ctx context.Context, w io.Writer, c *csr.CSR[int64], src, dst graph.NodeID, k int) error {
	start := time.Now()
	paths, err := search.YenKShortestCtx(ctx, c, src, dst, k)
	if err != nil {
		return fmt.Errorf("yen: %w", err)
	}
	elapsed := time.Since(start)
	fmt.Fprintf(w, "yen.count=%d\n", len(paths))
	for i, p := range paths {
		fmt.Fprintf(w, "yen.cost.%d=%d\n", i+1, p.Cost)
	}
	fmt.Fprintf(w, "# yen.latency=%s\n", elapsed.Round(time.Microsecond))
	return nil
}

// reportAStar runs A* with the coordinate-based Euclidean heuristic from
// src to dst, prints the optimal cost as a deterministic fact and the
// wall-clock as telemetry, and returns the cost so reportExpansions can
// cross-check it.
func reportAStar(ctx context.Context, w io.Writer, c *csr.CSR[int64], src, dst graph.NodeID, h func(graph.NodeID) int64) (int64, error) {
	start := time.Now()
	path, cost, err := search.AStarCtx(ctx, c, src, dst, h)
	if err != nil {
		return 0, fmt.Errorf("astar: %w", err)
	}
	elapsed := time.Since(start)
	fmt.Fprintf(w, "astar.cost=%d\n", cost)
	fmt.Fprintf(w, "astar.hops=%d\n", len(path)-1)
	fmt.Fprintf(w, "# astar.latency=%s\n", elapsed.Round(time.Microsecond))
	return cost, nil
}

// reportExpansions quantifies the benefit of the heuristic: A* expands
// no more nodes than Dijkstra (A* with the zero heuristic) while
// returning the same optimal cost. The expansion counts come from an
// instrumented runner (expansions) over the public NeighboursByID API,
// because the search package exposes no native nodes-expanded counter;
// the runner's cost is cross-checked against the library results passed
// in so the instrumented count cannot silently drift from the engine.
// Both counts and the speed-up are telemetry (machine-independent in
// value here, but reported as "# " because they describe the search
// effort, not the deterministic answer); the "same optimal cost"
// equality is a deterministic fact.
func reportExpansions(ctx context.Context, w io.Writer, c *csr.CSR[int64], src, dst graph.NodeID, h func(graph.NodeID) int64, dijkstraCost, astarCost int64) error {
	zeroH := func(graph.NodeID) int64 { return 0 }
	dijkstraExpanded, dijkstraRunCost, err := expansions(ctx, c, src, dst, zeroH)
	if err != nil {
		return fmt.Errorf("count Dijkstra expansions: %w", err)
	}
	astarExpanded, astarRunCost, err := expansions(ctx, c, src, dst, h)
	if err != nil {
		return fmt.Errorf("count A* expansions: %w", err)
	}
	if dijkstraRunCost != dijkstraCost || astarRunCost != astarCost {
		return fmt.Errorf(
			"expansion runner cost mismatch: dijkstra run=%d lib=%d, astar run=%d lib=%d",
			dijkstraRunCost, dijkstraCost, astarRunCost, astarCost,
		)
	}

	// The "same optimal cost" equality is the deterministic correctness
	// invariant the test pins; it must hold for any seed because the
	// heuristic is admissible.
	fmt.Fprintf(w, "astar_cost_equals_dijkstra=%t\n", astarCost == dijkstraCost)

	// The expansion counts and the resulting prune ratio are the search
	// evidence: A* settles fewer nodes for the same answer.
	fmt.Fprintf(w, "# expand.dijkstra=%d\n", dijkstraExpanded)
	fmt.Fprintf(w, "# expand.astar=%d\n", astarExpanded)
	fmt.Fprintf(w, "# expand.astar_saved=%d\n", dijkstraExpanded-astarExpanded)
	fmt.Fprintf(w, "# expand.astar_fraction=%.3f\n",
		safeDiv(float64(astarExpanded), float64(dijkstraExpanded)))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded coordinate-graph generator
// ─────────────────────────────────────────────────────────────────────────────

// point is a node's fixed 2D coordinate in [0,scale]^2 map units.
type point struct {
	x, y float64
}

// network bundles the materialised routing graph with the coordinate
// array the heuristic and edge weights are both derived from, the mapper
// that translates coordinate indices to graph NodeIDs, and the
// deterministically chosen source and destination — held as both the
// coordinate index and the resolved NodeID.
//
// Coordinate indices (0..nodes-1) and graph NodeIDs are NOT the same
// space: the adjacency Mapper is sharded by the hash of the value, so it
// scatters NodeIDs rather than assigning them in insertion order.
// Anything that crosses between the two spaces must go through the
// mapper, which is why the heuristic resolves a NodeID back to its
// coordinate index before measuring distance.
type network struct {
	adj            *adjlist.AdjList[int, int64]
	coords         []point
	mapper         *graph.Mapper[int]
	scale          float64
	srcIdx, dstIdx int          // coordinate indices
	src, dst       graph.NodeID // resolved graph NodeIDs
}

// dist returns the Euclidean distance between coordinate indices a and b.
func (n *network) dist(a, b int) float64 {
	dx := n.coords[a].x - n.coords[b].x
	dy := n.coords[a].y - n.coords[b].y
	return math.Hypot(dx, dy)
}

// heuristic returns the admissible, consistent A* heuristic for the
// destination coordinate index dstIdx: floor(D(node, dstIdx)). The A*
// engine and the expansion counter call it in NodeID space, so it
// resolves the NodeID back to a coordinate index through the mapper
// before measuring distance. See the package doc comment for the
// admissibility and consistency proofs. h(dst) is floor(0) = 0; an
// unresolvable id (a shard-padding slot the CSR never populated) yields
// 0, which is a safe lower bound.
func (n *network) heuristic(dstIdx int) func(graph.NodeID) int64 {
	return func(id graph.NodeID) int64 {
		idx, ok := n.mapper.Resolve(id)
		if !ok {
			return 0
		}
		return int64(math.Floor(n.dist(idx, dstIdx)))
	}
}

// checkEvery bounds how often the generator polls ctx for cancellation:
// often enough that a cancelled large build stops promptly, rare enough
// that the check is free relative to the surrounding work.
const checkEvery = 256

// buildNetwork materialises the seeded k-NN coordinate routing graph
// described by cfg and returns it alongside the wall-clock build time.
// It draws coordinates from the seeded RNG, links each node to its k
// nearest neighbours with symmetric (both-direction) Euclidean-weighted
// arcs, repairs the graph into a single connected component, and picks
// the source and destination nearest opposite corners. The build honours
// ctx cancellation on a periodic check.
func buildNetwork(ctx context.Context, cfg config) (*network, time.Duration, error) {
	start := time.Now()

	coords := randomCoords(cfg)

	adj := adjlist.New[int, int64](adjlist.Config{Directed: true})
	uf := newUnionFind(cfg.nodes)

	// k-NN edges, made symmetric. For each node, link to its k nearest
	// neighbours; addArc stores the reverse arc too, so the graph models a
	// two-way road network. Duplicate (u,v) arcs from the symmetric pass
	// collapse because the adjacency list is a simple graph.
	for i := 0; i < cfg.nodes; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		for _, j := range kNearest(coords, i, cfg.neighbours) {
			if err := addArc(adj, coords, i, j); err != nil {
				return nil, 0, err
			}
			uf.union(i, j)
		}
	}

	// Connectivity repair. k-NN over uniform coordinates is connected with
	// high probability but not with certainty for an arbitrary seed; this
	// component-merge pass turns "w.h.p." into a guarantee. While more than
	// one component remains, find the globally shortest edge between two
	// distinct components and add it (both directions). Every repair edge is
	// a genuine Euclidean edge, so the admissibility/consistency argument is
	// preserved unchanged.
	if err := repairConnectivity(ctx, adj, coords, uf); err != nil {
		return nil, 0, err
	}

	srcIdx := nearestCorner(coords, point{x: 0, y: 0})
	dstIdx := nearestCorner(coords, point{x: cfg.scale, y: cfg.scale})

	mapper := adj.Mapper()
	src, ok := mapper.Lookup(srcIdx)
	if !ok {
		return nil, 0, fmt.Errorf("source coordinate %d not interned in graph", srcIdx)
	}
	dst, ok := mapper.Lookup(dstIdx)
	if !ok {
		return nil, 0, fmt.Errorf("destination coordinate %d not interned in graph", dstIdx)
	}

	return &network{
		adj:    adj,
		coords: coords,
		mapper: mapper,
		scale:  cfg.scale,
		srcIdx: srcIdx,
		dstIdx: dstIdx,
		src:    src,
		dst:    dst,
	}, time.Since(start), nil
}

// randomCoords draws cfg.nodes uniform coordinates in [0,scale]^2 from a
// seeded RNG. Using a seeded math/rand is the point: the same -seed
// reproduces the exact coordinate set, and therefore the exact graph.
func randomCoords(cfg config) []point {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	pts := make([]point, cfg.nodes)
	for i := range pts {
		pts[i] = point{x: rng.Float64() * cfg.scale, y: rng.Float64() * cfg.scale}
	}
	return pts
}

// kNearest returns the indices of the k nodes nearest to node i
// (excluding i itself), by Euclidean distance. Ties are broken by node
// index so the neighbour set is fully deterministic. It is an O(n)
// scan plus an O(n log n) sort per call — O(n^2 log n) over the whole
// build, which is comfortable at example scale; a k-d tree would make
// it O(n log n) total for much larger graphs.
func kNearest(coords []point, i, k int) []int {
	type cand struct {
		idx int
		d2  float64 // squared distance avoids a sqrt in the comparison
	}
	cands := make([]cand, 0, len(coords)-1)
	for j := range coords {
		if j == i {
			continue
		}
		dx := coords[i].x - coords[j].x
		dy := coords[i].y - coords[j].y
		cands = append(cands, cand{idx: j, d2: dx*dx + dy*dy})
	}
	sort.Slice(cands, func(a, b int) bool {
		if cands[a].d2 != cands[b].d2 {
			return cands[a].d2 < cands[b].d2
		}
		return cands[a].idx < cands[b].idx
	})
	if k > len(cands) {
		k = len(cands)
	}
	out := make([]int, k)
	for n := 0; n < k; n++ {
		out[n] = cands[n].idx
	}
	return out
}

// addArc adds a directed edge i->j and its reverse j->i, both weighted
// by the integer Euclidean cost w = max(1, ceil(D(i,j))). The clamp to 1
// keeps the cost positive on coincident coordinates without breaking
// admissibility or consistency (it only raises w). The adjacency list is
// a simple graph, so a repeated (i,j) pair from the symmetric k-NN pass
// is idempotent.
func addArc(adj *adjlist.AdjList[int, int64], coords []point, i, j int) error {
	w := edgeWeight(coords, i, j)
	if err := adj.AddEdge(i, j, w); err != nil {
		return fmt.Errorf("AddEdge %d->%d: %w", i, j, err)
	}
	if err := adj.AddEdge(j, i, w); err != nil {
		return fmt.Errorf("AddEdge %d->%d: %w", j, i, err)
	}
	return nil
}

// edgeWeight returns the integer edge cost w = max(1, ceil(D(i,j))).
func edgeWeight(coords []point, i, j int) int64 {
	dx := coords[i].x - coords[j].x
	dy := coords[i].y - coords[j].y
	w := int64(math.Ceil(math.Hypot(dx, dy)))
	if w < 1 {
		w = 1
	}
	return w
}

// repairConnectivity adds the fewest Euclidean edges needed to merge the
// k-NN graph into a single connected component. It repeatedly finds the
// globally shortest edge joining two distinct components (per the
// union-find uf) and adds it in both directions, until one component
// remains. It honours ctx cancellation between merges.
func repairConnectivity(ctx context.Context, adj *adjlist.AdjList[int, int64], coords []point, uf *unionFind) error {
	for uf.components > 1 {
		if err := ctx.Err(); err != nil {
			return err
		}
		bi, bj, found := shortestCrossEdge(coords, uf)
		if !found {
			// Unreachable: with >= 2 nodes there is always a pair in
			// distinct components while components > 1. Guard defensively.
			return fmt.Errorf("connectivity repair found no cross-component edge with %d components", uf.components)
		}
		if err := addArc(adj, coords, bi, bj); err != nil {
			return fmt.Errorf("repair edge: %w", err)
		}
		uf.union(bi, bj)
	}
	return nil
}

// shortestCrossEdge returns the endpoints of the globally shortest edge
// joining two distinct union-find components, and whether one exists. It
// is an O(n^2) scan; the repair pass typically runs it only a handful of
// times because k-NN is almost always already connected.
func shortestCrossEdge(coords []point, uf *unionFind) (int, int, bool) {
	best := math.Inf(1)
	bi, bj, found := -1, -1, false
	for i := range coords {
		for j := i + 1; j < len(coords); j++ {
			if uf.find(i) == uf.find(j) {
				continue
			}
			dx := coords[i].x - coords[j].x
			dy := coords[i].y - coords[j].y
			if d2 := dx*dx + dy*dy; d2 < best {
				best, bi, bj, found = d2, i, j, true
			}
		}
	}
	return bi, bj, found
}

// nearestCorner returns the index of the node nearest the given corner,
// ties broken by node index for determinism.
func nearestCorner(coords []point, corner point) int {
	best := math.Inf(1)
	idx := 0
	for i := range coords {
		dx := coords[i].x - corner.x
		dy := coords[i].y - corner.y
		if d2 := dx*dx + dy*dy; d2 < best {
			best, idx = d2, i
		}
	}
	return idx
}

// ─────────────────────────────────────────────────────────────────────────────
// Union-find (connectivity tracking for the repair pass)
// ─────────────────────────────────────────────────────────────────────────────

// unionFind is a disjoint-set forest with path compression and union by
// rank, used only to count and merge connected components during
// generation.
type unionFind struct {
	parent     []int
	rank       []int
	components int
}

// newUnionFind returns a forest of n singleton sets.
func newUnionFind(n int) *unionFind {
	uf := &unionFind{
		parent:     make([]int, n),
		rank:       make([]int, n),
		components: n,
	}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

// find returns the representative of x's set, compressing the path.
func (uf *unionFind) find(x int) int {
	for uf.parent[x] != x {
		uf.parent[x] = uf.parent[uf.parent[x]]
		x = uf.parent[x]
	}
	return x
}

// union merges the sets containing a and b, decrementing the component
// count when they were distinct.
func (uf *unionFind) union(a, b int) {
	ra, rb := uf.find(a), uf.find(b)
	if ra == rb {
		return
	}
	if uf.rank[ra] < uf.rank[rb] {
		ra, rb = rb, ra
	}
	uf.parent[rb] = ra
	if uf.rank[ra] == uf.rank[rb] {
		uf.rank[ra]++
	}
	uf.components--
}

// ─────────────────────────────────────────────────────────────────────────────
// Instrumented expansion counter
// ─────────────────────────────────────────────────────────────────────────────

// expansions runs A* (Dijkstra is the special case h == 0) over the
// public CSR neighbour API and counts how many distinct nodes are
// settled (popped with their final distance) before dst is reached. It
// returns the expansion count and the optimal cost to dst.
//
// It exists because the search package exposes no native nodes-expanded
// counter, so the example measures what IS observable: a faithful,
// deterministic re-implementation of the engine's settle order used only
// to count expansions. run cross-checks its returned cost against
// search.DijkstraCtx / search.AStarCtx so the instrumented count cannot
// silently drift from the engine it illustrates. It honours ctx
// cancellation on a periodic check.
func expansions(
	ctx context.Context,
	c *csr.CSR[int64],
	src, dst graph.NodeID,
	h func(graph.NodeID) int64,
) (expanded int, cost int64, err error) {
	maxID := uint64(c.MaxNodeID())
	const unreached = math.MaxInt64
	g := make([]int64, maxID)
	for i := range g {
		g[i] = unreached
	}
	settled := make([]bool, maxID)

	pq := &fScoreHeap{}
	g[uint64(src)] = 0
	heap.Push(pq, fScoreItem{node: src, f: h(src)})

	pops := 0
	for pq.Len() > 0 {
		if pops%checkEvery == 0 {
			if e := ctx.Err(); e != nil {
				return 0, 0, e
			}
		}
		pops++
		top := heap.Pop(pq).(fScoreItem)
		if settled[uint64(top.node)] {
			continue
		}
		settled[uint64(top.node)] = true
		expanded++
		if top.node == dst {
			return expanded, g[uint64(dst)], nil
		}
		for nb, weight := range c.NeighboursByID(top.node) {
			cand := g[uint64(top.node)] + weight
			if cand < g[uint64(nb)] {
				g[uint64(nb)] = cand
				heap.Push(pq, fScoreItem{node: nb, f: cand + h(nb)})
			}
		}
	}
	return expanded, 0, fmt.Errorf("no path from %d to %d", src, dst)
}

// fScoreItem is one entry in the A* priority queue, ordered by f-score.
type fScoreItem struct {
	node graph.NodeID
	f    int64
}

// fScoreHeap is a min-heap of fScoreItem ordered by f, with ties broken
// by node id so the settle order is fully deterministic.
type fScoreHeap []fScoreItem

func (h fScoreHeap) Len() int { return len(h) }
func (h fScoreHeap) Less(i, j int) bool {
	if h[i].f != h[j].f {
		return h[i].f < h[j].f
	}
	return h[i].node < h[j].node
}
func (h fScoreHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *fScoreHeap) Push(x any) { *h = append(*h, x.(fScoreItem)) }

func (h *fScoreHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers (mirrors of example 26's)
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
func rate(count float64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return count / elapsed.Seconds()
}

// safeDiv divides a by b, returning 0 when b is 0.
func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
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
