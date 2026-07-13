// Example 30_min_spanning_tree — minimum-cost backbone design over one shared,
// immutable CSR snapshot: it builds a seeded, scale-parametrised geographic
// site network, then computes its minimum spanning tree with BOTH of GoGraph's
// MST algorithms — Prim (search.PrimMST) and Kruskal (search.KruskalMST) — and
// cross-checks them against each other as a correctness oracle.
//
// # Domain — laying a least-cost fibre backbone
//
// The scenario is a telecom/utility planner who must interconnect a set of
// physical sites (exchanges, cabinets, substations) with the cheapest possible
// cable run. Every site has a 2-D geographic position; the cost of a candidate
// link is its Euclidean distance in metres. Many candidate links exist (the
// planner surveyed more routes than they will build), so the optimal backbone
// is the minimum-weight set of links that keeps every site connected — exactly
// a minimum spanning tree (one connected region) or a minimum spanning forest
// (several regions that are not interconnected).
//
// The seeded generator lays out `regions` metro areas separated horizontally on
// the plane. Within a region it first wires a random spanning tree (so the
// region is always connected), then adds `extra-edges` redundant candidate
// links per site to nearby-or-random peers, so the graph has strictly more
// candidate links than a tree and the MST algorithms must genuinely SELECT the
// cheapest connecting subset rather than copy a fixed tree. When -interconnect
// is set (the default) consecutive regions are chained by one long inter-region
// link, yielding a single connected backbone; with -interconnect=false the
// regions stay disjoint and the result is a spanning FOREST of `regions` trees.
//
// # Correctness oracle
//
// Prim and Kruskal are independent algorithms; on the same undirected weighted
// graph they must agree. run() asserts, and returns a "MODULE BUG" error if any
// of these fail (the failing config and seed are a complete repro):
//
//   - equal TOTAL weight — Prim summed over every connected component equals
//     Kruskal's whole-graph total;
//   - equal sorted multiset of edge WEIGHTS — a theorem: all minimum spanning
//     trees of a graph share the same multiset of edge weights, so this holds
//     even when ties let the two algorithms pick different edges;
//   - when every candidate weight is distinct the MST is unique, so the two
//     algorithms must select the identical set of undirected links;
//   - spanning-forest shape — exactly V-K edges for V live sites in K
//     components, and V_c-1 edges within each component c (a tree).
//
// If any check trips, the discrepancy is surfaced as an error rather than
// silently reported as a passing fact.
//
// # Scale
//
// Run with no flags, the example builds a small deterministic default
// (3 regions of 40 sites, 2 extra links per site, interconnected — 120 sites)
// that the regression test pins and that runs in microseconds. Every dimension
// is a flag, so the same binary scales up to where the per-algorithm cost is
// observable:
//
//	go run ./examples/30_min_spanning_tree -regions 4 -sites 200000 -extra-edges 3 -seed 7
//
// That is ~800 000 sites and ~3.2M candidate links; Kruskal's edge sort then
// dominates and its wall-clock and allocation cost become measurable. The
// deterministic facts are reproducible for a fixed -seed; only the telemetry
// (lines prefixed with "# ") varies between runs and machines.
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
	"sort"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// config captures every scale and shape knob of the site-network generator.
// The zero value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression test).
type config struct {
	regions        int     // number of geographic metro areas
	sitesPerRegion int     // sites placed in each region
	extraEdges     int     // redundant candidate links added per site
	span           float64 // region-local coordinate span, in metres
	regionGap      float64 // horizontal offset between consecutive region origins, in metres
	interconnect   bool    // chain consecutive regions into one connected backbone
	seed           int64   // RNG seed; fixes the deterministic network shape
}

// defaultConfig returns the small deterministic default the regression test
// pins: three interconnected regions of 40 sites, with two redundant candidate
// links per site. That is 120 sites and a few hundred candidate links, so both
// MST passes run in microseconds — well under the short-layer package budget.
func defaultConfig() config {
	return config{
		regions:        3,
		sitesPerRegion: 40,
		extraEdges:     2,
		span:           10000,
		regionGap:      50000,
		interconnect:   true,
		seed:           1,
	}
}

