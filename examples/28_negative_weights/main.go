// Example 28_negative_weights — single-source shortest paths over a graph
// with NEGATIVE edge weights, using Bellman-Ford where Dijkstra cannot,
// and cross-checking the result against Johnson's all-pairs reweighting.
//
// The subject is negative-weight routing. Dijkstra assumes non-negative
// edges and this module makes that explicit: search.Dijkstra returns
// search.ErrNegativeWeight (without traversing) the moment it sees a
// negative edge. search.BellmanFord is the algorithm that handles signed
// weights and, crucially, detects a negative cycle reachable from the
// source (search.ErrNegativeCycle). This example demonstrates both, and
// certifies the Bellman-Ford answer against two independent oracles.
//
// # Domain: a backhaul freight network with rebate lanes
//
// Freight leaves a single origin depot (the source) and flows downstream
// through tiers of transshipment hubs to a final tier of destination
// markets. Most lanes cost money (a positive shipping cost). Some lanes
// are BACKHAUL lanes: a carrier that would otherwise run an empty return
// leg pays a REBATE to fill it, so moving cargo along that lane has a
// negative net cost. Routing to a market therefore wants to chain rebate
// lanes where it can, which is exactly a negative-weight shortest-path
// problem.
//
// # Why there is provably no negative cycle (the acyclic instance)
//
// The freight network is a LAYERED DAG: every lane goes from tier t to
// tier t+1, never backward. A directed acyclic graph has no directed
// cycle at all, so it has no negative cycle regardless of how negative the
// rebate lanes are. Acyclicity is a genuine domain invariant here (goods
// flow one way, depot -> market), not a numerical trick, which is what
// lets the example carry arbitrarily large rebates and still guarantee a
// well-defined shortest-path answer for every seed.
//
// # The arbitrage instance (-arbitrage)
//
// A negative cycle in this domain is an "arbitrage loop": a sequence of
// lanes you could traverse forever, being paid net on every lap — free
// money, which is physically impossible and signals a modelling error.
// The -arbitrage flag injects exactly one back-edge (market-tier ->
// depot's first hub) whose weight makes the two-node loop strictly
// negative for ANY base weight. Bellman-Ford must then refuse to return
// distances and instead report search.ErrNegativeCycle. The example
// asserts that it does.
//
// # Correctness: a three-way oracle
//
// Bellman-Ford's single-source distances are cross-checked against two
// independent computations of the same quantity:
//
//  1. search.JohnsonAPSP — all-pairs shortest paths via a Bellman-Ford
//     reweighting that turns the signed graph into a non-negative one and
//     runs Dijkstra from every vertex. For integer weights Johnson
//     reproduces the exact distances, so the source row johnson.At(src, j)
//     must equal Bellman-Ford's Distance(j) for every node j.
//  2. A textbook full-edge-sweep Bellman-Ford implemented here over the
//     public CSR neighbour API, used both to COUNT relaxation passes (the
//     search package exposes no such counter) and as a second oracle. Its
//     distances must equal the library's; the example fails loudly if they
//     drift, so the instrumented counter cannot silently diverge from the
//     engine it illustrates.
//
// On the acyclic instance all three must agree on every distance; on the
// arbitrage instance all three must agree that a negative cycle exists.
// A disagreement is treated as a module defect and surfaced as an error.
//
// # Scale
//
// With no flags the example builds a small deterministic default whose
// result facts the regression test pins. Every dimension is a flag, so the
// same binary scales up to where the timing and allocation evidence is
// interesting:
//
//	go run ./examples/28_negative_weights                          # small deterministic default
//	go run ./examples/28_negative_weights -layers 12 -width 4000    # observable-scale run
//	go run ./examples/28_negative_weights -arbitrage                # inject a negative cycle
//
// Deterministic facts (bare lines) are reproducible for a fixed -seed;
// only telemetry (lines prefixed with "# ") varies per run and per machine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// config captures every scale and shape knob of the freight network. The
// zero value is not valid; build one with defaultConfig and override
// fields from flags (see main) or construct one directly (see the tests).
type config struct {
	layers     int     // number of tiers, including the single-node source tier
	width      int     // hubs/markets per non-source tier
	fanout     int     // forward lanes per hub into the next tier
	rebateFrac float64 // fraction of lanes that are rebate (negative) lanes
	maxCost    int64   // positive lane cost is drawn from [1, maxCost]
	maxRebate  int64   // rebate lane weight is drawn from [-maxRebate, -1]
	arbitrage  bool    // inject a negative cycle (an "arbitrage loop")
	seed       int64   // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins. It is large enough to chain several rebate lanes into a real
// negative-weight routing problem, yet small enough to build, solve, and
// cross-check against Johnson APSP well under the short-layer 60 s budget.
func defaultConfig() config {
	return config{
		layers:     6,
		width:      16,
		fanout:     4,
		rebateFrac: 0.30,
		maxCost:    100,
		maxRebate:  40,
		arbitrage:  false,
		seed:       1,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.layers < 2:
		return fmt.Errorf("layers must be >= 2, got %d", c.layers)
	case c.width < 1:
		return fmt.Errorf("width must be >= 1, got %d", c.width)
	case c.fanout < 1:
		return fmt.Errorf("fanout must be >= 1, got %d", c.fanout)
	case c.fanout > c.width:
		return fmt.Errorf("fanout (%d) must be <= width (%d): not enough distinct targets", c.fanout, c.width)
	case c.rebateFrac < 0 || c.rebateFrac > 1:
		return fmt.Errorf("rebate_frac must be in [0,1], got %g", c.rebateFrac)
	case c.maxCost < 1:
		return fmt.Errorf("max_cost must be >= 1, got %d", c.maxCost)
	case c.maxRebate < 1:
		return fmt.Errorf("max_rebate must be >= 1, got %d", c.maxRebate)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.layers, "layers", cfg.layers, "number of tiers, including the single-node source tier")
	flag.IntVar(&cfg.width, "width", cfg.width, "hubs/markets per non-source tier")
	flag.IntVar(&cfg.fanout, "fanout", cfg.fanout, "forward lanes per hub into the next tier")
	flag.Float64Var(&cfg.rebateFrac, "rebate-frac", cfg.rebateFrac, "fraction of lanes that are rebate (negative) lanes")
	flag.Int64Var(&cfg.maxCost, "max-cost", cfg.maxCost, "positive lane cost is drawn from [1, max-cost]")
	flag.Int64Var(&cfg.maxRebate, "max-rebate", cfg.maxRebate, "rebate lane weight is drawn from [-max-rebate, -1]")
	flag.BoolVar(&cfg.arbitrage, "arbitrage", cfg.arbitrage, "inject a negative cycle (an arbitrage loop) and assert it is detected")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	prof := exprof.Bind(flag.CommandLine)
	flag.Parse()

	if err := prof.Run(os.Stdout, func() error {
		return run(context.Background(), os.Stdout, cfg)
	}); err != nil {
		log.Fatal(err)
	}
}

// run builds the seeded freight network, solves single-source shortest
// paths with Bellman-Ford, and writes a report to w. Bare lines carry
// deterministic facts (path costs, counts, invariants — reproducible for a
// fixed seed); lines prefixed with "# " carry volatile telemetry
// (durations, relaxation passes, allocations, heap) that vary per run and
// per machine. All output goes to w so a test can capture and assert on
// the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.layers=%d\n", cfg.layers)
	fmt.Fprintf(w, "config.width=%d\n", cfg.width)
	fmt.Fprintf(w, "config.fanout=%d\n", cfg.fanout)
	fmt.Fprintf(w, "config.rebate_frac=%g\n", cfg.rebateFrac)
	fmt.Fprintf(w, "config.max_cost=%d\n", cfg.maxCost)
	fmt.Fprintf(w, "config.max_rebate=%d\n", cfg.maxRebate)
	fmt.Fprintf(w, "config.arbitrage=%t\n", cfg.arbitrage)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	net, buildElapsed, err := buildNetwork(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	net.adj.Compact(ctx)
	c := csr.BuildFromAdjList(net.adj)

	fmt.Fprintf(w, "graph.nodes=%d\n", c.Order())
	fmt.Fprintf(w, "graph.edges=%d\n", c.Size())
	// The source is reported as its stable domain index (0, the depot),
	// not the hash-scattered NodeID the mapper assigns.
	fmt.Fprintf(w, "graph.src=%d\n", net.srcIdx)
	fmt.Fprintf(w, "neg_edges=%d\n", net.negEdges)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", buildElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# build.edge_rate=%.0f edges/s\n", rate(float64(c.Size()), buildElapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))
	fmt.Fprintf(w, "# mem.bytes_per_edge=%.1f\n",
		safeDiv(float64(built.HeapAlloc-base.HeapAlloc), float64(c.Size())))

	if cfg.arbitrage {
		return reportArbitrage(ctx, w, c, net.src)
	}
	return reportAcyclic(ctx, w, c, net)
}

