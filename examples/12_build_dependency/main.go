// Example 12_build_dependency — model a software build-dependency graph,
// derive a valid build order with search.TopologicalSort (Kahn's
// algorithm), and detect a circular dependency with search.TarjanSCC.
//
// It generates a realistic, seeded, scale-parametrised build graph: a
// layered module DAG in which every module depends on a few modules in
// strictly lower layers. That layering is what guarantees acyclicity —
// along any dependency path the layer index strictly decreases, so no
// directed cycle can form and TopologicalSort always succeeds. The
// example then exercises the two algorithms it teaches and reports both
// the deterministic shape of the data and the volatile telemetry —
// wall-clock and live heap — that make it a benchmark rather than a
// demonstration.
//
// # Model
//
//	module mNNNN                              // one node per module
//	(a)-[depends-on]->(b)                     // edge a->b: a depends on b
//
// Edge direction reads "a depends on b", so b must be built before a.
// Modules are partitioned into L layers; a module in layer k draws a few
// distinct dependencies from layers strictly below it ([0, k)). Layer 0
// modules are leaves (no dependencies — the foundation libraries). The
// layer widths follow a pyramid (widest at the leaves), apportioned with
// a floor pass so every layer is non-empty and the counts sum exactly to
// the requested module total. A single dependency chain is planted from
// the top layer down to layer 0 so the graph always has a known longest
// chain and a known forward path to close into a cycle.
//
// # Two stages
//
//  1. Build order. The DAG is frozen into a CSR snapshot and sorted with
//     [search.TopologicalSort]. The result is verified in-code against the
//     validity invariant — for every edge u->v the source precedes the
//     destination in the order — and the build order (dependencies first)
//     is the reverse of that linear extension. The longest dependency
//     chain (the build's critical-path depth) is computed by a linear-time
//     DP over the topological order.
//
//  2. Cycle detection. A back-edge from the bottom of the planted chain to
//     its top is injected, closing a circular dependency. [search.TopologicalSort]
//     then fails with [search.ErrCycle], and [search.TarjanSCC] reports the
//     single strongly connected component that contains the cycle. Because
//     the input was a DAG (whose every SCC is a singleton), the injected
//     back-edge produces exactly one component of size greater than one,
//     and that component contains both endpoints of the back-edge.
//
// # Scale
//
// Run with no flags, the example builds a small deterministic default
// (a few thousand modules) that the regression test pins. Every dimension
// is a flag, so the same binary scales up to where the algorithms' cost
// is observable:
//
//	go run ./examples/12_build_dependency -modules 1000000 -layers 40 -seed 7
//
// The deterministic data shape is reproducible for a fixed -seed; only
// the telemetry (lines prefixed with "# ") varies between runs and
// machines.
//
// # Why CSR
//
// TopologicalSort and TarjanSCC are read-only analytics, so the graph is
// built once in a mutable [adjlist.AdjList] and then frozen into an
// immutable [csr.CSR] snapshot — the lock-free, cache-friendly surface the
// search package runs against. This is the canonical build -> snapshot ->
// query flow; persistence is orthogonal to what this example measures and
// is demonstrated by examples 04, 17, 24 and 25.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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

