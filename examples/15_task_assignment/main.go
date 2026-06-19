// Example 15_task_assignment — two bipartite assignment algorithms side
// by side over one seeded, scale-parametrised worker/task instance:
// [search.Hungarian] computes the globally cheapest one-to-one assignment
// over the full cost matrix, and [search.HopcroftKarp] computes the
// largest matching once a feasibility rule prunes the edges.
//
// The two algorithms answer different questions over the same instance.
// Hungarian minimises total cost across the full n×m cost matrix and may
// legally use any pair, however unsuitable. Hopcroft-Karp ignores cost
// and instead maximises how many workers can be staffed once each worker
// is restricted to the tasks they are competent for — the "feasible"
// pairs whose cost falls at or below a percentile threshold. Reporting
// both, and relating them, is the point of the example.
//
// # Model
//
// The instance is a skilled-workforce / task-dispatch problem with a
// latent competency model. Each worker carries a hidden skill vector and
// each task a hidden requirement vector, both in [0,1]^skills drawn from
// the seeded RNG. The cost of worker i taking task j is a weighted
// under-qualification deficit — a worker pays only for the skills a task
// needs and they lack, never for surplus skill — plus a little
// idiosyncratic noise, scaled and rounded to a clean integer:
//
//	cost(i,j) = round( SCALE · ( Σ_k w_k·max(0, req_j[k] − skill_i[k]) + ε ) )
//
// Costs are integers held as float64, so the optimal total cost is an
// exact integer the regression test can assert. The structure (low-rank
// affinity plus noise) makes the optimal assignment non-trivial: the
// per-worker cheapest task frequently collides, so resolving the
// collisions optimally needs the global Hungarian trade-off rather than a
// greedy pick.
//
// # Feasibility pruning
//
// The Hopcroft-Karp graph keeps only the pairs a worker is competent for:
// edge (i,j) survives when cost(i,j) is at or below the feasiblePct-th
// percentile of all n·m costs. A percentile (rather than an absolute
// constant) is self-calibrating — it stays in the interesting regime as
// the scale, skill count, or noise change. At the default ~30% retention
// the cheap edges cluster on the easy tasks, so by Hall's theorem the
// maximum matching falls strictly short of min(workers, tasks): some
// workers cannot be staffed at all under the rule. That shortfall, and
// how the willing roster's cost compares to the unconstrained optimum, is
// what the example surfaces.
//
// # Scale
//
// Run with no flags the example uses a small, fast, deterministic default
// (200 workers, 240 tasks, 6 skills). Every dimension is a flag, so the
// same binary scales up to where the O(V^3) Hungarian cost and the
// matching structure become observable:
//
//	go run ./examples/15_task_assignment -workers 1000 -tasks 1200 -seed 7
//
// Hungarian requires at least as many tasks as workers (workers ≤ tasks);
// validate rejects a configuration that violates it. The deterministic
// facts — the optimal total cost and the maximum matching size — are
// reproducible for a fixed -seed; only the telemetry (lines prefixed with
// "# ") varies between runs and machines.
//
// # Why in-memory
//
// The example measures assignment-algorithm wall-clock and live-heap
// footprint, so it builds the instance in memory and never touches the
// WAL/recovery stack. The persistence path is demonstrated by examples
// 04, 17, 24 and 25.
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

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// costScale converts the real-valued skill-mismatch into clean integers.
// With the default skill count and weight range the mismatch lands in
// roughly [0, 9], so a scale of 1000 puts costs in roughly [0, 12000] as
// exact integers (well under 2^53, so the float64 holding them — and any
// sum of them — is exact and safe to assert with ==).
const costScale = 1000.0

