// Example 32_euler — Eulerian circuits over a route-inspection network, using
// Hierholzer's algorithm on both an undirected and a directed graph.
//
// The scenario is route inspection (the "Chinese postman" setting): a fleet
// must traverse every street of a network exactly once and return to the
// depot. Such a tour exists precisely when the network has an Eulerian
// circuit — every intersection has even degree (undirected), or equal in- and
// out-degree (directed, for one-way streets). The example builds a network
// that satisfies those conditions BY CONSTRUCTION, finds the circuit with
// search.Hierholzer / search.HierholzerUndirected, and verifies the tour uses
// every street exactly once. A -broken flag deletes one street to show the
// module correctly reports search.ErrNoEulerian when no such tour exists.
//
// # Topology — why it is always Eulerian
//
// The network is assembled from edge-disjoint CYCLES (patrol loops). A base
// ring through a seeded random permutation of all N intersections guarantees
// the network is connected and every node starts at even degree. Each extra
// loop is a simple cycle over a random subset of intersections, added only if
// all its streets are new (edge-disjoint). Adding a cycle raises the degree of
// each node it touches by exactly two (undirected) — or its in- and out-degree
// by one each (directed) — so the Eulerian precondition is preserved no matter
// how many loops are layered on. The result is a realistic, seeded, non-trivial
// network (streets far outnumber intersections) whose Euler tour genuinely
// exercises Hierholzer's loop-stitching rather than walking a single cycle.
//
// # Scale
//
// The small deterministic default (200 intersections, 40 extra loops) runs in
// milliseconds and is pinned by the regression test. Scale it up to make the
// O(E) tour construction observable:
//
//	go run ./examples/32_euler -nodes 200000 -loops 40000 -seed 7
//
// Only the telemetry (lines prefixed with "# ") varies between runs; the
// deterministic facts are reproducible for a fixed -seed.
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
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// config captures every scale and shape knob. The zero value is not valid;
// build one with defaultConfig and override fields from flags.
type config struct {
	nodes   int   // number of intersections
	loops   int   // extra edge-disjoint patrol loops layered on the base ring
	loopMin int   // minimum loop length (intersections), >= 3
	loopMax int   // maximum loop length (intersections)
	broken  bool  // delete one street to force ErrNoEulerian
	seed    int64 // RNG seed; fixes the network shape
}

// defaultConfig returns the small deterministic default the regression test
// pins: 200 intersections and 40 extra loops of 3..8 intersections each.
func defaultConfig() config {
	return config{
		nodes:   200,
		loops:   40,
		loopMin: 3,
		loopMax: 8,
		broken:  false,
		seed:    1,
	}
}