// config captures every scale and shape knob of the build-dependency
// generator. The zero value is not valid; build one with defaultConfig
// and override fields from flags (see main) or construct one directly
// (see the regression test).
type config struct {
	modules     int     // number of module nodes
	layers      int     // number of dependency layers (>= 2)
	depsMin     int     // minimum dependencies a non-leaf module requests
	depsMax     int     // maximum dependencies a non-leaf module requests
	pyramidBase float64 // layer-width growth toward the leaves (>= 1)
	seed        int64   // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns a small, deterministic build graph that the
// regression test pins. It is large enough that the DAG statistics are
// non-trivial yet builds and analyses well under the short-layer 60 s
// package budget.
func defaultConfig() config {
	return config{
		modules:     5000,
		layers:      12,
		depsMin:     1,
		depsMax:     5,
		pyramidBase: 1.6,
		seed:        1,
	}
}

// validate rejects a configuration that cannot produce a well-formed
// layered DAG. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.layers < 2:
		return fmt.Errorf("layers must be >= 2, got %d", c.layers)
	case c.modules < c.layers:
		return fmt.Errorf("modules (%d) must be >= layers (%d): every layer must hold at least one module", c.modules, c.layers)
	case c.depsMin < 1:
		return fmt.Errorf("depsMin must be >= 1, got %d", c.depsMin)
	case c.depsMax < c.depsMin:
		return fmt.Errorf("require depsMin <= depsMax, got [%d,%d]", c.depsMin, c.depsMax)
	case c.pyramidBase < 1:
		return fmt.Errorf("pyramidBase must be >= 1, got %g", c.pyramidBase)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.modules, "modules", cfg.modules, "number of module nodes")
	flag.IntVar(&cfg.layers, "layers", cfg.layers, "number of dependency layers (>= 2)")
	flag.IntVar(&cfg.depsMin, "deps-min", cfg.depsMin, "minimum dependencies a non-leaf module requests")
	flag.IntVar(&cfg.depsMax, "deps-max", cfg.depsMax, "maximum dependencies a non-leaf module requests")
	flag.Float64Var(&cfg.pyramidBase, "pyramid-base", cfg.pyramidBase, "layer-width growth toward the leaves (>= 1)")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the build-dependency DAG described by cfg, derives a
// build order and verifies it, then injects a circular dependency and
// detects it, writing a report to w. Bare lines carry deterministic
// facts (counts, invariants, results — reproducible for a fixed seed);
// lines prefixed with "# " carry volatile telemetry (durations and heap
// figures) that vary per run and per machine. All output goes to w so a
// test can capture and assert on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.modules=%d\n", cfg.modules)
	fmt.Fprintf(w, "config.layers=%d\n", cfg.layers)
	fmt.Fprintf(w, "config.deps=[%d,%d]\n", cfg.depsMin, cfg.depsMax)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	dag, err := generate(ctx, cfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	fmt.Fprintf(w, "nodes.modules=%d\n", dag.modules)
	fmt.Fprintf(w, "edges.dependencies=%d\n", dag.edges)
	fmt.Fprintf(w, "dag.layers=%d\n", cfg.layers)

	built := readMem()
	fmt.Fprintf(w, "# build.elapsed=%s\n", dag.elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# build.node_rate=%.0f nodes/s\n", rate(dag.modules, dag.elapsed))
	fmt.Fprintf(w, "# build.edge_rate=%.0f edges/s\n", rate(dag.edges, dag.elapsed))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))

	if err := buildOrder(ctx, w, dag); err != nil {
		return fmt.Errorf("build order: %w", err)
	}
	return detectCycle(ctx, w, dag)
}

// buildOrder topologically sorts the acyclic dependency graph, verifies
// the result against the validity invariant, and reports the build's
// longest dependency chain. The first module of the build order — the
// reverse of the topological order — is the deepest leaf, the last is a
// top-layer application module.
func buildOrder(ctx context.Context, w io.Writer, dag *buildDAG) error {
	start := time.Now()
	order, err := search.TopologicalSortCtx(ctx, dag.c)
	if err != nil {
		return fmt.Errorf("TopologicalSort: %w", err)
	}
	elapsed := time.Since(start)

	// Validity invariant: TopologicalSort returns a linear extension in
	// which every edge points forward, i.e. for every edge u->v the source
	// u precedes the destination v. With the convention "u depends on v",
	// the depender precedes its dependency in this order, so the physical
	// build order (dependencies first) is the reverse.
	valid, err := topoOrderValid(dag.c, order)
	if err != nil {
		return fmt.Errorf("verify topological order: %w", err)
	}

	fmt.Fprintf(w, "topo.order_valid=%t\n", valid)
	fmt.Fprintf(w, "topo.modules_ordered=%d\n", len(order))
	fmt.Fprintf(w, "dag.longest_chain=%d\n", longestChain(dag.c, order))
	fmt.Fprintf(w, "# topo.elapsed=%s\n", elapsed.Round(time.Microsecond))
	return nil
}