// reportAcyclic solves and certifies the negative-weight (but acyclic)
// instance. It runs Bellman-Ford, cross-checks its distances against
// Johnson APSP and a textbook reference, demonstrates that Dijkstra
// refuses the negative edges, reports the per-market distances, and
// reconstructs the shortest route to the cheapest market.
func reportAcyclic(ctx context.Context, w io.Writer, c *csr.CSR[int64], net *network) error {
	src := net.src

	// Bellman-Ford: the algorithm that handles negative edges.
	before := mallocs()
	bfStart := time.Now()
	bf, err := search.BellmanFordCtx(ctx, c, src)
	bfElapsed := time.Since(bfStart)
	if err != nil {
		if errors.Is(err, search.ErrNegativeCycle) {
			// The generator guarantees a DAG, so this is a module defect:
			// a false negative-cycle detection on an acyclic graph.
			return fmt.Errorf("MODULE BUG: BellmanFord reported ErrNegativeCycle on an acyclic (layered DAG) instance: %w", err)
		}
		return fmt.Errorf("bellman-ford: %w", err)
	}
	bfMallocs := mallocs() - before

	// Oracle 1: the textbook full-sweep reference (also counts passes).
	ref, err := referenceBellmanFord(ctx, c, src)
	if err != nil {
		return fmt.Errorf("reference bellman-ford: %w", err)
	}
	if ref.negCycle {
		return fmt.Errorf("MODULE BUG: reference Bellman-Ford found a negative cycle on an acyclic instance")
	}

	// Oracle 2: Johnson all-pairs reweighting.
	jStart := time.Now()
	apsp, err := search.JohnsonAPSPCtx(ctx, c)
	jElapsed := time.Since(jStart)
	if err != nil {
		if errors.Is(err, search.ErrNegativeCycle) {
			return fmt.Errorf("MODULE BUG: JohnsonAPSP reported ErrNegativeCycle on an acyclic instance: %w", err)
		}
		return fmt.Errorf("johnson apsp: %w", err)
	}

	// Cross-check every live node's distance across all three computations.
	matches, mismatch, err := crossCheck(c, src, bf, ref, apsp)
	if err != nil {
		return err
	}
	if !matches {
		// A genuine module defect: the library disagrees with an
		// independent oracle on a plain shortest-path distance.
		return fmt.Errorf("MODULE BUG: %s", mismatch)
	}

	// Dijkstra must refuse the negative edges (contract demonstration).
	dijkstraRejects, err := dijkstraRejectsNegatives(ctx, c, src, net.negEdges)
	if err != nil {
		return err
	}

	// Deterministic facts.
	fmt.Fprintf(w, "neg_cycle_detected=0\n")
	fmt.Fprintf(w, "dijkstra_rejects_negative=%t\n", dijkstraRejects)
	fmt.Fprintf(w, "bellman_ford_matches_johnson=%t\n", matches)

	if err := reportMarkets(w, net, bf); err != nil {
		return err
	}

	// Telemetry.
	fmt.Fprintf(w, "# bf.latency=%s\n", bfElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# bf.mallocs=%d\n", bfMallocs)
	fmt.Fprintf(w, "# bf.relaxation_passes=%d\n", ref.passes)
	fmt.Fprintf(w, "# bf.edge_relaxations=%d\n", ref.relaxations)
	fmt.Fprintf(w, "# johnson.latency=%s\n", jElapsed.Round(time.Microsecond))
	return nil
}

// reportMarkets reports the shortest-path cost from the depot to every
// destination market (the final tier), an aggregate fingerprint of that
// distance vector, and the reconstructed route to the cheapest market. All
// of these are deterministic facts for a fixed seed.
func reportMarkets(w io.Writer, net *network, bf *search.Distances[int64]) error {
	markets := net.tierIndices(net.cfg.layers - 1)

	var (
		sum       int64
		minCost   = int64(math.MaxInt64)
		maxCost   = int64(math.MinInt64)
		bestIdx   = -1
		reachable int
	)
	for pos, m := range markets {
		id, ok := net.mapper.Lookup(m)
		if !ok {
			return fmt.Errorf("market %d not interned in graph", m)
		}
		dist, ok := bf.Distance(id)
		if !ok {
			// Every market is reachable by construction (coverage links
			// every tier node to a reachable predecessor); a gap is a
			// generator invariant violation, surfaced rather than hidden.
			return fmt.Errorf("market %d (pos %d) unreachable from depot; generator invariant broken", m, pos)
		}
		fmt.Fprintf(w, "market.%d=%d\n", pos, dist)
		sum += dist
		reachable++
		if dist < minCost {
			minCost, bestIdx = dist, m
		}
		if dist > maxCost {
			maxCost = dist
		}
	}

	fmt.Fprintf(w, "markets.count=%d\n", reachable)
	fmt.Fprintf(w, "markets.dist_sum=%d\n", sum)
	fmt.Fprintf(w, "markets.dist_min=%d\n", minCost)
	fmt.Fprintf(w, "markets.dist_max=%d\n", maxCost)

	// Reconstruct and report the cheapest route (depot -> cheapest market).
	bestID, ok := net.mapper.Lookup(bestIdx)
	if !ok {
		return fmt.Errorf("cheapest market %d not interned in graph", bestIdx)
	}
	path := bf.Path(bestID)
	if len(path) == 0 {
		return fmt.Errorf("cheapest market %d has no reconstructable path", bestIdx)
	}
	domainPath, err := net.resolvePath(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "cheapest_market=%d\n", bestIdx)
	fmt.Fprintf(w, "cheapest_cost=%d\n", minCost)
	fmt.Fprintf(w, "cheapest_hops=%d\n", len(path)-1)
	fmt.Fprintf(w, "cheapest_path=%s\n", joinInts(domainPath, ">"))
	return nil
}

// reportArbitrage handles the -arbitrage instance: the injected negative
// cycle must be detected by Bellman-Ford, by Johnson, and by the textbook
// reference. A missed detection (or a spurious distance result) is a module
// defect and is surfaced as an error.
func reportArbitrage(ctx context.Context, w io.Writer, c *csr.CSR[int64], src graph.NodeID) error {
	bfStart := time.Now()
	_, bfErr := search.BellmanFordCtx(ctx, c, src)
	bfElapsed := time.Since(bfStart)
	bfDetects := errors.Is(bfErr, search.ErrNegativeCycle)
	if bfErr != nil && !bfDetects {
		return fmt.Errorf("bellman-ford (arbitrage): unexpected error: %w", bfErr)
	}
	if !bfDetects {
		return fmt.Errorf("MODULE BUG: BellmanFord returned distances on a graph with a reachable negative cycle; expected ErrNegativeCycle")
	}

	ref, err := referenceBellmanFord(ctx, c, src)
	if err != nil {
		return fmt.Errorf("reference bellman-ford (arbitrage): %w", err)
	}
	if !ref.negCycle {
		return fmt.Errorf("MODULE BUG: reference Bellman-Ford failed to find the injected negative cycle")
	}

	_, jErr := search.JohnsonAPSPCtx(ctx, c)
	johnsonDetects := errors.Is(jErr, search.ErrNegativeCycle)
	if jErr != nil && !johnsonDetects {
		return fmt.Errorf("johnson apsp (arbitrage): unexpected error: %w", jErr)
	}
	if !johnsonDetects {
		return fmt.Errorf("MODULE BUG: JohnsonAPSP did not report the injected negative cycle; expected ErrNegativeCycle")
	}

	fmt.Fprintf(w, "neg_cycle_detected=1\n")
	fmt.Fprintf(w, "bellman_ford_detects=%t\n", bfDetects)
	fmt.Fprintf(w, "johnson_detects=%t\n", johnsonDetects)
	fmt.Fprintf(w, "reference_detects=%t\n", ref.negCycle)
	// Both the library and the Johnson oracle agree a negative cycle exists.
	fmt.Fprintf(w, "bellman_ford_matches_johnson=%t\n", bfDetects && johnsonDetects)
	fmt.Fprintf(w, "# bf.latency=%s\n", bfElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# bf.relaxation_passes=%d\n", ref.passes)
	return nil
}

// dijkstraRejectsNegatives runs Dijkstra on the signed graph and reports
// whether it correctly refuses with ErrNegativeWeight. When the graph has
// negative edges (the usual case) Dijkstra's contract requires the refusal;
// a Dijkstra that silently proceeds over negative edges would be a module
// defect and is surfaced as an error. When there are no negative edges the
// check is vacuously true.
func dijkstraRejectsNegatives(ctx context.Context, c *csr.CSR[int64], src graph.NodeID, negEdges int) (bool, error) {
	_, err := search.DijkstraCtx(ctx, c, src)
	if negEdges == 0 {
		return err == nil, nil
	}
	if errors.Is(err, search.ErrNegativeWeight) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("dijkstra (negative instance): unexpected error: %w", err)
	}
	return false, fmt.Errorf("MODULE BUG: Dijkstra accepted a graph with %d negative edge(s); expected ErrNegativeWeight", negEdges)
}

// ─────────────────────────────────────────────────────────────────────────────
// Three-way cross-check (Bellman-Ford vs Johnson vs textbook reference)
// ─────────────────────────────────────────────────────────────────────────────

// crossCheck verifies that the library Bellman-Ford (bf), the textbook
// reference (ref), and Johnson APSP (apsp) agree on the distance from src
// to every live node — both on reachability and, where reachable, on the
// exact value. It returns whether they all match; on the first mismatch it
// returns a human-readable description so the caller can surface it as a
// module defect. Integer weights make Johnson exact, so an honest engine
// must agree to the unit.
func crossCheck(
	c *csr.CSR[int64],
	src graph.NodeID,
	bf *search.Distances[int64],
	ref *refResult,
	apsp *search.APSP[int64],
) (ok bool, mismatch string, err error) {
	for _, j := range c.LiveNodes() {
		bfDist, bfOK := bf.Distance(j)
		jDist, jOK := apsp.At(src, j)
		refDist := ref.dist[uint64(j)]
		refOK := refDist != refInf

		if bfOK != jOK || bfOK != refOK {
			return false, fmt.Sprintf(
				"reachability disagreement for node %d: bellman-ford=%t johnson=%t reference=%t",
				j, bfOK, jOK, refOK), nil
		}
		if !bfOK {
			continue
		}
		if bfDist != jDist {
			return false, fmt.Sprintf(
				"distance disagreement for node %d: bellman-ford=%d johnson=%d",
				j, bfDist, jDist), nil
		}
		if bfDist != refDist {
			return false, fmt.Sprintf(
				"distance disagreement for node %d: bellman-ford=%d reference=%d",
				j, bfDist, refDist), nil
		}
	}
	return true, "", nil
}

// refInf is the "unreached" sentinel of the reference Bellman-Ford. Real
// path costs never approach it because they are bounded by
// layers*max_cost, so it is safe to test equality against it.
const refInf = int64(math.MaxInt64)

// refResult is the output of the instrumented textbook Bellman-Ford: the
// distance vector (indexed by NodeID, refInf where unreached), the number
// of full relaxation passes performed, the total number of successful edge
// relaxations, and whether a negative cycle was detected.
type refResult struct {
	dist        []int64
	passes      int
	relaxations int64
	negCycle    bool
}

// referenceBellmanFord is a faithful, deterministic re-implementation of
// single-source Bellman-Ford over the public CSR neighbour API. It exists
// because the search package exposes no relaxation-pass counter, so the
// example measures what IS observable and, at the same time, obtains a
// second independent oracle for crossCheck. It performs full edge sweeps,
// stops early when a pass changes nothing, and then runs one detection
// sweep: on a graph without a reachable negative cycle the sweep finds
// nothing; on one with such a cycle a relaxation is always still possible,
// so the sweep sets negCycle. It honours ctx cancellation on each pass.
func referenceBellmanFord(ctx context.Context, c *csr.CSR[int64], src graph.NodeID) (*refResult, error) {
	n := uint64(c.MaxNodeID())
	dist := make([]int64, n)
	for i := range dist {
		dist[i] = refInf
	}
	dist[uint64(src)] = 0

	live := c.LiveNodes()
	res := &refResult{dist: dist}

	// Number of vertices on any path is bounded by the live-node count, so
	// (live-1) full passes suffice to converge when there is no negative
	// cycle; the detection sweep below is the authority on cycle presence.
	maxPasses := len(live)
	for p := 0; p < maxPasses; p++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		changed := false
		for _, u := range live {
			du := dist[uint64(u)]
			if du == refInf {
				continue
			}
			for v, wt := range c.NeighboursByID(u) {
				if nd := du + wt; nd < dist[uint64(v)] {
					dist[uint64(v)] = nd
					res.relaxations++
					changed = true
				}
			}
		}
		res.passes = p + 1
		if !changed {
			break
		}
	}

	// Detection sweep: any further possible relaxation means a reachable
	// negative cycle.
	for _, u := range live {
		du := dist[uint64(u)]
		if du == refInf {
			continue
		}
		for v, wt := range c.NeighboursByID(u) {
			if du+wt < dist[uint64(v)] {
				res.negCycle = true
				return res, nil
			}
		}
	}
	return res, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded layered-DAG freight generator
// ─────────────────────────────────────────────────────────────────────────────

// network bundles the materialised freight graph with the mapper that
// translates domain indices to graph NodeIDs, the resolved source NodeID,
// the count of negative (rebate) lanes generated, and the config the shape
// was built from.
//
// Domain indices (0..nodes-1) and graph NodeIDs are NOT the same space: the
// adjacency Mapper shards by the hash of the value, so it scatters NodeIDs
// rather than assigning them in insertion order. Anything crossing between
// the two spaces goes through the mapper.
type network struct {
	adj      *adjlist.AdjList[int, int64]
	mapper   *graph.Mapper[int]
	cfg      config
	srcIdx   int          // domain index of the depot (always 0)
	src      graph.NodeID // resolved depot NodeID
	negEdges int          // number of rebate (negative-weight) lanes
}

// tierIndices returns the domain indices of the nodes in tier t. Tier 0 is
// the single-node source tier {0}; tier t>=1 holds width nodes laid out
// contiguously after the source.
func (n *network) tierIndices(t int) []int {
	if t == 0 {
		return []int{0}
	}
	start := 1 + (t-1)*n.cfg.width
	out := make([]int, n.cfg.width)
	for i := range out {
		out[i] = start + i
	}
	return out
}

// resolvePath translates a path of graph NodeIDs into domain indices via
// the mapper, so a reconstructed route can be printed in the stable domain
// space rather than the hash-scattered NodeID space.
func (n *network) resolvePath(path []graph.NodeID) ([]int, error) {
	out := make([]int, len(path))
	for i, id := range path {
		idx, ok := n.mapper.Resolve(id)
		if !ok {
			return nil, fmt.Errorf("path node %d does not resolve to a domain index", id)
		}
		out[i] = idx
	}
	return out, nil
}

// checkEvery bounds how often the generator polls ctx for cancellation.
const checkEvery = 8

// buildNetwork materialises the seeded layered-DAG freight network and
// returns it alongside the wall-clock build time. It links every tier to
// the next with forward-only lanes (guaranteeing a DAG, hence no negative
// cycle), makes a fraction of lanes rebate (negative) lanes, guarantees
// every node is reachable from the depot, and — when cfg.arbitrage is set —
// injects one back-edge forming a strictly negative two-node cycle. The
// build honours ctx cancellation on a periodic check.
func buildNetwork(ctx context.Context, cfg config) (*network, time.Duration, error) {
	start := time.Now()

	adj := adjlist.New[int, int64](adjlist.Config{Directed: true})
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))

	net := &network{adj: adj, cfg: cfg, srcIdx: 0}
	g := &genState{net: net, rng: rng, seen: make(map[[2]int]struct{})}

	// Forward lanes, tier t -> tier t+1, for every t.
	for t := 0; t < cfg.layers-1; t++ {
		if t%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		sources := net.tierIndices(t)
		targets := net.tierIndices(t + 1)

		// Coverage: every target gets at least one lane from a tier-t node
		// (round-robin), so — since tier t is reachable by induction from
		// the depot — every target is reachable too.
		for k, tgt := range targets {
			src := sources[k%len(sources)]
			if err := g.addLane(src, tgt); err != nil {
				return nil, 0, err
			}
		}

		// Extra fan-out: each source adds up to fanout-1 more forward lanes
		// to random distinct targets in the next tier.
		for _, u := range sources {
			for e := 0; e < cfg.fanout-1; e++ {
				tgt := targets[rng.Intn(len(targets))]
				if err := g.addLane(u, tgt); err != nil {
					return nil, 0, err
				}
			}
		}
	}

	// Optionally inject an arbitrage loop. The depot (0) always has a
	// coverage lane to the first tier-1 hub (index 1). Adding the reverse
	// lane 1 -> 0 with weight -(max_cost)-1 makes the loop 0 -> 1 -> 0 cost
	// w(0,1) + (-(max_cost)-1) <= max_cost - max_cost - 1 = -1 < 0 for ANY
	// base weight w(0,1) in [-max_rebate, max_cost]. The loop is reachable
	// from the depot, so Bellman-Ford must detect it.
	if cfg.arbitrage {
		if err := adj.AddEdge(1, 0, -(cfg.maxCost)-1); err != nil {
			return nil, 0, fmt.Errorf("inject arbitrage back-edge: %w", err)
		}
		net.negEdges++
	}

	mapper := adj.Mapper()
	srcID, ok := mapper.Lookup(net.srcIdx)
	if !ok {
		return nil, 0, fmt.Errorf("depot index %d not interned in graph", net.srcIdx)
	}
	net.mapper = mapper
	net.src = srcID

	return net, time.Since(start), nil
}