// validate rejects a configuration that cannot produce the requested shape.
func (c config) validate() error {
	switch {
	case c.nodes < 4:
		return fmt.Errorf("nodes must be >= 4, got %d", c.nodes)
	case c.loops < 0:
		return fmt.Errorf("loops must be >= 0, got %d", c.loops)
	case c.loopMin < 3:
		return fmt.Errorf("loop-min must be >= 3, got %d", c.loopMin)
	case c.loopMax < c.loopMin:
		return fmt.Errorf("loop-max (%d) must be >= loop-min (%d)", c.loopMax, c.loopMin)
	case c.loopMax > c.nodes:
		return fmt.Errorf("loop-max (%d) must be <= nodes (%d)", c.loopMax, c.nodes)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of intersections")
	flag.IntVar(&cfg.loops, "loops", cfg.loops, "extra edge-disjoint patrol loops")
	flag.IntVar(&cfg.loopMin, "loop-min", cfg.loopMin, "minimum loop length (>= 3)")
	flag.IntVar(&cfg.loopMax, "loop-max", cfg.loopMax, "maximum loop length")
	flag.BoolVar(&cfg.broken, "broken", cfg.broken, "delete one street to force ErrNoEulerian")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the network shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the route network, finds Eulerian circuits over its undirected
// and directed forms, verifies the tours, and writes a report to w. Bare lines
// carry deterministic facts; "# "-prefixed lines carry volatile telemetry.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.loops=%d\n", cfg.loops)
	fmt.Fprintf(w, "config.broken=%t\n", cfg.broken)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()
	cycles := generateCycles(ctx, cfg)

	if err := reportEuler(ctx, w, cfg, cycles, false); err != nil {
		return err
	}
	if err := reportEuler(ctx, w, cfg, cycles, true); err != nil {
		return err
	}

	m := readMem()
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(m.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(m.HeapAlloc-base.HeapAlloc))
	return nil
}

// reportEuler builds one CSR (undirected or directed) from the shared cycle
// set, runs the matching Hierholzer variant, and either verifies the returned
// tour (uses every street exactly once, starts and ends at the depot) or — when
// cfg.broken deleted a street — asserts ErrNoEulerian. Facts are prefixed with
// the graph kind so the two passes never collide.
func reportEuler(ctx context.Context, w io.Writer, cfg config, cycles [][]int, directed bool) error {
	kind := "undirected"
	if directed {
		kind = "directed"
	}
	a, edges := buildGraph(cfg, cycles, directed)
	c := csr.BuildFromAdjList(a)

	fmt.Fprintf(w, "%s.streets=%d\n", kind, edges)

	start := time.Now()
	var (
		trail []graph.NodeID
		err   error
	)
	if directed {
		trail, err = search.HierholzerCtx(ctx, c)
	} else {
		trail, err = search.HierholzerUndirectedCtx(ctx, c)
	}
	elapsed := time.Since(start)

	if cfg.broken {
		// One street was deleted, breaking the degree/balance precondition, so
		// no Euler tour can exist: the module must report ErrNoEulerian.
		fmt.Fprintf(w, "%s.no_eulerian=%t\n", kind, errors.Is(err, search.ErrNoEulerian))
		if err != nil && !errors.Is(err, search.ErrNoEulerian) {
			return fmt.Errorf("%s hierholzer (broken): unexpected error %w", kind, err)
		}
		if err == nil {
			return fmt.Errorf("%s: expected ErrNoEulerian after deleting a street, got a tour of %d nodes (a module correctness bug)", kind, len(trail))
		}
		fmt.Fprintf(w, "# %s.elapsed=%s\n", kind, elapsed.Round(time.Microsecond))
		return nil
	}

	if err != nil {
		return fmt.Errorf("%s hierholzer: %w", kind, err)
	}
	// A valid Euler tour visits E+1 nodes (E streets, each traversed once) and,
	// on this all-even / balanced network, is a circuit (start == end).
	usesEachOnce := verifyTrail(c, trail, directed)
	fmt.Fprintf(w, "%s.trail_len=%d\n", kind, len(trail))
	fmt.Fprintf(w, "%s.trail_len_is_streets_plus_1=%t\n", kind, len(trail) == edges+1)
	fmt.Fprintf(w, "%s.each_street_once=%t\n", kind, usesEachOnce)
	fmt.Fprintf(w, "%s.is_circuit=%t\n", kind, len(trail) > 0 && trail[0] == trail[len(trail)-1])
	fmt.Fprintf(w, "# %s.elapsed=%s\n", kind, elapsed.Round(time.Microsecond))
	return nil
}

// verifyTrail checks that the trail traverses every edge of c exactly once: it
// counts each consecutive (trail[i], trail[i+1]) step and confirms the multiset
// of steps equals the graph's edge multiset. For the undirected case each step
// is canonicalised to an unordered pair and the symmetric CSR's edge count is
// halved.
func verifyTrail(c *csr.CSR[struct{}], trail []graph.NodeID, directed bool) bool {
	if len(trail) < 2 {
		return c.Size() == 0
	}
	type pair struct{ u, v graph.NodeID }
	seen := make(map[pair]int, len(trail))
	for i := 0; i+1 < len(trail); i++ {
		u, v := trail[i], trail[i+1]
		if !directed && u > v {
			u, v = v, u
		}
		seen[pair{u, v}]++
	}
	// No edge may be used more than once.
	for _, n := range seen {
		if n != 1 {
			return false
		}
	}
	// The number of distinct steps must equal the number of edges (directed) or
	// distinct undirected edges (symmetric CSR stores each twice).
	wantEdges := int(c.Size()) //nolint:gosec // G115: a CSR edge count fits comfortably in int
	if !directed {
		wantEdges /= 2
	}
	return len(seen) == wantEdges && len(trail)-1 == wantEdges
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded generator
// ─────────────────────────────────────────────────────────────────────────────

// generateCycles produces the edge-disjoint cycles (as slices of node ids) that
// define the network: a base ring through a random permutation of all nodes,
// then cfg.loops extra simple cycles over random subsets, each accepted only if
// every one of its streets is new. The cycle set is a pure function of the seed
// and is shared by the undirected and directed builds so the two graphs have
// the same street layout.
func generateCycles(ctx context.Context, cfg config) [][]int {
	//nolint:gosec // G404: a seeded math/rand is intentional; the example must
	// reproduce a fixed network for a given -seed, which crypto/rand would defeat.
	rng := rand.New(rand.NewSource(cfg.seed))

	// Base ring: a random permutation of all nodes, joined into one cycle.
	perm := rng.Perm(cfg.nodes)
	cycles := make([][]int, 0, cfg.loops+1)
	cycles = append(cycles, perm)

	type upair struct{ a, b int }
	used := make(map[upair]struct{}, cfg.nodes)
	mark := func(cyc []int) {
		for i := range cyc {
			a, b := cyc[i], cyc[(i+1)%len(cyc)]
			if a > b {
				a, b = b, a
			}
			used[upair{a, b}] = struct{}{}
		}
	}
	disjoint := func(cyc []int) bool {
		for i := range cyc {
			a, b := cyc[i], cyc[(i+1)%len(cyc)]
			if a > b {
				a, b = b, a
			}
			if _, ok := used[upair{a, b}]; ok {
				return false
			}
		}
		return true
	}
	mark(perm)

	for added, attempts := 0, 0; added < cfg.loops && attempts < cfg.loops*32; attempts++ {
		if attempts%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return cycles
			}
		}
		l := cfg.loopMin + rng.Intn(cfg.loopMax-cfg.loopMin+1)
		cyc := rng.Perm(cfg.nodes)[:l]
		if !distinctAdjacent(cyc) || !disjoint(cyc) {
			continue
		}
		mark(cyc)
		cycles = append(cycles, cyc)
		added++
	}
	return cycles
}