// detectCycle injects a back-edge that closes the planted dependency
// chain into a circular dependency, confirms TopologicalSort now rejects
// the graph with ErrCycle, and reports the single strongly connected
// component that Tarjan's algorithm finds for the cycle. The DAG had no
// non-trivial SCC (every SCC of an acyclic graph is a singleton), so the
// one back-edge produces exactly one component of size > 1, containing
// both endpoints of the back-edge.
func detectCycle(ctx context.Context, w io.Writer, dag *buildDAG) error {
	cyclic, err := dag.withBackEdge()
	if err != nil {
		return fmt.Errorf("inject cycle: %w", err)
	}

	if _, err := search.TopologicalSortCtx(ctx, cyclic); !errors.Is(err, search.ErrCycle) {
		return fmt.Errorf("expected ErrCycle after injecting a back-edge, got %w", err)
	}

	start := time.Now()
	comps, err := search.TarjanSCCCtx(ctx, cyclic)
	if err != nil {
		return fmt.Errorf("TarjanSCC: %w", err)
	}
	elapsed := time.Since(start)

	// Exactly one non-trivial SCC is expected, and it must contain both
	// endpoints of the injected back-edge (the top and bottom of the
	// planted chain). Its exact size depends on how many DAG paths the
	// cycle absorbs, so it is reported empirically rather than predicted —
	// it is reproducible for a fixed seed.
	var cycles, cycleSize int
	containsEndpoints := false
	for _, comp := range comps {
		if len(comp) <= 1 {
			continue
		}
		cycles++
		cycleSize = len(comp)
		containsEndpoints = slices.Contains(comp, dag.cycleTop) && slices.Contains(comp, dag.cycleBottom)
	}

	fmt.Fprintf(w, "cycle.detected=%t\n", cycles == 1 && containsEndpoints)
	fmt.Fprintf(w, "cycle.scc_count=%d\n", cycles)
	fmt.Fprintf(w, "cycle.scc_size=%d\n", cycleSize)
	fmt.Fprintf(w, "# tarjan.elapsed=%s\n", elapsed.Round(time.Microsecond))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Generator
// ─────────────────────────────────────────────────────────────────────────────

// buildDAG is a materialised layered build-dependency graph: the frozen
// CSR snapshot the algorithms run against, plus the realised counts and
// the two endpoints of the planted chain that the cycle stage closes
// into a circular dependency.
type buildDAG struct {
	a           *adjlist.AdjList[string, struct{}]
	c           *csr.CSR[struct{}]
	modules     int
	edges       int
	cycleTop    graph.NodeID  // top-layer end of the planted chain (the depender)
	cycleBottom graph.NodeID  // layer-0 end of the planted chain (the leaf)
	elapsed     time.Duration // wall-clock to build and freeze the graph
}

// moduleName returns the stable, zero-padded name of module index i
// (m0000, m0001, …). The name fixes nothing about the topology — the
// shape is fixed by the seed — but keeps the node identifiers readable.
func moduleName(i int) string {
	return fmt.Sprintf("m%05d", i)
}

// generate builds the layered DAG described by cfg into a fresh adjacency
// list and freezes it into a CSR snapshot. Modules are partitioned into
// cfg.layers layers by a deterministic pyramid apportionment; each
// non-leaf module draws a few distinct dependencies from strictly lower
// layers; and a single chain is planted from the top layer down to layer
// 0 so the graph has a known longest chain and a known forward path to
// close into a cycle. The build honours ctx cancellation on a periodic
// check.
func generate(ctx context.Context, cfg config) (*buildDAG, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dependency graph for a given -seed; crypto/rand
	// would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	counts := apportion(cfg.modules, cfg.layers, cfg.pyramidBase)

	// layerOf[i] is the layer of module i; firstOfLayer[k] is the index of
	// the first module in layer k. Module indices are laid out layer by
	// layer (all of layer 0, then layer 1, …), so the slice both names the
	// nodes and records the strict layering the acyclicity proof rests on.
	layerOf := make([]int, cfg.modules)
	firstOfLayer := make([]int, cfg.layers+1)
	idx := 0
	for k := 0; k < cfg.layers; k++ {
		firstOfLayer[k] = idx
		for j := 0; j < counts[k]; j++ {
			layerOf[idx] = k
			idx++
		}
	}
	firstOfLayer[cfg.layers] = idx

	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})

	// Intern every module up front so dependency targets always exist
	// before an edge references them, and so isolated leaves still appear.
	for i := 0; i < cfg.modules; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if err := a.AddNode(moduleName(i)); err != nil {
			return nil, fmt.Errorf("AddNode %s: %w", moduleName(i), err)
		}
	}

	edges := 0
	seen := make(map[int]struct{}, cfg.depsMax) // reused per module, cleared each time
	chosen := make([]int, 0, cfg.depsMax)       // reused per module
	for i := 0; i < cfg.modules; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		k := layerOf[i]
		if k == 0 {
			continue // layer-0 modules are leaves: no dependencies
		}
		// Candidates are every module in a strictly lower layer [0, avail).
		avail := firstOfLayer[k]
		degree := cfg.depsMin + rng.Intn(cfg.depsMax-cfg.depsMin+1)
		if degree > avail {
			degree = avail
		}
		// Draw distinct targets by rejection sampling: degree is small
		// (<= depsMax) and avail is large, so collisions are rare and this
		// is O(degree) per module — far cheaper than materialising the whole
		// candidate pool. The chosen targets are sorted before insertion so
		// the adjacency is reproducible regardless of map iteration order.
		clear(seen)
		chosen = chosen[:0]
		for len(chosen) < degree {
			t := rng.Intn(avail)
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			chosen = append(chosen, t)
		}
		slices.Sort(chosen)
		for _, t := range chosen {
			if err := a.AddEdge(moduleName(i), moduleName(t), struct{}{}); err != nil {
				return nil, fmt.Errorf("AddEdge %s->%s: %w", moduleName(i), moduleName(t), err)
			}
			edges++
		}
	}

	// Plant a single chain top-layer -> ... -> layer-0 so the graph always
	// has a deep longest chain and a guaranteed forward path from cycleTop
	// down to cycleBottom that the cycle stage closes with one back-edge.
	// One representative module per layer (the first of each) is linked
	// downward; duplicate links are skipped so a re-linked edge is not
	// double-counted.
	prev := firstOfLayer[cfg.layers-1]
	for k := cfg.layers - 2; k >= 0; k-- {
		cur := firstOfLayer[k]
		if !a.HasEdge(moduleName(prev), moduleName(cur)) {
			if err := a.AddEdge(moduleName(prev), moduleName(cur), struct{}{}); err != nil {
				return nil, fmt.Errorf("AddEdge %s->%s: %w", moduleName(prev), moduleName(cur), err)
			}
			edges++
		}
		prev = cur
	}

	c := csr.BuildFromAdjList(a)

	mapper := a.Mapper()
	top, ok := mapper.Lookup(moduleName(firstOfLayer[cfg.layers-1]))
	if !ok {
		return nil, fmt.Errorf("planted chain top %s missing from mapper", moduleName(firstOfLayer[cfg.layers-1]))
	}
	bottom, ok := mapper.Lookup(moduleName(firstOfLayer[0]))
	if !ok {
		return nil, fmt.Errorf("planted chain bottom %s missing from mapper", moduleName(firstOfLayer[0]))
	}

	return &buildDAG{
		a:           a,
		c:           c,
		modules:     cfg.modules,
		edges:       edges,
		cycleTop:    top,
		cycleBottom: bottom,
		elapsed:     time.Since(start),
	}, nil
}