// validate rejects a configuration that cannot produce the requested shape. It
// is checked once, at the boundary, before any work. A region needs at least
// two sites to carry an edge (a lone site would be an isolated component the
// spanning forest cannot reach), and the geometry spans must be positive.
func (c config) validate() error {
	switch {
	case c.regions <= 0:
		return fmt.Errorf("regions must be > 0, got %d", c.regions)
	case c.sitesPerRegion < 2:
		return fmt.Errorf("sites (per region) must be >= 2, got %d", c.sitesPerRegion)
	case c.extraEdges < 0:
		return fmt.Errorf("extra-edges must be >= 0, got %d", c.extraEdges)
	case c.span <= 0:
		return fmt.Errorf("span must be > 0, got %g", c.span)
	case c.regionGap < 0:
		return fmt.Errorf("region-gap must be >= 0, got %g", c.regionGap)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.regions, "regions", cfg.regions, "number of geographic metro areas")
	flag.IntVar(&cfg.sitesPerRegion, "sites", cfg.sitesPerRegion, "sites placed in each region")
	flag.IntVar(&cfg.extraEdges, "extra-edges", cfg.extraEdges, "redundant candidate links added per site")
	flag.Float64Var(&cfg.span, "span", cfg.span, "region-local coordinate span, in metres")
	flag.Float64Var(&cfg.regionGap, "region-gap", cfg.regionGap, "horizontal offset between region origins, in metres")
	flag.BoolVar(&cfg.interconnect, "interconnect", cfg.interconnect, "chain consecutive regions into one connected backbone")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic network shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the site network described by cfg, freezes it into one CSR
// snapshot, computes its MST with both Prim and Kruskal, cross-checks the two
// as a correctness oracle, and writes a report to w. Bare lines carry
// deterministic facts (site and link counts, component count, the optimal
// backbone's total/min/max link cost — reproducible for a fixed seed); lines
// prefixed with "# " carry volatile telemetry (per-algorithm wall-clock and
// allocations, build cost, heap, and the backbone's cost saving over building
// every candidate link) that varies per run and machine.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.regions=%d\n", cfg.regions)
	fmt.Fprintf(w, "config.sites_per_region=%d\n", cfg.sitesPerRegion)
	fmt.Fprintf(w, "config.extra_edges=%d\n", cfg.extraEdges)
	fmt.Fprintf(w, "config.interconnect=%t\n", cfg.interconnect)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// Build the mutable undirected weighted graph, then freeze it into the
	// single immutable CSR snapshot both MST algorithms read. An immutable CSR
	// needs no synchronisation on the read path, so Prim and Kruskal could even
	// run concurrently against it.
	a, gen, err := build(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	c := csr.BuildFromAdjList(a)

	live := c.LiveNodes()
	fmt.Fprintf(w, "sites.total=%d\n", len(live))
	fmt.Fprintf(w, "links.total=%d\n", gen.links)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", gen.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))

	// Independent component count: the symmetric closure of the candidate-link
	// relation partitions the sites into weakly-connected regions. A connected
	// backbone has one; a disjoint set of regions has one per region.
	component, k, err := search.WCC(c)
	if err != nil {
		return fmt.Errorf("wcc: %w", err)
	}
	fmt.Fprintf(w, "graph.component_count=%d\n", k)

	// ── Kruskal: whole-graph minimum spanning forest ────────────────────────
	kStart := time.Now()
	kMem := readMem()
	kruskalEdges, kruskalTotal, err := search.KruskalMSTCtx(ctx, c)
	if err != nil {
		return fmt.Errorf("kruskal: %w", err)
	}
	kAfter := readMem()

	// ── Prim: one tree per connected component, summed ──────────────────────
	pStart := time.Now()
	pMem := readMem()
	primEdges, primWeights, primTotal, err := primForest(ctx, c, component, k, live)
	if err != nil {
		return fmt.Errorf("prim: %w", err)
	}
	pAfter := readMem()

	// ── Correctness oracle: Prim and Kruskal must agree ─────────────────────
	kruskalWeights := make([]int64, len(kruskalEdges))
	kruskalPairs := make([][2]uint64, len(kruskalEdges))
	for i, e := range kruskalEdges {
		kruskalWeights[i] = e.Weight
		kruskalPairs[i] = canonicalPair(e.From, e.To)
	}
	if err := checkAgree(cfg, k, len(live), primTotal, kruskalTotal,
		primWeights, kruskalWeights, primEdges, kruskalPairs); err != nil {
		return err
	}

	// The oracle passed, so Prim and Kruskal agree — report a single MST total.
	minCost, maxCost := extent(kruskalWeights)
	fmt.Fprintf(w, "mst.total_weight=%d\n", kruskalTotal)
	fmt.Fprintf(w, "mst.edge_count=%d\n", len(kruskalEdges))
	fmt.Fprintf(w, "mst.min_link_cost=%d\n", minCost)
	fmt.Fprintf(w, "mst.max_link_cost=%d\n", maxCost)

	fmt.Fprintf(w, "# kruskal.elapsed=%s\n", time.Since(kStart).Round(time.Microsecond))
	fmt.Fprintf(w, "# kruskal.mallocs=%d\n", kAfter.Mallocs-kMem.Mallocs)
	fmt.Fprintf(w, "# prim.elapsed=%s\n", time.Since(pStart).Round(time.Microsecond))
	fmt.Fprintf(w, "# prim.mallocs=%d\n", pAfter.Mallocs-pMem.Mallocs)
	// The backbone's cost saving over naively building every surveyed link.
	if gen.allLinksCost > 0 {
		saving := 100 * (1 - float64(kruskalTotal)/float64(gen.allLinksCost))
		fmt.Fprintf(w, "# mst.savings_pct=%.1f\n", saving)
	}
	return nil
}

// primForest runs Prim's algorithm once per connected component (rooted at the
// first live site encountered in each) and returns the union of the component
// trees: the canonical undirected link of each tree edge, that link's weight,
// and the summed total weight. component is WCC's per-NodeID label slice, k the
// component count, and live the live NodeID set.
func primForest(ctx context.Context, c *csr.CSR[int64], component []int, k int, live []graph.NodeID) (edges [][2]uint64, weights []int64, total int64, err error) {
	// One representative live site per component: the first we meet.
	reps := make([]graph.NodeID, k)
	seen := make([]bool, k)
	for _, id := range live {
		lbl := component[uint64(id)]
		if lbl < 0 || lbl >= k {
			return nil, nil, 0, fmt.Errorf("live site %d has out-of-range component label %d", id, lbl)
		}
		if !seen[lbl] {
			seen[lbl] = true
			reps[lbl] = id
		}
	}

	for lbl := 0; lbl < k; lbl++ {
		if !seen[lbl] {
			return nil, nil, 0, fmt.Errorf("component %d has no representative live site", lbl)
		}
		parent, found, w, perr := search.PrimMSTCtx(ctx, c, reps[lbl])
		if perr != nil {
			return nil, nil, 0, fmt.Errorf("prim component %d: %w", lbl, perr)
		}
		total += w
		for v := 0; v < len(found); v++ {
			if !found[v] {
				continue
			}
			id := graph.NodeID(v)
			if id == reps[lbl] {
				continue // the root has no incoming tree edge
			}
			p := parent[v]
			ew, ok := edgeWeight(c, p, id)
			if !ok {
				return nil, nil, 0, fmt.Errorf("prim tree edge %d-%d absent from CSR", p, id)
			}
			edges = append(edges, canonicalPair(p, id))
			weights = append(weights, ew)
		}
	}
	return edges, weights, total, nil
}

// checkAgree is the correctness oracle: it verifies that the Prim and Kruskal
// results describe the same minimum spanning forest, returning a descriptive
// "MODULE BUG" error (with cfg and seed as a repro) on any disagreement.
func checkAgree(cfg config, k, liveN int, primTotal, kruskalTotal int64,
	primWeights, kruskalWeights []int64, primEdges, kruskalPairs [][2]uint64) error {
	repro := fmt.Sprintf("(regions=%d sites=%d extra-edges=%d interconnect=%t seed=%d)",
		cfg.regions, cfg.sitesPerRegion, cfg.extraEdges, cfg.interconnect, cfg.seed)

	// (1) Equal total weight.
	if primTotal != kruskalTotal {
		return fmt.Errorf("MODULE BUG: Prim total %d != Kruskal total %d %s",
			primTotal, kruskalTotal, repro)
	}
	// Prim's summed total must equal the sum of its own per-edge weights.
	var primEdgeSum int64
	for _, w := range primWeights {
		primEdgeSum += w
	}
	if primEdgeSum != primTotal {
		return fmt.Errorf("MODULE BUG: Prim edge-weight sum %d != Prim total %d %s",
			primEdgeSum, primTotal, repro)
	}

	// (2) Spanning-forest shape: exactly V-K edges for V live sites, K
	// components. kruskalWeights has one entry per Kruskal edge.
	wantEdges := liveN - k
	if len(kruskalWeights) != wantEdges {
		return fmt.Errorf("MODULE BUG: Kruskal produced %d edges, want V-K = %d-%d = %d %s",
			len(kruskalWeights), liveN, k, wantEdges, repro)
	}
	if len(primEdges) != wantEdges {
		return fmt.Errorf("MODULE BUG: Prim produced %d edges, want V-K = %d-%d = %d %s",
			len(primEdges), liveN, k, wantEdges, repro)
	}

	// (3) Equal sorted multiset of edge weights — tie-safe (all MSTs share it).
	ps := append([]int64(nil), primWeights...)
	ks := append([]int64(nil), kruskalWeights...)
	sort.Slice(ps, func(i, j int) bool { return ps[i] < ps[j] })
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	for i := range ps {
		if ps[i] != ks[i] {
			return fmt.Errorf("MODULE BUG: Prim/Kruskal edge-weight multisets differ at index %d (%d vs %d) %s",
				i, ps[i], ks[i], repro)
		}
	}

	// (4) When every candidate weight is distinct the MST is unique, so the two
	// algorithms must select the identical set of undirected links.
	if allDistinct(ks) {
		if !sameEdgeSet(primEdges, kruskalPairs) {
			return fmt.Errorf("MODULE BUG: distinct-weight MST is unique, but Prim and Kruskal chose different links %s",
				repro)
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded generator
// ─────────────────────────────────────────────────────────────────────────────

// genResult reports the realised shape of a build (the random redundant links
// mean the exact link total is not known until the graph is materialised) plus
// the aggregate candidate-link cost and the wall-clock cost.
type genResult struct {
	links        int           // distinct undirected candidate links added
	allLinksCost int64         // summed cost of every candidate link
	elapsed      time.Duration // build wall-clock
}

// site is a 2-D geographic position, in metres.
type site struct{ x, y float64 }

// build materialises the site network described by cfg into a fresh undirected
// weighted adjlist, consuming the seeded RNG in a single fixed order so the
// shape is a pure function of cfg.seed. For each region it places the sites,
// wires a random spanning tree (guaranteeing the region is connected), then
// adds extraEdges redundant candidate links per site; finally, when requested,
// it chains consecutive regions with one inter-region link. Link cost is the
// Euclidean distance between the two sites, rounded to whole metres. The build
// honours ctx cancellation on a coarse interval.
func build(ctx context.Context, cfg config) (*adjlist.AdjList[int, int64], genResult, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed network for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	sites := make([]site, cfg.regions*cfg.sitesPerRegion)
	added := make(map[[2]int]struct{})
	var res genResult

	// addLink adds the undirected link u-v once (deduplicated on the canonical
	// node-value pair), weighted by the two sites' Euclidean separation.
	addLink := func(u, v int) error {
		key := [2]int{u, v}
		if u > v {
			key = [2]int{v, u}
		}
		if _, dup := added[key]; dup {
			return nil
		}
		w := cost(sites[u], sites[v])
		if err := a.AddEdge(u, v, w); err != nil {
			return fmt.Errorf("AddEdge %d-%d: %w", u, v, err)
		}
		added[key] = struct{}{}
		res.links++
		res.allLinksCost += w
		return nil
	}

	for r := 0; r < cfg.regions; r++ {
		if err := ctx.Err(); err != nil {
			return nil, genResult{}, err
		}
		base := r * cfg.sitesPerRegion
		originX := float64(r) * cfg.regionGap

		// Place this region's sites.
		for i := 0; i < cfg.sitesPerRegion; i++ {
			sites[base+i] = site{
				x: originX + rng.Float64()*cfg.span,
				y: rng.Float64() * cfg.span,
			}
		}
		// Random spanning tree: attach every later site to a random earlier one.
		for i := 1; i < cfg.sitesPerRegion; i++ {
			if err := addLink(base+i, base+rng.Intn(i)); err != nil {
				return nil, genResult{}, err
			}
		}
		// Redundant candidate links: extraEdges per site to random peers.
		for i := 0; i < cfg.sitesPerRegion; i++ {
			for e := 0; e < cfg.extraEdges; e++ {
				j := rng.Intn(cfg.sitesPerRegion)
				if j == i {
					continue // reject self-loop; a skipped draw keeps the shape stable
				}
				if err := addLink(base+i, base+j); err != nil {
					return nil, genResult{}, err
				}
			}
		}
	}

	// Inter-region backbone: chain region r's first site to region r+1's first
	// site, collapsing the regions into a single connected component.
	if cfg.interconnect {
		for r := 0; r+1 < cfg.regions; r++ {
			u := r * cfg.sitesPerRegion
			v := (r + 1) * cfg.sitesPerRegion
			if err := addLink(u, v); err != nil {
				return nil, genResult{}, err
			}
		}
	}

	res.elapsed = time.Since(start)
	return a, res, nil
}

// cost is the Euclidean distance between two sites, rounded to whole metres.
// Integer weights make the deterministic facts exact and reproducible across
// machines and keep the MST algorithms on their integer fast path (no NaN/Inf
// validation pass).
func cost(a, b site) int64 {
	dx := a.x - b.x
	dy := a.y - b.y
	return int64(math.Round(math.Sqrt(dx*dx + dy*dy)))
}

// ─────────────────────────────────────────────────────────────────────────────
// Small helpers
// ─────────────────────────────────────────────────────────────────────────────

// edgeWeight returns the weight of the arc u→v in the symmetric CSR, scanning
// u's adjacency. The generated graph is simple (no parallel links), so the
// first match is unambiguous.
func edgeWeight(c *csr.CSR[int64], u, v graph.NodeID) (int64, bool) {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	for k := verts[uint64(u)]; k < verts[uint64(u)+1]; k++ {
		if edges[k] == v {
			if weights != nil {
				return weights[k], true
			}
			return 0, true
		}
	}
	return 0, false
}

// canonicalPair orders an undirected link's endpoints as (min, max) so the two
// arc directions map to one identity.
func canonicalPair(a, b graph.NodeID) [2]uint64 {
	x, y := uint64(a), uint64(b)
	if x > y {
		x, y = y, x
	}
	return [2]uint64{x, y}
}

// extent returns the minimum and maximum of a non-empty int64 slice; (0, 0) for
// an empty one.
func extent(vs []int64) (lo, hi int64) {
	if len(vs) == 0 {
		return 0, 0
	}
	lo, hi = vs[0], vs[0]
	for _, v := range vs[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return lo, hi
}

// allDistinct reports whether a SORTED slice has no repeated value.
func allDistinct(sorted []int64) bool {
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return false
		}
	}
	return true
}

// sameEdgeSet reports whether two undirected-link multisets are equal.
func sameEdgeSet(a, b [][2]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[[2]uint64]int, len(a))
	for _, e := range a {
		m[e]++
	}
	for _, e := range b {
		m[e]--
		if m[e] < 0 {
			return false
		}
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc reflects
// live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
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