// distinctAdjacent reports whether a cycle has no repeated consecutive node
// (including the wrap-around), which would otherwise create a self-loop street.
func distinctAdjacent(cyc []int) bool {
	if len(cyc) < 3 {
		return false
	}
	for i := range cyc {
		if cyc[i] == cyc[(i+1)%len(cyc)] {
			return false
		}
	}
	return true
}

// buildGraph materialises the cycle set into an adjlist and returns the graph
// plus the number of distinct streets. When directed, each cycle is oriented
// one way (i -> i+1), so every node's in-degree equals its out-degree; when
// undirected, adjlist mirrors each street. When cfg.broken is set, TWO
// vertex-disjoint streets of the base ring are omitted: closing a single
// street would only turn the circuit into an Eulerian PATH (two odd-degree
// vertices — a route that no longer returns to the depot), which Hierholzer
// still finds; closing two disjoint streets leaves four odd-degree vertices
// (directed: two surplus sources and two surplus sinks), so no Eulerian trail
// exists at all and the module must report ErrNoEulerian.
func buildGraph(cfg config, cycles [][]int, directed bool) (*adjlist.AdjList[int, struct{}], int) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: directed})
	edges := 0
	for ci, cyc := range cycles {
		for i := range cyc {
			u, v := cyc[i], cyc[(i+1)%len(cyc)]
			// The base ring's edges 0 (perm[0]-perm[1]) and 2 (perm[2]-perm[3])
			// are vertex-disjoint because perm is a permutation; omitting both
			// makes exactly four vertices odd-degree.
			if cfg.broken && ci == 0 && (i == 0 || i == 2) {
				continue
			}
			_ = a.AddEdge(u, v, struct{}{})
			edges++
		}
	}
	return a, edges
}

// checkEvery bounds how often the generator polls ctx for cancellation.
const checkEvery = 4096

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