// withBackEdge returns a fresh CSR snapshot of the DAG with one extra
// edge — from the bottom of the planted chain back to its top — which
// closes a circular dependency. The original DAG is left untouched so
// the build-order stage and the cycle stage observe independent graphs.
func (d *buildDAG) withBackEdge() (*csr.CSR[struct{}], error) {
	top, ok := d.a.Mapper().Resolve(d.cycleTop)
	if !ok {
		return nil, fmt.Errorf("unresolved cycle top id %d", d.cycleTop)
	}
	bottom, ok := d.a.Mapper().Resolve(d.cycleBottom)
	if !ok {
		return nil, fmt.Errorf("unresolved cycle bottom id %d", d.cycleBottom)
	}
	if err := d.a.AddEdge(bottom, top, struct{}{}); err != nil {
		return nil, fmt.Errorf("AddEdge %s->%s (back-edge): %w", bottom, top, err)
	}
	cyclic := csr.BuildFromAdjList(d.a)
	// Remove the back-edge again so the receiver's adjacency stays acyclic
	// for any later use; the build-order stage ran against d.c (a snapshot
	// taken before this method), so it is unaffected regardless.
	d.a.RemoveEdge(bottom, top)
	return cyclic, nil
}

// checkEvery bounds how often generation polls ctx for cancellation:
// often enough that a cancelled large run stops promptly, rare enough
// that the check is free relative to the surrounding work.
const checkEvery = 4096