// genState carries the mutable bookkeeping of the generator: the network
// being built, the seeded RNG, a dedup set of already-added lanes, and the
// running negative-lane count.
type genState struct {
	net  *network
	rng  *rand.Rand
	seen map[[2]int]struct{}
}

// addLane adds a forward lane u->v with a seeded weight, deduplicating
// repeated (u,v) pairs. A duplicate is skipped WITHOUT drawing a weight, so
// the RNG stream — and therefore the whole data shape — stays deterministic
// for a fixed seed. Roughly rebateFrac of lanes are rebate (negative) lanes.
func (g *genState) addLane(u, v int) error {
	key := [2]int{u, v}
	if _, ok := g.seen[key]; ok {
		return nil
	}
	w := g.drawWeight()
	if err := g.net.adj.AddEdge(u, v, w); err != nil {
		return fmt.Errorf("AddEdge %d->%d: %w", u, v, err)
	}
	g.seen[key] = struct{}{}
	if w < 0 {
		g.net.negEdges++
	}
	return nil
}

// drawWeight returns the next lane weight from the seeded stream: a rebate
// (negative) weight in [-max_rebate, -1] with probability rebateFrac, else
// a positive shipping cost in [1, max_cost].
func (g *genState) drawWeight() int64 {
	cfg := g.net.cfg
	if g.rng.Float64() < cfg.rebateFrac {
		return -(1 + g.rng.Int63n(cfg.maxRebate))
	}
	return 1 + g.rng.Int63n(cfg.maxCost)
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

// mallocs returns the process-cumulative heap-object allocation count. The
// difference between two samples attributes allocations to the work
// between them (used to report the Bellman-Ford query's allocation count).
func mallocs() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Mallocs
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
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

// joinInts renders a slice of ints as sep-separated text (used to print a
// reconstructed route in the stable domain-index space).
func joinInts(xs []int, sep string) string {
	if len(xs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, x := range xs {
		if i > 0 {
			b.WriteString(sep)
		}
		fmt.Fprintf(&b, "%d", x)
	}
	return b.String()
}