// config captures every scale and shape knob of the instance. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test). Costs and the feasibility edge set are a deterministic function
// of every field below plus the fixed generator arithmetic, so a fixed
// config yields byte-identical facts.
type config struct {
	workers     int     // number of worker rows (the left partition)
	tasks       int     // number of task columns (the right partition)
	skills      int     // latent skill/requirement vector dimensionality
	noiseFrac   float64 // idiosyncratic-noise magnitude as a fraction of a skill deficit
	feasiblePct float64 // percentile of costs kept as feasible pairs, in (0,1]
	seed        int64   // RNG seed; fixes the deterministic instance shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins: 200 workers onto 240 tasks over a 6-dimensional skill space,
// keeping the cheapest 15% of pairs feasible. At that retention the
// feasible edges cluster enough that the maximum matching falls strictly
// short of the 200-worker ceiling — the binding regime the example is
// meant to show — while staying clear of the near-empty extreme. O(V^3)
// Hungarian on V=240 completes in well under a second, so `go test`
// stays comfortably under the short-layer 60 s package budget.
func defaultConfig() config {
	return config{
		workers:     200,
		tasks:       240,
		skills:      6,
		noiseFrac:   0.3,
		feasiblePct: 0.15,
		seed:        1,
	}
}

// validate rejects a configuration that cannot produce a well-defined
// instance — chiefly the Hungarian precondition that there be at least as
// many tasks as workers (the rectangular Kuhn-Munkres formulation
// requires columns ≥ rows). It is checked once, at the boundary, before
// any work.
func (c config) validate() error {
	switch {
	case c.workers <= 0:
		return fmt.Errorf("workers must be > 0, got %d", c.workers)
	case c.tasks <= 0:
		return fmt.Errorf("tasks must be > 0, got %d", c.tasks)
	case c.workers > c.tasks:
		return fmt.Errorf("workers (%d) exceeds tasks (%d): Hungarian requires tasks >= workers", c.workers, c.tasks)
	case c.skills <= 0:
		return fmt.Errorf("skills must be > 0, got %d", c.skills)
	case c.noiseFrac < 0:
		return fmt.Errorf("noise-frac must be >= 0, got %g", c.noiseFrac)
	case c.feasiblePct <= 0 || c.feasiblePct > 1:
		return fmt.Errorf("feasible-pct must be in (0,1], got %g", c.feasiblePct)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.workers, "workers", cfg.workers, "number of workers (left partition; must be <= tasks)")
	flag.IntVar(&cfg.tasks, "tasks", cfg.tasks, "number of tasks (right partition)")
	flag.IntVar(&cfg.skills, "skills", cfg.skills, "latent skill/requirement vector dimensionality")
	flag.Float64Var(&cfg.noiseFrac, "noise-frac", cfg.noiseFrac, "idiosyncratic-noise magnitude as a fraction of a skill deficit")
	flag.Float64Var(&cfg.feasiblePct, "feasible-pct", cfg.feasiblePct, "percentile of costs kept as feasible pairs, in (0,1]")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic instance shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run generates the worker/task instance described by cfg, solves it both
// ways, and writes a report to w. Bare lines carry deterministic facts
// (counts and the two assignment invariants, reproducible for a fixed
// seed); lines prefixed with "# " carry volatile telemetry (durations and
// heap figures) that vary per run and per machine. All output goes to w
// so a test can capture and assert on the deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.workers=%d\n", cfg.workers)
	fmt.Fprintf(w, "config.tasks=%d\n", cfg.tasks)
	fmt.Fprintf(w, "config.skills=%d\n", cfg.skills)
	fmt.Fprintf(w, "config.feasible_pct=%.2f\n", cfg.feasiblePct)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	base := readMem()

	// Generate the cost matrix and derive the feasibility threshold.
	if err := ctx.Err(); err != nil {
		return err
	}
	cost := generateCosts(cfg)
	threshold := feasibilityThreshold(cost, cfg.feasiblePct)
	feasible := countFeasible(cost, threshold)
	fmt.Fprintf(w, "feasible.pairs=%d\n", feasible)

	// Hungarian: globally cheapest one-to-one assignment over the full
	// cost matrix. The optimal total cost is an exact integer (a sum of
	// integer cost cells), so it is a deterministic fact.
	hStart := time.Now()
	a, err := search.HungarianCtx(ctx, cost, cfg.workers, cfg.tasks)
	if err != nil {
		return fmt.Errorf("hungarian: %w", err)
	}
	hElapsed := time.Since(hStart)
	fmt.Fprintf(w, "hungarian.optimal_cost=%d\n", int64(a.TotalCost))

	// Hopcroft-Karp: largest matching over the feasible-pairs graph. The
	// maximum cardinality is unique even when the matching itself is not,
	// so the matching size is a deterministic fact.
	mStart := time.Now()
	match, err := maxFeasibleMatching(ctx, cfg, cost, threshold)
	if err != nil {
		return fmt.Errorf("matching: %w", err)
	}
	mElapsed := time.Since(mStart)
	fmt.Fprintf(w, "matching.size=%d\n", match.Size)

	// The willingness rule is binding when the maximum feasible matching
	// cannot staff every worker. min(workers, tasks) is the ceiling a
	// perfect assignment would reach.
	ceiling := cfg.workers
	if cfg.tasks < ceiling {
		ceiling = cfg.tasks
	}
	bindingFlag := 0
	if match.Size < ceiling {
		bindingFlag = 1
	}
	fmt.Fprintf(w, "matching.ceiling=%d\n", ceiling)
	fmt.Fprintf(w, "feasibility.binding=%d\n", bindingFlag)

	built := readMem()
	fmt.Fprintf(w, "# hungarian.elapsed=%s\n", hElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# matching.elapsed=%s\n", mElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(built.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(built.HeapAlloc-base.HeapAlloc))
	fmt.Fprintf(w, "# mem.total_alloc=%s\n", humanBytes(built.TotalAlloc-base.TotalAlloc))
	fmt.Fprintf(w, "# mem.num_gc=%d\n", built.NumGC-base.NumGC)

	return nil
}

// generateCosts builds the row-major n*m cost matrix from the seeded RNG.
// It draws, in a fixed order so the stream is reproducible: the per-skill
// importance weights, then every worker skill vector, then every task
// requirement vector, then the per-pair noise in row-major order. The
// cost of a pair is the weighted under-qualification deficit (a worker
// pays only for skills the task needs and they lack) plus noise, scaled
// and rounded to an exact integer held as a float64.
func generateCosts(cfg config) []float64 {
	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed instance for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))

	weights := make([]float64, cfg.skills)
	for k := range weights {
		weights[k] = 1 + 2*rng.Float64() // importance in [1,3]
	}

	skill := make([][]float64, cfg.workers)
	for i := range skill {
		v := make([]float64, cfg.skills)
		for k := range v {
			v[k] = rng.Float64()
		}
		skill[i] = v
	}

	req := make([][]float64, cfg.tasks)
	for j := range req {
		v := make([]float64, cfg.skills)
		for k := range v {
			v[k] = rng.Float64()
		}
		req[j] = v
	}

	// A representative deficit is ~0.5 weighted by ~2 over each skill, so
	// the noise ceiling is a fraction of one skill's worth of deficit —
	// enough to break ties without swamping the latent structure.
	noiseCeiling := cfg.noiseFrac

	cost := make([]float64, cfg.workers*cfg.tasks)
	for i := 0; i < cfg.workers; i++ {
		si := skill[i]
		for j := 0; j < cfg.tasks; j++ {
			rj := req[j]
			deficit := 0.0
			for k := 0; k < cfg.skills; k++ {
				if d := rj[k] - si[k]; d > 0 {
					deficit += weights[k] * d
				}
			}
			noise := noiseCeiling * rng.Float64()
			cost[i*cfg.tasks+j] = math.Round(costScale * (deficit + noise))
		}
	}
	return cost
}