// apportion distributes total modules across layers count layers using a
// pyramid weight (widest at layer 0, the leaves) and largest-remainder
// (Hamilton) apportionment, guaranteeing every layer holds at least one
// module and the counts sum exactly to total. The result is a pure
// function of its arguments, so the layering is reproducible.
func apportion(total, layers int, base float64) []int {
	counts := make([]int, layers)
	// Floor pass: one module per layer first, so none is ever empty.
	for k := range counts {
		counts[k] = 1
	}
	remaining := total - layers
	if remaining <= 0 {
		return counts
	}

	// Pyramid weights: layer 0 (leaves) widest, top layer narrowest.
	weights := make([]float64, layers)
	var sum float64
	for k := range weights {
		weights[k] = pow(base, float64(layers-1-k))
		sum += weights[k]
	}

	// Ideal real share, integer floor, and fractional remainder per layer.
	type share struct {
		layer int
		frac  float64
	}
	allotted := 0
	shares := make([]share, layers)
	for k := range weights {
		ideal := float64(remaining) * weights[k] / sum
		base := int(ideal)
		counts[k] += base
		allotted += base
		shares[k] = share{layer: k, frac: ideal - float64(base)}
	}

	// Distribute the leftover units to the largest fractional remainders;
	// ties break by ascending layer index for determinism.
	leftover := remaining - allotted
	slices.SortStableFunc(shares, func(x, y share) int {
		if x.frac > y.frac {
			return -1
		}
		if x.frac < y.frac {
			return 1
		}
		return x.layer - y.layer
	})
	for i := 0; i < leftover; i++ {
		counts[shares[i].layer]++
	}
	return counts
}

// pow returns base**exp for a non-negative integer exponent. It avoids
// math.Pow's floating-point drift so the apportionment is bit-stable
// across platforms for the small exponents layer counts produce.
func pow(base, exp float64) float64 {
	r := 1.0
	for i := 0; i < int(exp); i++ {
		r *= base
	}
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Invariants and statistics
// ─────────────────────────────────────────────────────────────────────────────

// topoOrderValid verifies the topological-order validity invariant: in
// the order returned by TopologicalSort, every edge u->v has the source u
// before the destination v. It returns false on the first violating edge.
func topoOrderValid(c *csr.CSR[struct{}], order []graph.NodeID) (bool, error) {
	// pos[id] is the 0-based position of id in order; -1 means absent
	// (a NodeID never assigned an edge, which TopologicalSort omits).
	pos := make([]int, c.MaxNodeID()+1)
	for i := range pos {
		pos[i] = -1
	}
	for i, id := range order {
		pos[id] = i
	}
	for _, u := range order {
		for v := range c.NeighboursByID(u) {
			if pos[v] < 0 {
				return false, fmt.Errorf("edge %d->%d targets a node absent from the order", u, v)
			}
			if pos[u] >= pos[v] {
				return false, nil
			}
		}
	}
	return true, nil
}

// longestChain returns the number of modules on the longest dependency
// chain — the build's critical-path depth in module count. It is the
// longest path in the DAG (by edge count) plus one, computed by a
// linear-time DP that relaxes each edge in topological order so every
// predecessor of a node is settled before the node is processed. Longest
// path is O(V+E) only because the graph is acyclic.
func longestChain(c *csr.CSR[struct{}], order []graph.NodeID) int {
	depth := make([]int, c.MaxNodeID()+1)
	best := 0
	for _, u := range order {
		du := depth[u]
		for v := range c.NeighboursByID(u) {
			if du+1 > depth[v] {
				depth[v] = du + 1
			}
		}
		if du > best {
			best = du
		}
	}
	return best + 1 // edges on the path + 1 = modules on the chain
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

// rate returns count/elapsed in units per second, or 0 for a
// zero-length interval.
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