// feasibilityThreshold returns the pct-th percentile of all cost values:
// the cutoff at or below which a (worker, task) pair is considered
// feasible. Sorting a copy keeps the original row-major matrix intact for
// Hungarian. The value at a fixed percentile index is deterministic
// regardless of how the sort permutes equal (integer) costs.
func feasibilityThreshold(cost []float64, pct float64) float64 {
	flat := make([]float64, len(cost))
	copy(flat, cost)
	sort.Float64s(flat)
	idx := int(pct * float64(len(flat)))
	if idx >= len(flat) {
		idx = len(flat) - 1
	}
	return flat[idx]
}

// countFeasible reports how many pairs survive the feasibility cutoff —
// the edge count of the Hopcroft-Karp graph.
func countFeasible(cost []float64, threshold float64) int {
	n := 0
	for _, c := range cost {
		if c <= threshold {
			n++
		}
	}
	return n
}

// maxFeasibleMatching builds the bipartite graph whose only edges are the
// feasible (worker -> task) pairs and finds the largest matching with
// Hopcroft-Karp, which ignores cost and answers "how many workers can be
// staffed at all once the feasibility rule is honoured?".
//
// The adjlist Mapper assigns sparse, hash-derived NodeIDs (the low 8 bits
// of every NodeID are a shard index), so the worker keys 0..workers-1 do
// NOT map to the contiguous NodeID range [0, workers). Passing
// nLeft = MaxNodeID therefore makes every interned NodeID a potential left
// vertex; only worker nodes carry out-edges (task nodes appear solely as
// edge targets), so only workers are ever matched as left vertices and
// the matching cardinality is exactly the number of staffed workers. This
// mirrors the convention in search/hopcroft_karp_test.go.
func maxFeasibleMatching(ctx context.Context, cfg config, cost []float64, threshold float64) (search.Matching, error) {
	adj := adjlist.New[int, struct{}](adjlist.Config{Directed: true})

	// Intern every worker, then every task, so both partitions exist
	// before any edge references them. Worker keys are 0..workers-1 and
	// task keys are workers..workers+tasks-1, a disjoint, stable encoding
	// the match resolver below relies on.
	for i := 0; i < cfg.workers; i++ {
		if err := adj.AddNode(i); err != nil {
			return search.Matching{}, fmt.Errorf("AddNode worker %d: %w", i, err)
		}
	}
	for j := 0; j < cfg.tasks; j++ {
		if err := adj.AddNode(cfg.workers + j); err != nil {
			return search.Matching{}, fmt.Errorf("AddNode task %d: %w", j, err)
		}
	}

	for i := 0; i < cfg.workers; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return search.Matching{}, err
			}
		}
		for j := 0; j < cfg.tasks; j++ {
			if cost[i*cfg.tasks+j] <= threshold {
				if err := adj.AddEdge(i, cfg.workers+j, struct{}{}); err != nil {
					return search.Matching{}, fmt.Errorf("AddEdge %d->%d: %w", i, cfg.workers+j, err)
				}
			}
		}
	}

	c := csr.BuildFromAdjList(adj)
	match, err := search.HopcroftKarpCtx(ctx, c, int(c.MaxNodeID())) //nolint:gosec // G115: bounded example graph size, no realistic overflow
	if err != nil {
		return search.Matching{}, fmt.Errorf("hopcroft-karp: %w", err)
	}
	return match, nil
}

// checkEvery bounds how often edge construction polls ctx for
// cancellation: often enough that a cancelled large run stops promptly,
// rare enough that the check is free relative to the surrounding work.
const checkEvery = 256

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers (mirrors example 26)
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage.
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
