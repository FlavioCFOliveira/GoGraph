package sim

// pagerank_ranker.go — rmp #2495: the STATEFUL [centrality.PageRanker] driven as
// a reused object across an interleaved serial/parallel sequence, so its two
// published promises — bit-identity with the one-shot and the result-slice
// aliasing hazard — are witnessed rather than assumed.
//
// # What was unreached, and why the gap was invisible
//
// `NewPageRanker`/`Run` is not entirely undriven: [ctxEntries] in
// search_ctx_cancel.go has a `PageRanker.Run` row, and it is the ONE row in that
// table whose identity arm compares two genuinely independent implementations
// (`Run` against [centrality.PageRankCtx]) rather than a delegation. But every
// property that makes a PageRanker a PageRanker is outside what that row can see:
//
//   - it builds a FRESH ranker per call (`centrality.NewPageRanker(f.dir).Run(...)`),
//     so no cached state is ever reused;
//   - it Runs ONCE, with the default options, so no result ever outlives a
//     subsequent Run and the aliasing hazard is structurally unreachable;
//   - its fixtures are a few tens of nodes, which is below
//     `pageRankParallelThreshold`, so the reverse-CSR transpose is never built at
//     all and the identity arm compares two SERIAL runs.
//
// The in-package tests reach further and still stop short.
// `TestPageRanker_BitIdenticalToOneShot` (pagerank_repeated_test.go) does compare
// bit patterns, on a 7-node graph and a 10 000-node Barabasi-Albert graph, over
// three runs — but all three runs use the IDENTICAL options, so no run ever
// starts from a state a differently-converging run left behind; it never touches
// GOMAXPROCS, so which regime the large fixture takes is a property of the host
// and is asserted nowhere; and the transpose, if built at all, is built on the
// FIRST run, never mid-sequence. `TestPageRank_ParallelBitIdentical`
// (pagerank_parallel_test.go) does clamp GOMAXPROCS and does compare serial
// against parallel — but only for the ONE-SHOT `PageRank`, never for a
// PageRanker. And nothing anywhere pins the aliasing contract: the single
// mention of it in the whole package is a comment in
// `TestPageRanker_ConcurrentIndependent` saying the test "mirrors the documented
// contract", above code that neither copies the result nor Runs again.
//
// So the two claims this file exists for were carried by their godoc alone.
//
// # A premise in the task was wrong: options cannot move the regime
//
// The brief asked for the sequence to interleave the serial and parallel regimes
// "with varying options". [centrality.PageRankOptions] has no parallelism knob —
// VERIFIED in source, its only fields are `Damping`, `MaxIterations` and
// `Tolerance`. The regime is decided inside `pageRankState.run` as
//
//	workers := runtime.GOMAXPROCS(0)
//	useParallel := workers > 1 && live >= pageRankParallelThreshold
//
// and a PageRanker is bound to one immutable CSR, so `live` is fixed for its
// whole lifetime. The only reachable lever is therefore the process-global
// GOMAXPROCS, over a fixture with at least `pageRankParallelThreshold` live
// nodes — and that is also the only way to make the LAZY transpose build land
// mid-sequence instead of on the first Run.
//
// # How the regime each Run took is ESTABLISHED, not assumed
//
// Two independent instruments, because the task explicitly refuses timing:
//
//  1. A DERIVATION. `live` is recomputed here from the fixture's own edge list
//     (a node is live when it has at least one incident edge — the same
//     definition `newPageRankState` uses), GOMAXPROCS is read back inside the
//     clamped window, and the documented predicate is re-evaluated. This is a
//     derivation from the two inputs the decision consumes, and it is labelled
//     as such.
//
//  2. AN OBSERVATION, exact and deterministic. Every worker of the parallel
//     engine starts inside `pprof.Do(e.ctx, ...)`, and `pprof.Do` consults the
//     PARENT label set — `WithLabels` calls `ctx.Value(labelContextKey{})` — on
//     the context the caller handed to `Run`. [prLabelProbe] is a context that
//     counts exactly those lookups. The serial path creates no engine and no
//     worker, so it performs ZERO of them; the parallel path performs one per
//     worker at spawn, and the spawn provably precedes the first
//     `iterate` return, because `iterate` sends on each worker's UNBUFFERED
//     start channel and that receive happens inside the function `pprof.Do`
//     wraps. Each worker performs a SECOND lookup on the way out
//     (`defer SetGoroutineLabels(ctx)` re-reads the parent set), and
//     `pageRankEngine.close` does not JOIN its workers, so how many of those
//     have landed when the count is read is a scheduling question. The clause is
//     therefore a BAND — zero for serial, `[workers, 2*workers]` for parallel —
//     and only the band membership, never the raw count, enters the reproducible
//     digest. MEASURED in this scenario: 0 for every serial window, and 4, 8 or
//     9 for parallel windows at clamps of 4 and 8 — the 9 is one worker of the
//     PREVIOUS window's pool having exited in the meantime, which is exactly why
//     the clause is a band and not an equality.
//
//     The observation depends on an implementation detail of the standard
//     library (that `pprof.Do` reads the parent label set through
//     `Context.Value`), and it fails LOUDLY if that changes: a parallel window
//     would report zero lookups and the `regime` clause would fire with the
//     foreign-lookup count in its message. The `label-probe` clause exists to
//     name that case directly.
//
// # The two claims need DIFFERENT instruments
//
// Bit-identity is an EXACT comparison and is done on the bit pattern
// (`math.Float64bits`) rather than with `==`, so a +0/-0 divergence and a NaN
// cannot be read as agreement. The package's existing PageRank oracle
// (`pagerankViolations`) compares within `pagerankEpsilon` = 1e-4, which is
// correct for ITS claim — the library-versus-reference convergence gap — and
// would make a bit-identity clause unfalsifiable. It is deliberately not reused
// here. It is also the wrong SCALE for this fixture: MEASURED on the catalogue
// seed's 3 069-node graph, the median rank is 2.03e-4 and the smallest 1.69e-4,
// so an absolute 1e-4 tolerance is HALF a typical rank — it would accept a rank
// that is wrong by 50%.
//
// The aliasing claim is pinned in BOTH directions, because only one of them is
// about aliasing:
//
//   - the previous window's returned SLICE must read the new run's values (the
//     buffer really was reused). MEASURED: every element changed — 3 069 of
//     3 069 at the catalogue seed, at all five window transitions;
//   - the COPY taken from it must still hold what it held, checked against a
//     hash recorded at copy time rather than against itself, so the control
//     cannot pass by construction;
//   - and both are gated on the two consecutive results actually DIFFERING. If
//     run k+1 produced the same vector as run k, "the buffer changed" would be
//     unfalsifiable — which is why the plan gives every window its own damping;
//   - plus the whole-sequence shape: `Run` may only ever return one of TWO
//     backing arrays, and at least one of them must come back twice. Pointer
//     identity of the first element gives that exactly, and it is what separates
//     "aliases an internal buffer" from "happens to return equal values".
//
//     The shape is AT MOST two, not exactly two, and that distinction was a
//     finding of the soak sweep rather than a guess: which buffer a run returns
//     is `start XOR (iterations mod 2)`, because `run` swaps `cur` and `next`
//     once per iteration and returns `cur`. MEASURED, seed 0x8d10afeecdf8dcf
//     gave all six windows an EVEN iteration count and therefore returned the
//     SAME array six times — one distinct buffer, five repeats. An "exactly two"
//     assertion would have been a parity coincidence dressed up as an invariant,
//     and it failed on the first 32-seed sweep.
//
// # What is NOT claimed
//
//   - THE CACHE IS EVIDENCE, NOT A CLAUSE. That the transpose is built once and
//     reused is only observable through allocation, and the alloc counter is
//     PROCESS-GLOBAL, so an upper bound on a later window would flake under a
//     swarm that runs other scenarios beside this one. Only the LOWER bound on
//     the first parallel window is asserted (noise can only add), and the
//     measured per-window deltas ride in the evidence. MEASURED on the reference
//     host at the catalogue seed, un-raced: 16 and 0 bytes for the serial
//     windows, 117 456 for the FIRST parallel window against a 104 328-byte
//     floor, and 11 144 then 2 288 for the two parallel windows after it — a
//     10.5x drop that says the cache works, reported as a number rather than
//     dressed up as a tripwire.
//   - NO CRASH/RECOVERY ARM. Both claims are pure functions of an immutable CSR
//     snapshot and touch no durable state, so repeating them after a recovery
//     would cost time and detect nothing. The per-tick half of this task's
//     coverage — which DOES run after every recovery, on the recovered graph's
//     own CSR — is [pagerankerStatefulViolations] in search_pagerank.go.
//   - NO CONCURRENCY ARM. A PageRanker is documented as unsafe for concurrent
//     use, and `TestPageRanker_ConcurrentIndependent` already covers the
//     supported shape (one ranker per goroutine over a shared CSR).
//   - THE PARTITION SHAPE IS DERIVED, NOT OBSERVED. `edgeBalancedBounds`
//     documents that a hub-dominated graph collapses several boundaries onto one
//     vertex, leaving a worker an empty range. This file recomputes that
//     partition from its own in-degree prefix sums to record whether the fixture
//     reaches that shape; it is coverage evidence about the FIXTURE, and no
//     clause asserts anything about the library from it.
//
// # A finding this file recorded, and the godoc it corrected
//
// [centrality.PageRank]'s godoc used to claim the parallel pull path is
// "bit-for-bit identical to the serial path regardless of GOMAXPROCS or worker
// scheduling", and that "the per-worker partial L1 deltas are reduced in fixed
// worker-id order, so the returned delta is likewise deterministic across worker
// counts". The second sentence is true for a GIVEN worker count and not across
// worker counts: reducing per-range partial sums is not the same float operation
// as one sequential sum.
//
// The godoc was CORRECTED rather than the reduction changed (rmp #2605, decided
// with the user): it now states the per-given-count guarantee, the across-count
// caveat, and the bound measured below. The reduction is unchanged, so
// everything this file measures still holds and the `cross-regime` clause keeps
// exactly the role described at the end of this note. MEASURED, over one pair of consecutive iterate vectors from a
// 2 400-node probe graph of the same family, a sequential L1 reduction gave
// 0x3fb51ef43754e4b2 while equal-range partitioned reductions gave five
// DIFFERENT values for 2, 3, 4, 8 and 10 ranges (0x...e469, e476, e474, e472,
// e473) — a spread of 73 ULP, about 1e-14 relative. The result stays
// bit-identical only because that difference has never straddled the convergence
// threshold, which would change the iteration count and with it the answer.
//
// How unlikely that is was measured rather than assumed. A search over 40 seeds x
// 4 dampings x 9 tolerances x 12 worker counts — 17 280 serial-versus-parallel
// comparisons — found ZERO bit divergences and zero iteration-count differences,
// and a 400-seed sweep of the scenario itself found zero across its own 1 600
// comparisons. That is what the arithmetic predicts: the delta shrinks by a
// factor of about `d` per iteration, so the stopping delta is spread over a
// log-width of about ln(1/d) — 0.11 to 0.60 across this scenario's damping band
// — and the chance of it landing inside a 1e-14-wide relative window is on the
// order of 1e-13 per run.
//
// So the `cross-regime` clause is NOT a likely detector of the threshold
// coincidence, and it is not offered as one. What it detects is a STRUCTURAL
// change — a pull formulation that stopped summing each vertex's in-edges in the
// reverse-CSR's increasing-source order, a partition that stopped being
// contiguous, a reduction reordered — because any of those diverges
// systematically rather than by coincidence. If it fires, the finding is the
// change beneath it, not the clause — and no longer the godoc claim, which
// #2605 brought into line with what is measured here.

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// ScenarioPageRankRanker is the catalogue key for the stateful-PageRanker
// scenario.
const ScenarioPageRankRanker = "pagerank-ranker"

// pageRankRankerDefaultSeed is the catalogue seed.
const pageRankRankerDefaultSeed uint64 = 0x2495_9A17_C0DE

// pageRankRankerSeedMix salts the scenario seed so this file's draw stream is
// disjoint from every other checker's.
const pageRankRankerSeedMix uint64 = 0x2495_B171_DE71_79AB

// pageRankerThresholdMirror mirrors `pageRankParallelThreshold` in
// search/centrality/pagerank.go, which is unexported and has no accessor. It is
// the live-node count at or above which the parallel pull path is eligible.
// [TestPageRankRanker_ThresholdMirrorMatchesSource] parses the constant out of
// the library source and fails if the two ever drift, because a raised
// threshold would silently push this scenario's fixture back onto the serial
// path and make every parallel clause vacuous while still passing.
const pageRankerThresholdMirror = 2048

// The fixture size. pageRankerMinNodes keeps at least a 25% margin above
// pageRankerThresholdMirror so a modest change to the fixture generator cannot
// drop the graph below the parallel threshold, and the span gives the seed room
// to vary the size without threatening that margin.
const (
	pageRankerMinNodes = 2560
	pageRankerNodeSpan = 512
)

// The damping band the plan draws from. The low end is assigned to the
// reference window because it converges in the fewest iterations, and the high
// end to the iteration-cap window. Both ends are pinned rather than drawn so
// the convergence-mix gate is a property of the plan and not of the draw.
const (
	pageRankerDampingLow  = 0.55
	pageRankerDampingHigh = 0.90
)

// The per-window budgets. pageRankerMaxIters is generous enough that every
// window whose damping is at most pageRankerDampingHigh converges before it —
// log(1e-9)/log(0.9) is 197 — and pageRankerCapIters is small enough that the
// capped window provably does NOT converge, so both exits are reached by
// construction.
const (
	pageRankerMaxIters  = 300
	pageRankerCapIters  = 12
	pageRankerTolerance = 1e-9
	pageRankerCapTol    = 1e-12
)

// pageRankerRefEpsilon is the absolute tolerance for the comparison against the
// independent power-iteration reference ([pagerankReference], driven to a 1e-13
// residual).
//
// It is derived, not guessed. The library stops at an L1 residual below
// pageRankerTolerance, and for a contraction of factor d the distance to the
// fixpoint is bounded by that residual divided by (1-d): at the reference
// window's damping of 0.55 that is 1e-9/0.45 = 2.2e-9, and at the band's high
// end 0.90 it is 1e-8. This epsilon is an order of magnitude above the worse of
// those, and MEASURED it is ~500x the real deviation (1.79e-10 at damping 0.85
// on this fixture). It is still ~1e-3 of the SMALLEST rank the fixture carries
// (8.5e-5 measured), so a defect large enough to matter cannot hide under it.
const pageRankerRefEpsilon = 1e-7

// pageRankerMassEpsilon bounds |sum(ranks) - 1|. PageRank conserves total mass
// by construction (dangling mass is redistributed, never dropped), so this is an
// independent invariant the reference comparison does not cover. MEASURED, the
// sum landed within 1e-15 of 1 on this fixture.
const pageRankerMassEpsilon = 1e-9

// pageRankerCrossRegimeWorkers returns the worker counts the cross-regime arm
// compares against the serial reference. They are fixed constants rather than
// derived from NumCPU so the partition — and therefore the reduction order the
// arm compares — is a property of the fixture and not of the host, the same
// reason ctxCancelWorkers is a constant.
//
// It is a function rather than a package-level slice because a package-level
// slice is mutable state that CLAUDE.md forbids and global_state_guard_test.go
// polices; a fresh slice per call costs nothing at this cadence.
func pageRankerCrossRegimeWorkers() []int { return []int{2, 3, 4, 8} }

// pageRankerReportCap bounds how many violations one report carries, so a
// wholesale failure cannot produce an unbounded report. It is above the window
// count on purpose: a plan of six windows can fire one clause six times, and a
// cap at the window count would let the first clause crowd every other clause
// out of the report. The package's own tests re-adjudicate the evidence
// ([prAdjudicate]) rather than reading the report, so they always see the
// complete list.
const pageRankerReportCap = 16

// prLabelKeyType is the type name of the context key runtime/pprof uses for its
// goroutine label set. [prLabelProbe] classifies lookups by it so a lookup from
// some other source cannot be counted as a worker spawn. It is an unexported
// standard-library type, so it is matched by name; see the file header for what
// happens if it ever changes.
const prLabelKeyType = "pprof.labelContextKey"

// prLabelProbe is a [context.Context] that counts Value lookups, split by
// whether the key is runtime/pprof's label key. It is the parallel-regime
// observable: the serial path performs none, and the parallel engine performs
// one per worker at spawn plus one per worker on the way out.
//
// # Concurrency contract
//
// prLabelProbe is safe for concurrent use: the engine's workers look up the
// label key from their own goroutines, and both counters are atomic.
type prLabelProbe struct {
	context.Context
	label atomic.Int64
	other atomic.Int64
}

// Value counts the lookup and delegates.
func (p *prLabelProbe) Value(key any) any {
	if fmt.Sprintf("%T", key) == prLabelKeyType {
		p.label.Add(1)
	} else {
		p.other.Add(1)
	}
	return p.Context.Value(key)
}

// newPRLabelProbe wraps parent in a fresh probe.
func newPRLabelProbe(parent context.Context) *prLabelProbe {
	return &prLabelProbe{Context: parent}
}

// prAllocBytes returns the process-wide cumulative heap-allocated byte count.
//
// It reads runtime.ReadMemStats rather than the cheaper, non-stop-the-world
// runtime/metrics counter `/gc/heap/allocs:bytes`, and the reason is a
// measurement, not a preference: that counter is fed from per-P caches that are
// flushed at a GC or an mcache refill, so an allocation smaller than the 32 KiB
// large-object threshold need not be visible when it is read. MEASURED through
// the metrics API on this fixture, every SERIAL window read a delta of exactly
// 0 bytes, and so did two of the three parallel windows — while the first
// parallel window, whose revEdges array alone is a large object, read 114 KiB.
// A floor clause fed by a counter that under-reports by tens of kilobytes is a
// flake, so the stop-the-world read (which flushes every mcache) is the correct
// trade: 12 pauses per run against a clause that means what it says.
func prAllocBytes() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.TotalAlloc
}

// -----------------------------------------------------------------------------
// The fixture
// -----------------------------------------------------------------------------

// prFixture is the seed-derived graph the whole scenario runs on, kept in both
// forms the run needs: the edge list the independent reference consumes, and the
// CSR the library consumes.
type prFixture struct {
	c     *csr.CSR[float64]
	edges [][2]int
	n     int
	live  int
	hub   int
}

// prGenFixture derives the fixture from seed.
//
// The shape is chosen for three reasons and no others:
//
//   - a forward spine 0->1->...->(n-1) makes every node live, so `live == n` and
//     the parallel threshold is crossed by a margin the generator controls
//     rather than by luck; it also leaves n-1 a dangling sink, which is what
//     drives the dangling-mass redistribution both the library and the reference
//     implement;
//   - one HUB at index 1 receiving an in-edge from every other node makes the
//     in-degree distribution extremely skewed, which is the regime
//     `edgeBalancedBounds` documents as collapsing several partition boundaries
//     onto one vertex. Equal-vertex chunking would be grossly imbalanced there,
//     so the fixture exercises the load-balancing branch rather than its trivial
//     case;
//   - a back edge and seed-chosen extra arcs make the stationary distribution
//     non-trivial (mass recirculates) and vary the graph across seeds.
//
// Edges are emitted in a fixed order with no map iteration, so the fixture is a
// pure function of the seed.
func prGenFixture(seed *Seed) *prFixture {
	n := pageRankerMinNodes + seed.IntN(pageRankerNodeSpan)
	const hub = 1
	edges := make([][2]int, 0, 3*n)
	// The spine: every node acquires an incident edge, and n-1 stays a sink.
	for i := 0; i < n-1; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	// The hub: an in-edge from every node that is not the hub and not the sink.
	for i := 0; i < n-1; i++ {
		if i != hub {
			edges = append(edges, [2]int{i, hub})
		}
	}
	// A back edge from a seed-chosen middle node to 0, so mass recirculates.
	edges = append(edges, [2]int{1 + seed.IntN(n-2), 0})
	// Seed-chosen extra arcs. The sink is never a source, so it stays dangling.
	extra := n / 4
	for k := 0; k < extra; k++ {
		a := seed.IntN(n - 1)
		b := seed.IntN(n)
		if a == b {
			continue
		}
		edges = append(edges, [2]int{a, b})
	}
	return &prFixture{
		n:     n,
		edges: edges,
		c:     pagerankBuildCSR(n, edges),
		live:  prLiveCount(n, edges),
		hub:   hub,
	}
}

// prLiveCount recomputes the live-node count from the edge list, using the same
// definition `newPageRankState` uses: a node is live when it has at least one
// incident edge, in or out. It is an independent derivation of one of the two
// inputs the regime decision consumes — deliberately not read back from the CSR,
// so the scenario never asks the library what the library is about to decide.
func prLiveCount(n int, edges [][2]int) int {
	live := make([]bool, n)
	for _, e := range edges {
		live[e[0]] = true
		live[e[1]] = true
	}
	c := 0
	for _, l := range live {
		if l {
			c++
		}
	}
	return c
}

// prExpectParallel re-evaluates the documented regime predicate from the two
// inputs it consumes. It mirrors `pageRankState.run`:
//
//	useParallel := runtime.GOMAXPROCS(0) > 1 && live >= pageRankParallelThreshold
func prExpectParallel(live, workers int) bool {
	return workers > 1 && live >= pageRankerThresholdMirror
}

// prExpectedWorkers is the worker count the engine builds for a parallel run:
// GOMAXPROCS, clamped down to the node count. It mirrors `run`'s
// `if workers > n { workers = n }`.
func prExpectedWorkers(workers, n int) int {
	if workers > n {
		return n
	}
	return workers
}

// prUint64Bytes and prNodeIDBytes are the widths the reverse-CSR arrays are
// built from: revVerts is []uint64 and revEdges is []graph.NodeID, which is
// declared `type NodeID uint64` (graph/graph.go). They are constants rather than
// a sizeof so no unsafe is needed here;
// [TestPageRankRanker_NodeIDWidthIsPinned] holds both against the real types.
const (
	prUint64Bytes = 8
	prNodeIDBytes = 8
)

// prTransposeBytes is the byte floor the FIRST parallel run must allocate: the
// structure-only reverse-CSR `pageRankBuildReverseStructure` builds, which is
// revVerts (n+1 uint64 offsets), revEdges (one graph.NodeID per edge) and the
// temporary scatter cursor (n uint64). Weights and handles are deliberately not
// transposed, so they are not counted.
//
// It is a FLOOR and nothing else: the engine, its channels and its per-worker
// pprof labels allocate on top of it, and the counter it is compared against is
// process-global. MEASURED, the first parallel window's delta was 117 456 bytes
// against a floor of 104 328 for the catalogue seed's 3 069-node, 6 902-edge
// fixture: the floor is most of the allocation, and the few kilobytes above it
// are the engine, its channels and its per-worker pprof labels.
func prTransposeBytes(n, edges int) uint64 {
	return uint64(n+1)*prUint64Bytes + uint64(edges)*prNodeIDBytes + uint64(n)*prUint64Bytes
}

// prDerivedEmptyRanges recomputes the edge-balanced partition for a given worker
// count from the fixture's own in-degree prefix sums, mirroring
// `edgeBalancedBounds`, and returns how many workers it leaves an EMPTY vertex
// range. It is coverage evidence about the fixture's skew, not an observation of
// the library: nothing is asserted about the engine from it.
func prDerivedEmptyRanges(fx *prFixture, workers int) int {
	if workers <= 1 {
		return 0
	}
	revVerts := make([]uint64, fx.n+1)
	for _, e := range fx.edges {
		revVerts[e[1]+1]++
	}
	for i := 1; i <= fx.n; i++ {
		revVerts[i] += revVerts[i-1]
	}
	bounds := make([]int, workers+1)
	bounds[workers] = fx.n
	total := revVerts[fx.n]
	v := 0
	for w := 1; w < workers; w++ {
		target := uint64(w) * total / uint64(workers)
		for v < fx.n && revVerts[v] < target {
			v++
		}
		bounds[w] = v
	}
	empty := 0
	for w := 0; w < workers; w++ {
		if bounds[w+1] == bounds[w] {
			empty++
		}
	}
	return empty
}

// -----------------------------------------------------------------------------
// The plan
// -----------------------------------------------------------------------------

// prWindow is one Run in the interleaved sequence: the GOMAXPROCS clamp it runs
// under and the options it runs with.
type prWindow struct {
	Opts  centrality.PageRankOptions
	Clamp int
}

// prPlan builds the interleaved sequence.
//
// The clamp pattern is 1, 4, 1, 8, 4, 1 and every position in it is load-bearing:
//
//	0  serial   — the first Run, so the transpose is provably NOT yet built
//	1  parallel — the FIRST parallel Run, so the lazy transpose is built HERE,
//	              mid-sequence, which is the state no existing test reaches
//	2  serial   — a serial Run with the transpose cached and unused
//	3  parallel — a DIFFERENT worker count, so the partition and therefore the
//	              delta-reduction order differ from window 1's while the answer
//	              must not
//	4  parallel — the cached transpose reused, and the iteration-CAP exit
//	5  serial   — the reference-compared window, at the lowest damping so it
//	              converges in the fewest iterations
//
// Only windows 0..3 draw their damping; window 4 is pinned to the top of the
// band (so its 12-iteration budget provably caps) and window 5 to the bottom (so
// its 300-iteration budget provably converges). That keeps the convergence-mix
// gate a property of the plan while the seed still varies four of the six
// trajectories continuously.
func prPlan(seed *Seed) []prWindow {
	clamps := []int{1, 4, 1, 8, 4, 1}
	plan := make([]prWindow, 0, len(clamps))
	for i, clamp := range clamps {
		w := prWindow{Clamp: clamp}
		switch i {
		case 4:
			w.Opts = centrality.PageRankOptions{
				Damping: pageRankerDampingHigh, MaxIterations: pageRankerCapIters, Tolerance: pageRankerCapTol,
			}
		case 5:
			w.Opts = centrality.PageRankOptions{
				Damping: pageRankerDampingLow, MaxIterations: pageRankerMaxIters, Tolerance: pageRankerTolerance,
			}
		default:
			d := pageRankerDampingLow + seed.Float64()*(pageRankerDampingHigh-pageRankerDampingLow)
			w.Opts = centrality.PageRankOptions{
				Damping: d, MaxIterations: pageRankerMaxIters, Tolerance: pageRankerTolerance,
			}
		}
		plan = append(plan, w)
	}
	return plan
}

// The reference-compared window is the LAST window of the plan, whichever plan
// runs. In the default plan that is the one pinned to the lowest damping so it
// converges well inside its budget; a custom plan (the soak arms build wider
// ones) must likewise end on a window that converges, and
// `gate:reference-converged` fails the run if it does not — comparing a vector
// that never reached the fixpoint against a 1e-13-residual reference would fail
// correct code.

// -----------------------------------------------------------------------------
// Perturbations
// -----------------------------------------------------------------------------

// prPerturb names one deliberate corruption, applied at the exact comparison
// site so the perturbed run reproduces the OUTPUT the real defect would produce
// rather than merely flipping a verdict. It is threaded through
// [PageRankRankerConfig] as a parameter; nothing in this file holds it in a
// package-level variable.
type prPerturb uint8

// The perturbations. Each names the clause it is meant to fire.
const (
	// prPerturbNone is the unperturbed run.
	prPerturbNone prPerturb = iota
	// prPerturbFlipResultBit flips one bit of a COPY of the Run result before it
	// is compared with the one-shot, reproducing a Run whose cached state changed
	// the answer. Fires `bit-identity`. The engine's own buffer is never written.
	prPerturbFlipResultBit
	// prPerturbDropIteration reports the Run's iteration count one lower than it
	// was, reproducing a Run that converged at a different step. Fires
	// `iteration-parity`.
	prPerturbDropIteration
	// prPerturbHideWorkers reports zero label lookups for a parallel window,
	// reproducing an engine that silently stopped fanning out. Fires `regime`.
	prPerturbHideWorkers
	// prPerturbForeignLookup reports a lookup under an unrecognised context key,
	// reproducing the standard library changing how pprof.Do reads its parent
	// label set. Fires `label-probe`.
	prPerturbForeignLookup
	// prPerturbInflateTransposeFloor raises the transpose byte floor far above
	// anything the run could allocate, reproducing a first parallel Run that
	// skipped the transpose build. Fires `transpose-alloc`.
	prPerturbInflateTransposeFloor
	// prPerturbAliasCopyAliases makes the "copy" of a result alias the live
	// buffer instead of copying it, reproducing the caller mistake the aliasing
	// note warns about. Fires `alias-copy-intact`.
	prPerturbAliasCopyAliases
	// prPerturbFreezePrevSlice reports the previous window's slice as unchanged,
	// reproducing a Run that stopped reusing the buffer. Fires
	// `alias-invalidated`.
	prPerturbFreezePrevSlice
	// prPerturbFreshBuffers reports every window's result as a distinct backing
	// array, reproducing a Run that allocates a fresh result each call instead of
	// aliasing one of two internal buffers. Fires `buffer-recycling`.
	prPerturbFreshBuffers
	// prPerturbShiftReference moves one reference rank well outside the epsilon,
	// reproducing a divergence from the independent power iteration. Fires
	// `reference`.
	prPerturbShiftReference
	// prPerturbBreakMass scales the measured mass off 1, reproducing lost or
	// duplicated dangling mass. Fires `mass`.
	prPerturbBreakMass
	// prPerturbCrossRegimeBit flips one bit of the parallel one-shot's copy
	// before it is compared with the serial one, reproducing the divergence the
	// L1-reduction-order finding in the file header would cause. Fires
	// `cross-regime`.
	prPerturbCrossRegimeBit
	// prPerturbSerialOnly clamps every window to one core. The sequence then has
	// no parallel window at all, so the transpose is never built and the
	// interleaving never happens. Fires `gate:both-regimes` and
	// `gate:mid-sequence-build`. It is applied to the PLAN, so it needs its own
	// run.
	prPerturbSerialOnly
	// prPerturbSameDamping gives every window the identical damping, so
	// consecutive results are equal and the aliasing pin has nothing to detect.
	// Fires `gate:alias-armed`. Applied to the PLAN.
	prPerturbSameDamping
)

// String names the perturbation for a subtest name and a report.
func (p prPerturb) String() string {
	switch p {
	case prPerturbNone:
		return "none"
	case prPerturbFlipResultBit:
		return "flip-result-bit"
	case prPerturbDropIteration:
		return "drop-iteration"
	case prPerturbHideWorkers:
		return "hide-workers"
	case prPerturbForeignLookup:
		return "foreign-lookup"
	case prPerturbInflateTransposeFloor:
		return "inflate-transpose-floor"
	case prPerturbAliasCopyAliases:
		return "alias-copy-aliases"
	case prPerturbFreezePrevSlice:
		return "freeze-prev-slice"
	case prPerturbFreshBuffers:
		return "fresh-buffers"
	case prPerturbShiftReference:
		return "shift-reference"
	case prPerturbBreakMass:
		return "break-mass"
	case prPerturbCrossRegimeBit:
		return "cross-regime-bit"
	case prPerturbSerialOnly:
		return "serial-only"
	case prPerturbSameDamping:
		return "same-damping"
	default:
		return "unknown"
	}
}

// -----------------------------------------------------------------------------
// The evidence
// -----------------------------------------------------------------------------

// prWindowEvidence is everything one window of the sequence measured. Fields
// that are a pure function of the seed feed the reproducible digest; the ones
// that are not — the raw label-lookup count and the allocation delta — are
// carried for a human and are excluded from it, which each field's comment says.
type prWindowEvidence struct {
	// Damping, Tolerance, MaxIterations and Clamp are the plan.
	Damping       float64
	Tolerance     float64
	MaxIterations int
	Clamp         int
	// Workers is GOMAXPROCS read back INSIDE the clamped window, and
	// ExpectWorkers is that value clamped down to the node count the way the
	// engine clamps it.
	Workers       int
	ExpectWorkers int
	// ExpectParallel is the documented predicate re-evaluated from (live,
	// Workers). It is a derivation, not an observation.
	ExpectParallel bool
	// RunIters and OneShotIters are the iteration counts the two entry points
	// returned; Converged reports whether the Run stopped before its cap.
	RunIters     int
	OneShotIters int
	Converged    bool
	// BitDiff is how many rank elements differ in BIT PATTERN between Run and the
	// one-shot under the same clamp; FirstDiff is the lowest such index (-1 when
	// none) and MaxULP the largest bit-pattern distance.
	BitDiff   int
	FirstDiff int
	MaxULP    uint64
	// LabelLookups counts runtime/pprof label-key lookups on the Run's context —
	// the parallel-regime observation. OtherLookups counts lookups under any
	// other key, which must be zero. NOT in the digest: the count within its band
	// depends on how many workers have exited by the time it is read.
	LabelLookups int64
	OtherLookups int64
	// OneShotLabelLookups is the same observation for the one-shot call beside
	// it. NOT in the digest, for the same reason.
	OneShotLabelLookups int64
	// AllocBytes is the process-wide allocated-byte delta across the Run. NOT in
	// the digest: the counter is process-global and other goroutines contribute.
	AllocBytes uint64
	// Buffer is the index of this result's backing array in the order the
	// sequence first saw it, so 0 and 1 are the two internal buffers and anything
	// higher is a third array Run should never return.
	Buffer int
	// MassErr is |sum(ranks) - 1|.
	MassErr float64
	// ChangedFromPrev is how many elements of the PREVIOUS window's returned
	// slice now read differently than when that window returned it — the aliasing
	// observation. PrevCopyIntact reports whether the copy taken from it still
	// hashes to what it hashed to at copy time — the control. PrevDiffered is how
	// many elements of THIS window's result differ from the previous window's
	// copy, which is what arms the pin.
	ChangedFromPrev int
	PrevCopyIntact  bool
	PrevDiffered    int
	// EmptyRanges is the derived edge-balanced partition's empty-worker count for
	// this window's worker count. Fixture coverage evidence only.
	EmptyRanges int
}

// prCrossRegime is one row of the cross-regime arm: the parallel one-shot at a
// given worker count against the serial one-shot, both over the same fixture and
// options.
type prCrossRegime struct {
	Workers     int
	Iters       int
	SerialIters int
	BitDiff     int
	MaxULP      uint64
	FirstDiff   int
}

// PageRankRankerEvidence is what one run of the scenario measured. It is the
// object both the terminal gate and the short-layer test read, so "the run
// passed" and "the run exercised something" are separate questions with separate
// answers.
type PageRankRankerEvidence struct {
	Windows     []prWindowEvidence
	CrossRegime []prCrossRegime
	// The fixture.
	Nodes     int
	Edges     int
	Live      int
	Threshold int
	Hub       int
	HubInDeg  int
	// Sequence aggregates.
	SerialWindows   int
	ParallelWindows int
	FirstParallel   int
	DistinctBuffers int
	BufferRepeats   int
	AliasArmed      int
	ConvergedRuns   int
	CappedRuns      int
	DistinctIters   int
	MaxEmptyRanges  int
	// The reference arm.
	RefWindow  int
	RefMaxDev  float64
	RefEpsilon float64
	RefIndex   int
	// TransposeFloor is the byte floor the first parallel window is held to, and
	// FirstParallelAlloc the delta it measured.
	TransposeFloor      uint64
	FirstParallelAlloc  uint64
	SecondParallelAlloc uint64
	// Digest is an order-sensitive hash of every reproducible fact above.
	Digest  uint64
	Perturb prPerturb
}

// String renders the evidence for a report and a log line.
func (e *PageRankRankerEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pagerank-ranker: n=%d edges=%d live=%d (threshold %d) hub=%d in-deg=%d; ",
		e.Nodes, e.Edges, e.Live, e.Threshold, e.Hub, e.HubInDeg)
	fmt.Fprintf(&b, "windows=%d serial=%d parallel=%d first-parallel=%d; ",
		len(e.Windows), e.SerialWindows, e.ParallelWindows, e.FirstParallel)
	fmt.Fprintf(&b, "buffers=%d repeats=%d alias-armed=%d; converged=%d capped=%d distinct-iters=%d; ",
		e.DistinctBuffers, e.BufferRepeats, e.AliasArmed, e.ConvergedRuns, e.CappedRuns, e.DistinctIters)
	fmt.Fprintf(&b, "transpose-floor=%dB first-parallel-alloc=%dB second=%dB; ",
		e.TransposeFloor, e.FirstParallelAlloc, e.SecondParallelAlloc)
	fmt.Fprintf(&b, "ref[w%d] max-dev=%.3e (eps %.0e) at rank %d; max-empty-ranges=%d; ",
		e.RefWindow, e.RefMaxDev, e.RefEpsilon, e.RefIndex, e.MaxEmptyRanges)
	fmt.Fprintf(&b, "cross-regime=%d rows; perturb=%s; digest=%#016x",
		len(e.CrossRegime), e.Perturb, e.Digest)
	for i := range e.Windows {
		w := &e.Windows[i]
		regime := "serial"
		if w.ExpectParallel {
			regime = "parallel"
		}
		fmt.Fprintf(&b, "\n  w%d %s clamp=%d workers=%d d=%.4f maxIter=%d iters=%d(%d) conv=%v "+
			"bitdiff=%d lookups=%d/%d(one-shot %d) alloc=%dB buf=%d mass-err=%.2e "+
			"changed-prev=%d/%d copy-intact=%v empty-ranges=%d",
			i, regime, w.Clamp, w.Workers, w.Damping, w.MaxIterations, w.RunIters, w.OneShotIters,
			w.Converged, w.BitDiff, w.LabelLookups, w.OtherLookups, w.OneShotLabelLookups,
			w.AllocBytes, w.Buffer, w.MassErr, w.ChangedFromPrev, w.PrevDiffered,
			w.PrevCopyIntact, w.EmptyRanges)
	}
	for i := range e.CrossRegime {
		r := &e.CrossRegime[i]
		fmt.Fprintf(&b, "\n  cross workers=%d iters=%d(serial %d) bitdiff=%d maxULP=%d",
			r.Workers, r.Iters, r.SerialIters, r.BitDiff, r.MaxULP)
	}
	return b.String()
}

// ReproducibleSummary renders only the facts that are a pure function of the
// seed, so two runs of one seed can be compared without the timing-dependent
// counters (the label-lookup position within its band, and the process-global
// allocation deltas) making a deterministic harness look non-deterministic.
func (e *PageRankRankerEvidence) ReproducibleSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "n=%d e=%d live=%d serial=%d parallel=%d first-par=%d bufs=%d repeats=%d "+
		"armed=%d conv=%d capped=%d iters=%d empty=%d ref-dev=%x cross=%d digest=%#016x",
		e.Nodes, e.Edges, e.Live, e.SerialWindows, e.ParallelWindows, e.FirstParallel,
		e.DistinctBuffers, e.BufferRepeats, e.AliasArmed, e.ConvergedRuns, e.CappedRuns,
		e.DistinctIters, e.MaxEmptyRanges, math.Float64bits(e.RefMaxDev), len(e.CrossRegime), e.Digest)
	for i := range e.Windows {
		w := &e.Windows[i]
		fmt.Fprintf(&b, " w%d[d=%x it=%d/%d bd=%d par=%v buf=%d chg=%d dif=%d ok=%v er=%d]",
			i, math.Float64bits(w.Damping), w.RunIters, w.OneShotIters, w.BitDiff,
			w.ExpectParallel, w.Buffer, w.ChangedFromPrev, w.PrevDiffered, w.PrevCopyIntact,
			w.EmptyRanges)
	}
	for i := range e.CrossRegime {
		r := &e.CrossRegime[i]
		fmt.Fprintf(&b, " x%d[w=%d it=%d/%d bd=%d]", i, r.Workers, r.Iters, r.SerialIters, r.BitDiff)
	}
	return b.String()
}

// prMix is the FNV-1a step the digest is built from.
func prMix(h, v uint64) uint64 { return (h ^ v) * 1099511628211 }

// prBoolBits renders a bool as a digest input.
func prBoolBits(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// computeDigest folds every reproducible fact into an order-sensitive hash. It
// deliberately excludes the label-lookup counts and the allocation deltas: both
// are honest measurements and neither is a function of the seed.
func (e *PageRankRankerEvidence) computeDigest() uint64 {
	h := uint64(14695981039346656037)
	for _, v := range []int64{
		int64(e.Nodes), int64(e.Edges), int64(e.Live), int64(e.Hub), int64(e.HubInDeg),
		int64(e.SerialWindows), int64(e.ParallelWindows), int64(e.FirstParallel),
		int64(e.DistinctBuffers), int64(e.BufferRepeats), int64(e.AliasArmed),
		int64(e.ConvergedRuns), int64(e.CappedRuns), int64(e.DistinctIters),
		int64(e.MaxEmptyRanges), int64(e.RefIndex),
	} {
		h = prMix(h, uint64(v))
	}
	h = prMix(h, math.Float64bits(e.RefMaxDev))
	for i := range e.Windows {
		w := &e.Windows[i]
		h = prMix(h, math.Float64bits(w.Damping))
		h = prMix(h, math.Float64bits(w.Tolerance))
		for _, v := range []int64{
			int64(w.MaxIterations), int64(w.Clamp), int64(w.Workers), int64(w.ExpectWorkers),
			int64(w.RunIters), int64(w.OneShotIters), int64(w.BitDiff), int64(w.FirstDiff),
			int64(w.Buffer), int64(w.ChangedFromPrev), int64(w.PrevDiffered), int64(w.EmptyRanges),
		} {
			h = prMix(h, uint64(v))
		}
		h = prMix(h, w.MaxULP)
		h = prMix(h, prBoolBits(w.ExpectParallel))
		h = prMix(h, prBoolBits(w.Converged))
		h = prMix(h, prBoolBits(w.PrevCopyIntact))
		h = prMix(h, math.Float64bits(w.MassErr))
	}
	for i := range e.CrossRegime {
		r := &e.CrossRegime[i]
		for _, v := range []int64{
			int64(r.Workers), int64(r.Iters), int64(r.SerialIters), int64(r.BitDiff), int64(r.FirstDiff),
		} {
			h = prMix(h, uint64(v))
		}
		h = prMix(h, r.MaxULP)
	}
	return h
}

// prHashFloats hashes a rank vector by BIT PATTERN, so the aliasing control
// records what the copy held rather than comparing the copy with itself.
func prHashFloats(v []float64) uint64 {
	h := uint64(14695981039346656037)
	for _, x := range v {
		h = prMix(h, math.Float64bits(x))
	}
	return h
}

// -----------------------------------------------------------------------------
// The run
// -----------------------------------------------------------------------------

// PageRankRankerConfig parameterises one run of the scenario.
type PageRankRankerConfig struct {
	// Plan is the window sequence. Nil derives it from Seed via [prPlan].
	Plan []prWindow
	// CrossRegimeWorkers are the worker counts the cross-regime arm sweeps. Nil
	// uses [pageRankerCrossRegimeWorkers].
	CrossRegimeWorkers []int
	// Seed drives the fixture and the drawn dampings.
	Seed uint64
	// Perturb is the deliberate corruption to apply, threaded as a parameter so
	// no package-level variable carries it.
	Perturb prPerturb
}

// DefaultPageRankRankerConfig returns the configuration the catalogue entry runs.
func DefaultPageRankRankerConfig(seed uint64) PageRankRankerConfig {
	return PageRankRankerConfig{Seed: seed}
}

// prSeqState carries what one window needs to know about the window before it.
type prSeqState struct {
	prevSlice []float64
	prevCopy  []float64
	buffers   []*float64
	prevHash  uint64
	havePrev  bool
}

// prBufferIndex returns the index of s's backing array in the order the sequence
// first saw it, appending it when new. Pointer identity of the first element is
// the exact question "is this one of the buffers Run already returned", and it
// needs no unsafe.
func prBufferIndex(seen *[]*float64, s []float64) int {
	if len(s) == 0 {
		return -1
	}
	p := &s[0]
	for i, q := range *seen {
		if q == p {
			return i
		}
	}
	*seen = append(*seen, p)
	return len(*seen) - 1
}

// prWithClamp runs fn with GOMAXPROCS clamped to want, restoring the previous
// value on every exit path.
//
// It reads the value back on BOTH sides of the window. A mismatch means a
// FOREIGN clamp — `runCPUStarvation` is the only other one in this package —
// landed inside the window, which would decide this run's regime for it; that is
// a harness interference, not an invariant violation, so it is returned as an
// error and reaches the CLI through the error channel rather than as a false
// report.
//
// The caller holds [gomaxprocsMu] exclusively across the whole clamped phase, so
// since rmp #2613 no foreign clamp can land here. The read-back is kept
// deliberately: it is the detector that caught the interference in the first
// place, and a silent wrong regime is exactly the fail-silent class this suite
// exists to catch. It should now never fire.
func prWithClamp(want int, fn func() error) error {
	prev := runtime.GOMAXPROCS(want)
	defer runtime.GOMAXPROCS(prev)
	if got := runtime.GOMAXPROCS(0); got != want {
		return fmt.Errorf("sim: pagerank-ranker: GOMAXPROCS clamp to %d read back %d before the "+
			"window: a foreign clamp is active", want, got)
	}
	if err := fn(); err != nil {
		return err
	}
	if got := runtime.GOMAXPROCS(0); got != want {
		return fmt.Errorf("sim: pagerank-ranker: GOMAXPROCS was %d at the end of a window clamped "+
			"to %d: a foreign clamp landed mid-window", got, want)
	}
	return nil
}

// prCompareBits compares two rank vectors by BIT PATTERN and returns how many
// elements differ, the lowest differing index (-1 when none) and the largest
// bit-pattern distance. It is exact by construction: a +0/-0 divergence and a
// NaN both count as a difference, which `==` would not report the same way.
func prCompareBits(got, want []float64) (bitDiff, firstDiff int, maxULP uint64) {
	firstDiff = -1
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		a, b := math.Float64bits(got[i]), math.Float64bits(want[i])
		if a == b {
			continue
		}
		bitDiff++
		if firstDiff < 0 {
			firstDiff = i
		}
		d := a - b
		if b > a {
			d = b - a
		}
		if d > maxULP {
			maxULP = d
		}
	}
	// A length difference is itself a divergence, counted so no clause can read
	// two vectors of different lengths as agreement.
	bitDiff += len(got) - n
	bitDiff += len(want) - n
	return bitDiff, firstDiff, maxULP
}

// prFlipOneBit returns a COPY of v with the lowest bit of one element flipped.
// The copy matters: v aliases the PageRanker's internal buffer, and a checker
// that wrote through it would corrupt the object it is auditing.
func prFlipOneBit(v []float64) []float64 {
	out := append([]float64(nil), v...)
	if len(out) > 0 {
		out[len(out)/2] = math.Float64frombits(math.Float64bits(out[len(out)/2]) ^ 1)
	}
	return out
}

// prSum totals a rank vector in index order.
func prSum(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s
}

// prApplyPlanPerturb returns the plan the run should drive. Only the two
// plan-level perturbations change it; every other perturbation acts at a
// comparison site and leaves the plan alone.
func prApplyPlanPerturb(plan []prWindow, p prPerturb) []prWindow {
	switch p {
	case prPerturbSerialOnly:
		out := make([]prWindow, len(plan))
		copy(out, plan)
		for i := range out {
			out[i].Clamp = 1
		}
		return out
	case prPerturbSameDamping:
		out := make([]prWindow, len(plan))
		copy(out, plan)
		for i := range out {
			out[i].Opts.Damping = pageRankerDampingLow
			out[i].Opts.MaxIterations = pageRankerMaxIters
			out[i].Opts.Tolerance = pageRankerTolerance
		}
		return out
	default:
		return plan
	}
}

// prDriveWindow runs one window: the PageRanker under the window's clamp, the
// one-shot beside it under the SAME clamp, and every comparison that needs the
// result before the next Run invalidates it.
//
// It returns the window's evidence and a COPY of the result, and it leaves
// st pointing at this window so the next one can adjudicate the aliasing pin.
func prDriveWindow(parent context.Context, cfg *PageRankRankerConfig, fx *prFixture,
	pr *centrality.PageRanker[float64], w prWindow, st *prSeqState,
) (prWindowEvidence, []float64, error) {
	we := prWindowEvidence{
		Damping: w.Opts.Damping, Tolerance: w.Opts.Tolerance,
		MaxIterations: w.Opts.MaxIterations, Clamp: w.Clamp, FirstDiff: -1, Buffer: -1,
	}
	var got, want []float64
	err := prWithClamp(w.Clamp, func() error {
		we.Workers = runtime.GOMAXPROCS(0)
		we.ExpectWorkers = prExpectedWorkers(we.Workers, fx.n)
		we.ExpectParallel = prExpectParallel(fx.live, we.Workers)
		we.EmptyRanges = prDerivedEmptyRanges(fx, we.ExpectWorkers)

		probe := newPRLabelProbe(parent)
		before := prAllocBytes()
		r, iters, err := pr.Run(probe, w.Opts)
		after := prAllocBytes()
		if err != nil {
			return fmt.Errorf("PageRanker.Run(clamp %d): %w", w.Clamp, err)
		}
		got, we.RunIters, we.AllocBytes = r, iters, after-before
		we.LabelLookups, we.OtherLookups = probe.label.Load(), probe.other.Load()

		oneShot := newPRLabelProbe(parent)
		o, oIters, err := centrality.PageRankCtx(oneShot, fx.c, w.Opts)
		if err != nil {
			return fmt.Errorf("PageRankCtx(clamp %d): %w", w.Clamp, err)
		}
		want, we.OneShotIters, we.OneShotLabelLookups = o, oIters, oneShot.label.Load()
		return nil
	})
	if err != nil {
		return we, nil, err
	}
	prAdjudicateWindow(cfg.Perturb, &we, got, want, st)
	copyOut := append([]float64(nil), got...)
	prAdvanceSeq(cfg.Perturb, st, got, copyOut)
	return we, copyOut, nil
}

// prAdjudicateWindow fills in every comparison the window's evidence carries,
// applying the perturbation at the exact site it belongs to.
func prAdjudicateWindow(p prPerturb, we *prWindowEvidence, got, want []float64, st *prSeqState) {
	we.Converged = we.RunIters < we.MaxIterations
	compared := got
	if p == prPerturbFlipResultBit {
		compared = prFlipOneBit(got)
	}
	we.BitDiff, we.FirstDiff, we.MaxULP = prCompareBits(compared, want)
	if p == prPerturbDropIteration {
		we.RunIters--
	}
	if p == prPerturbHideWorkers && we.ExpectParallel {
		we.LabelLookups = 0
	}
	if p == prPerturbForeignLookup {
		we.OtherLookups++
	}
	we.MassErr = math.Abs(prSum(got) - 1)
	if p == prPerturbBreakMass {
		we.MassErr = 1e-3
	}
	we.Buffer = prBufferIndex(&st.buffers, got)
	we.PrevCopyIntact = true
	if !st.havePrev {
		return
	}
	changed := 0
	for j := range st.prevCopy {
		if j < len(st.prevSlice) && math.Float64bits(st.prevSlice[j]) != math.Float64bits(st.prevCopy[j]) {
			changed++
		}
	}
	if p == prPerturbFreezePrevSlice {
		changed = 0
	}
	we.ChangedFromPrev = changed
	we.PrevCopyIntact = prHashFloats(st.prevCopy) == st.prevHash
	diff := 0
	for j := range st.prevCopy {
		if j < len(got) && math.Float64bits(got[j]) != math.Float64bits(st.prevCopy[j]) {
			diff++
		}
	}
	we.PrevDiffered = diff
}

// prAdvanceSeq points the sequence state at the window just run, recording the
// hash of the copy at copy time so the control can be checked against what the
// copy HELD rather than against itself.
func prAdvanceSeq(p prPerturb, st *prSeqState, got, copyOut []float64) {
	st.prevSlice = got
	st.prevCopy = copyOut
	if p == prPerturbAliasCopyAliases {
		// The caller mistake the aliasing note warns about: keeping the returned
		// slice instead of copying it. Under this perturbation prevSlice and
		// prevCopy are the SAME array, so the control's hash moves with the buffer
		// and the "changed" comparison compares the array with itself.
		st.prevCopy = got
	}
	st.prevHash = prHashFloats(st.prevCopy)
	st.havePrev = true
}

// prDriveCrossRegime drives the published parallelism claim: the same one-shot
// call at a serial clamp and at several worker counts, required to agree bit for
// bit. It uses the one-shot rather than the PageRanker so a difference cannot be
// blamed on cached state, and the lowest damping so the sweep is cheap.
func prDriveCrossRegime(parent context.Context, cfg *PageRankRankerConfig, fx *prFixture,
	ev *PageRankRankerEvidence,
) error {
	opts := centrality.PageRankOptions{
		Damping: pageRankerDampingLow, MaxIterations: pageRankerMaxIters, Tolerance: pageRankerTolerance,
	}
	var serial []float64
	var serialIters int
	err := prWithClamp(1, func() error {
		r, it, err := centrality.PageRankCtx(parent, fx.c, opts)
		if err != nil {
			return fmt.Errorf("cross-regime serial PageRankCtx: %w", err)
		}
		serial, serialIters = append([]float64(nil), r...), it
		return nil
	})
	if err != nil {
		return err
	}
	ev.CrossRegime = make([]prCrossRegime, 0, len(cfg.CrossRegimeWorkers))
	for _, workers := range cfg.CrossRegimeWorkers {
		row := prCrossRegime{Workers: workers, SerialIters: serialIters, FirstDiff: -1}
		err := prWithClamp(workers, func() error {
			r, it, err := centrality.PageRankCtx(parent, fx.c, opts)
			if err != nil {
				return fmt.Errorf("cross-regime PageRankCtx(workers %d): %w", workers, err)
			}
			parallel := append([]float64(nil), r...)
			if cfg.Perturb == prPerturbCrossRegimeBit {
				parallel = prFlipOneBit(parallel)
			}
			row.Iters = it
			row.BitDiff, row.FirstDiff, row.MaxULP = prCompareBits(parallel, serial)
			return nil
		})
		if err != nil {
			return err
		}
		ev.CrossRegime = append(ev.CrossRegime, row)
	}
	return nil
}

// prFinish derives the sequence aggregates and the digest from the per-window
// evidence.
func prFinish(ev *PageRankRankerEvidence, fx *prFixture, buffers int) {
	ev.FirstParallel = -1
	iters := make(map[int]struct{}, len(ev.Windows))
	for i := range ev.Windows {
		w := &ev.Windows[i]
		if w.ExpectParallel {
			ev.ParallelWindows++
			switch ev.ParallelWindows {
			case 1:
				ev.FirstParallel = i
				ev.FirstParallelAlloc = w.AllocBytes
			case 2:
				ev.SecondParallelAlloc = w.AllocBytes
			}
		} else {
			ev.SerialWindows++
		}
		if w.Converged {
			ev.ConvergedRuns++
		} else {
			ev.CappedRuns++
		}
		if w.PrevDiffered > 0 {
			ev.AliasArmed++
		}
		if w.EmptyRanges > ev.MaxEmptyRanges {
			ev.MaxEmptyRanges = w.EmptyRanges
		}
		iters[w.RunIters] = struct{}{}
	}
	ev.DistinctIters = len(iters)
	ev.DistinctBuffers = buffers
	ev.BufferRepeats = len(ev.Windows) - buffers
	if ev.Perturb == prPerturbFreshBuffers {
		// A Run that allocated a fresh result each call would report one distinct
		// backing array per window and no repeat at all.
		ev.DistinctBuffers = len(ev.Windows)
		ev.BufferRepeats = 0
	}
	ev.TransposeFloor = prTransposeBytes(fx.n, len(fx.edges))
	if ev.Perturb == prPerturbInflateTransposeFloor {
		ev.TransposeFloor *= 1_000_000
	}
}

// prReferenceArm compares the reference window's result with the independent
// power-iteration reference and records the largest deviation and where it was.
func prReferenceArm(ev *PageRankRankerEvidence, fx *prFixture, refCopy []float64, damping float64, p prPerturb) {
	ev.RefWindow = len(ev.Windows) - 1
	ev.RefEpsilon = pageRankerRefEpsilon
	ev.RefIndex = -1
	want := pagerankReference(fx.n, fx.edges, damping)
	if p == prPerturbShiftReference && len(want) > 0 {
		want[len(want)/2] += 10 * pageRankerRefEpsilon
	}
	for i := range want {
		if i >= len(refCopy) {
			break
		}
		d := math.Abs(refCopy[i] - want[i])
		if d > ev.RefMaxDev {
			ev.RefMaxDev, ev.RefIndex = d, i
		}
	}
}

// RunPageRankRanker drives one whole run of the scenario: build the fixture,
// drive the interleaved sequence over ONE PageRanker, drive the cross-regime
// arm, compare the reference window against the independent power iteration, and
// adjudicate.
//
// It returns the evidence in every case, a report when a clause or a gate fired,
// and an error only for a harness failure — which here means a foreign GOMAXPROCS
// clamp landed inside a window, or an entry point returned an unexpected error.
func RunPageRankRanker(ctx context.Context, cfg PageRankRankerConfig) (*PageRankRankerEvidence, *SimReport, error) {
	if cfg.CrossRegimeWorkers == nil {
		cfg.CrossRegimeWorkers = pageRankerCrossRegimeWorkers()
	}
	seed := NewSeed(cfg.Seed ^ pageRankRankerSeedMix)
	fx := prGenFixture(seed)
	plan := cfg.Plan
	if plan == nil {
		plan = prPlan(seed)
	}
	plan = prApplyPlanPerturb(plan, cfg.Perturb)
	if len(plan) < 2 {
		return nil, nil, fmt.Errorf("sim: pagerank-ranker: the plan has %d window(s); the aliasing pin "+
			"needs at least two Runs on one PageRanker", len(plan))
	}

	ev := &PageRankRankerEvidence{
		Windows:   make([]prWindowEvidence, 0, len(plan)),
		Nodes:     fx.n,
		Edges:     len(fx.edges),
		Live:      fx.live,
		Threshold: pageRankerThresholdMirror,
		Hub:       fx.hub,
		HubInDeg:  prInDegree(fx, fx.hub),
		Perturb:   cfg.Perturb,
	}

	// The whole GOMAXPROCS-mutating phase runs under the package-wide exclusive
	// clamp hold, so neither a second instance of this scenario nor any other
	// scenario the swarm dispatches can decide this one's regimes — nor observe
	// the clamped value (rmp #2613). The reference power iteration below is
	// deliberately OUTSIDE it: it is pure arithmetic and holding a process-global
	// lock across it would serialise a swarm for no reason.
	var refCopy []float64
	refDamping := plan[len(plan)-1].Opts.Damping
	err := func() error {
		defer holdGOMAXPROCSExclusive()()
		pr := centrality.NewPageRanker(fx.c)
		st := &prSeqState{}
		for i, w := range plan {
			we, copyOut, err := prDriveWindow(ctx, &cfg, fx, pr, w, st)
			if err != nil {
				return err
			}
			ev.Windows = append(ev.Windows, we)
			if i == len(plan)-1 {
				refCopy, refDamping = copyOut, w.Opts.Damping
			}
		}
		prFinish(ev, fx, len(st.buffers))
		return prDriveCrossRegime(ctx, &cfg, fx, ev)
	}()
	if err != nil {
		return ev, nil, err
	}
	prReferenceArm(ev, fx, refCopy, refDamping, cfg.Perturb)
	ev.Digest = ev.computeDigest()

	v := append(checkPageRankRanker(ev), checkPageRankRankerNonVacuity(ev)...)
	if len(v) == 0 {
		return ev, nil, nil
	}
	return ev, prReport(cfg.Seed, ev, v), nil
}

// prInDegree counts a node's in-edges in the fixture. It exists so the evidence
// can report the hub's skew, which is what makes the derived partition's
// empty-range shape reachable.
func prInDegree(fx *prFixture, node int) int {
	c := 0
	for _, e := range fx.edges {
		if e[1] == node {
			c++
		}
	}
	return c
}

// -----------------------------------------------------------------------------
// Adjudication
// -----------------------------------------------------------------------------

// prOp renders the Op field for a clause, so a report says which clause fired
// without the message having to repeat it.
func prOp(clause string) string { return "<pagerank-ranker:" + clause + ">" }

// prViolation builds one clause's violation.
//
// The KIND split is deliberate. A disagreement between two computations of the
// same rank vector — the two entry points, the two regimes, the library against
// the independent power iteration, or the conservation of total mass — is a
// SEARCH_DIVERGENCE, the kind this package already uses for an algorithm that
// disagrees with its reference. A breach of the type's stated CONTRACT — which
// regime ran, whether the result aliases an internal buffer, whether the buffer
// was invalidated — is an ORACLE_DEVIATION: nothing computed the wrong number,
// the object behaved differently from its documentation.
func prViolation(kind ViolationKind, clause, format string, args ...any) Violation {
	return Violation{Kind: kind, Op: prOp(clause), Message: fmt.Sprintf(format, args...)}
}

// checkPageRankRanker adjudicates the run against every contract clause. It
// returns one violation per clause that fired; an empty result means the run
// satisfied all of them. The non-vacuity gates are separate
// ([checkPageRankRankerNonVacuity]) so "nothing was wrong" and "nothing was
// exercised" are never the same verdict.
func checkPageRankRanker(e *PageRankRankerEvidence) []Violation {
	v := make([]Violation, 0, 8)
	v = append(v, prCheckWindows(e)...)
	v = append(v, prCheckAliasing(e)...)
	v = append(v, prCheckSequence(e)...)
	v = append(v, prCheckReference(e)...)
	v = append(v, prCheckCrossRegime(e)...)
	return v
}

// prCheckWindows holds each window's Run to the one-shot beside it, to the
// regime the inputs imply, and to mass conservation.
func prCheckWindows(e *PageRankRankerEvidence) []Violation {
	var v []Violation
	for i := range e.Windows {
		w := &e.Windows[i]
		if w.BitDiff != 0 {
			v = append(v, prViolation(ViolationSearchDivergence, "bit-identity",
				"window %d (clamp %d, damping %.6f): PageRanker.Run differs from the one-shot "+
					"PageRankCtx in %d of %d rank bit patterns, first at index %d, max ULP distance %d; "+
					"Run's godoc claims bit-for-bit identity",
				i, w.Clamp, w.Damping, w.BitDiff, e.Nodes, w.FirstDiff, w.MaxULP))
		}
		if w.RunIters != w.OneShotIters {
			v = append(v, prViolation(ViolationSearchDivergence, "iteration-parity",
				"window %d (clamp %d, damping %.6f): Run converged in %d iterations and the one-shot "+
					"in %d over the same fixture and options",
				i, w.Clamp, w.Damping, w.RunIters, w.OneShotIters))
		}
		v = append(v, prCheckWindowRegime(e, i, w)...)
		if w.MassErr > pageRankerMassEpsilon {
			v = append(v, prViolation(ViolationSearchDivergence, "mass",
				"window %d: the rank vector sums to 1%+.3e, outside %.0e; PageRank redistributes "+
					"dangling mass and must conserve it", i, w.MassErr, pageRankerMassEpsilon))
		}
	}
	return v
}

// prCheckWindowRegime holds the observed worker-spawn count to the regime the
// derivation implies. See the file header for why the parallel side is a band.
func prCheckWindowRegime(e *PageRankRankerEvidence, i int, w *prWindowEvidence) []Violation {
	var v []Violation
	if w.OtherLookups != 0 {
		v = append(v, prViolation(ViolationOracleDeviation, "label-probe",
			"window %d: %d context lookup(s) arrived under a key that is not %q, so the "+
				"worker-spawn observation can no longer be attributed to runtime/pprof; the regime "+
				"clause beside it is reading an instrument that has changed",
			i, w.OtherLookups, prLabelKeyType))
	}
	switch {
	case !w.ExpectParallel && w.LabelLookups != 0:
		v = append(v, prViolation(ViolationOracleDeviation, "regime",
			"window %d: live=%d and GOMAXPROCS=%d imply the SERIAL push path (threshold %d), which "+
				"creates no worker pool, yet %d pprof label lookup(s) were observed on the Run's context",
			i, e.Live, w.Workers, e.Threshold, w.LabelLookups))
	case w.ExpectParallel && (w.LabelLookups < int64(w.ExpectWorkers) || w.LabelLookups > 2*int64(w.ExpectWorkers)):
		v = append(v, prViolation(ViolationOracleDeviation, "regime",
			"window %d: live=%d and GOMAXPROCS=%d imply the PARALLEL pull path with %d workers "+
				"(threshold %d), so between %d and %d pprof label lookups were expected on the Run's "+
				"context; %d were observed",
			i, e.Live, w.Workers, w.ExpectWorkers, e.Threshold,
			w.ExpectWorkers, 2*w.ExpectWorkers, w.LabelLookups))
	}
	return v
}

// prCheckAliasing pins the documented result-slice hazard in both directions.
//
// The two clauses have different standing, and it is worth being explicit about
// which is which. `alias-invalidated` is a real detector of a LIBRARY change: it
// fires whenever a Run stops overwriting the buffer the previous Run returned.
// `alias-copy-intact` is not — on the unperturbed path it is a theorem, because
// [prAdvanceSeq] copies into a freshly allocated array and hashes it there, and
// nothing writes to that array afterwards. It is kept as the CONTROL for the
// clause beside it, so a harness in which the "copy" stopped being a copy fails
// here loudly instead of leaving `alias-invalidated` comparing an array with
// itself; [prPerturbAliasCopyAliases] drives exactly that state through the
// normal configuration path and [TestPageRankRanker_ClausesFire] requires it to
// fire.
func prCheckAliasing(e *PageRankRankerEvidence) []Violation {
	var v []Violation
	for i := 1; i < len(e.Windows); i++ {
		w := &e.Windows[i]
		if !w.PrevCopyIntact {
			v = append(v, prViolation(ViolationOracleDeviation, "alias-copy-intact",
				"window %d: the COPY taken from window %d's result no longer hashes to what it "+
					"hashed to at copy time, so the control for the aliasing pin is unsound — a copy "+
					"is what the aliasing note tells callers to make", i, i-1))
		}
		if w.PrevDiffered == 0 {
			// Not armed: the two consecutive results are equal, so "the buffer was
			// overwritten" is not observable. The gate below fails a run in which
			// that is true of EVERY window.
			continue
		}
		if w.ChangedFromPrev == 0 {
			v = append(v, prViolation(ViolationOracleDeviation, "alias-invalidated",
				"window %d: not one of the %d elements of window %d's returned SLICE changed after "+
					"this Run, although the two results differ in %d elements; Run's godoc says the "+
					"returned slice aliases an internal buffer and is invalidated by the next Run",
				i, e.Nodes, i-1, w.PrevDiffered))
		}
	}
	return v
}

// prCheckSequence holds the whole-sequence shape: only two backing arrays are
// ever returned, and the first parallel Run really did build the transpose.
func prCheckSequence(e *PageRankRankerEvidence) []Violation {
	var v []Violation
	if e.DistinctBuffers > 2 {
		v = append(v, prViolation(ViolationOracleDeviation, "buffer-recycling",
			"the sequence's %d Run(s) returned %d distinct backing arrays; a PageRanker owns exactly "+
				"two rank vectors and Run's godoc says the result ALIASES one of them, so at most two "+
				"can ever be returned", len(e.Windows), e.DistinctBuffers))
	}
	if e.FirstParallel >= 0 && e.FirstParallelAlloc < e.TransposeFloor {
		v = append(v, prViolation(ViolationOracleDeviation, "transpose-alloc",
			"window %d is the first Run to take the parallel path and allocated %d byte(s), below the "+
				"%d-byte floor the structure-only reverse-CSR transpose alone requires (revVerts "+
				"%d*8 + revEdges %d*%d + cursor %d*8); the lazy build appears not to have happened",
			e.FirstParallel, e.FirstParallelAlloc, e.TransposeFloor,
			e.Nodes+1, e.Edges, prNodeIDBytes, e.Nodes))
	}
	return v
}

// prCheckReference holds the reference window against the independent power
// iteration.
func prCheckReference(e *PageRankRankerEvidence) []Violation {
	if e.RefMaxDev <= e.RefEpsilon {
		return nil
	}
	return []Violation{prViolation(ViolationSearchDivergence, "reference",
		"window %d's rank vector deviates from the independent power-iteration reference by %.3e at "+
			"rank %d, outside the derived epsilon %.0e",
		e.RefWindow, e.RefMaxDev, e.RefIndex, e.RefEpsilon)}
}

// prCheckCrossRegime holds the published parallelism claim: the parallel pull
// path must agree with the serial push path bit for bit, at every worker count.
// A firing here refutes a godoc claim rather than this clause — see the file
// header's finding on the L1-delta reduction order.
func prCheckCrossRegime(e *PageRankRankerEvidence) []Violation {
	var v []Violation
	for i := range e.CrossRegime {
		r := &e.CrossRegime[i]
		if r.BitDiff == 0 && r.Iters == r.SerialIters {
			continue
		}
		v = append(v, prViolation(ViolationSearchDivergence, "cross-regime",
			"the parallel pull path at %d worker(s) differs from the serial push path in %d rank bit "+
				"pattern(s) (first at index %d, max ULP distance %d) and took %d iterations against the "+
				"serial %d; PageRank's godoc claims bit-for-bit identity regardless of GOMAXPROCS",
			r.Workers, r.BitDiff, r.FirstDiff, r.MaxULP, r.Iters, r.SerialIters))
	}
	return v
}

// checkPageRankRankerNonVacuity fails a run that proved nothing.
//
// Every clause above is conditional on the run REACHING the state it adjudicates,
// and each of these gates names one such precondition. A serial-only run, a
// fixture that fell below the parallel threshold, a plan whose windows all
// converge to the same vector — each would leave the clauses silent and the run
// green while nothing was tested.
func checkPageRankRankerNonVacuity(e *PageRankRankerEvidence) []Violation {
	var v []Violation
	if e.Live < e.Threshold {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:live-margin",
			"the fixture has %d live node(s), below the %d-node parallel threshold, so no clamp can "+
				"reach the parallel path at all", e.Live, e.Threshold))
	}
	if e.SerialWindows == 0 || e.ParallelWindows == 0 {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:both-regimes",
			"the sequence reached %d serial and %d parallel window(s); the interleaving this scenario "+
				"exists for needs at least one of each", e.SerialWindows, e.ParallelWindows))
	}
	if e.FirstParallel <= 0 {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:mid-sequence-build",
			"the first parallel window is at index %d; it must be later than the first window, or the "+
				"lazy reverse-CSR transpose is built on the first Run and never MID-sequence, which is "+
				"the state no existing test reaches", e.FirstParallel))
	}
	if e.ConvergedRuns == 0 || e.CappedRuns == 0 {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:convergence-mix",
			"%d window(s) converged and %d hit their iteration cap; both exits from the power "+
				"iteration must be reached, since they leave the reused state in different shapes",
			e.ConvergedRuns, e.CappedRuns))
	}
	if e.AliasArmed == 0 {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:alias-armed",
			"no window produced a result differing from the window before it, so the aliasing pin was "+
				"never armed: \"the buffer was overwritten\" is unfalsifiable when the overwrite would "+
				"write the same values"))
	}
	if e.BufferRepeats == 0 {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:buffer-repeat",
			"%d window(s) returned %d distinct backing array(s) with no repeat, so the recycling "+
				"clause never had a repeat to confirm", len(e.Windows), e.DistinctBuffers))
	}
	if e.DistinctIters < 2 {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:iteration-spread",
			"every window took the same iteration count (%d distinct), so the varying options never "+
				"varied the trajectory", e.DistinctIters))
	}
	if e.MaxEmptyRanges == 0 {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:empty-range",
			"the derived edge-balanced partition left no worker an empty vertex range at any clamp, so "+
				"the hub's skew no longer reaches the collapsed-boundary shape edgeBalancedBounds "+
				"documents (hub %d holds %d of %d in-edges)", e.Hub, e.HubInDeg, e.Edges))
	}
	if len(e.CrossRegime) == 0 {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:cross-regime-rows",
			"the cross-regime arm compared nothing"))
	}
	if e.RefWindow < 0 || e.RefWindow >= len(e.Windows) || !e.Windows[e.RefWindow].Converged {
		v = append(v, prViolation(ViolationOracleDeviation, "gate:reference-converged",
			"the reference-compared window (index %d of %d) did not converge, so its rank vector is a "+
				"mid-trajectory iterate and comparing it with a near-exact fixpoint would fail correct "+
				"code", e.RefWindow, len(e.Windows)))
	}
	return v
}

// -----------------------------------------------------------------------------
// Catalogue wiring
// -----------------------------------------------------------------------------

// prReport wraps violations in a scenario report. It PANICS on an empty
// violation slice: a non-nil report that names nothing is a reporting defect,
// which SimReport.String shouts about and report_render_test pins.
func prReport(seed uint64, ev *PageRankRankerEvidence, v []Violation) *SimReport {
	if len(v) == 0 {
		panic("sim: prReport called with no violations; a report must always name what it found")
	}
	if len(v) > pageRankerReportCap {
		v = v[:pageRankerReportCap]
	}
	return &SimReport{
		Scenario:   ScenarioPageRankRanker,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<pagerank-ranker: " + ev.String() + ">"},
		Violations: v,
	}
}

// pageRankRankerScenario builds the catalogue entry.
//
// Mode is [ModeDeterministic] with a run override, and both halves are
// deliberate. The run IS bit-reproducible from the seed — the fixture, the plan,
// every rank vector and every comparison outcome are pure functions of it, and
// the two quantities that are not (the label-lookup position inside its band and
// the process-global allocation deltas) are excluded from the digest and from
// [PageRankRankerEvidence.ReproducibleSummary] — so declaring anything weaker
// would stop the CLI recording and replaying a failure it can in fact reproduce.
// The override is mandatory rather than stylistic: a ModeDeterministic scenario
// without one dispatches to runDeterministic, which drives the engine's Cypher
// surface through a shadow oracle, and this scenario has no engine, no store and
// no Cypher at all — it drives an in-process library API over an immutable CSR.
// generation-swap and readtx-isolation use the same pairing for the same reason.
func pageRankRankerScenario() Scenario {
	return Scenario{
		Name: ScenarioPageRankRanker,
		Description: "the stateful PageRanker across an interleaved serial/parallel sequence: every Run " +
			"held bit-for-bit to the one-shot PageRankCtx beside it under the same clamp, the regime " +
			"each Run took established from the worker pool's own pprof label lookups rather than " +
			"assumed, the lazy reverse-CSR transpose built MID-sequence and held to its allocation " +
			"floor, and the result-slice aliasing contract pinned in both directions — the previous " +
			"slice must read the new run's values while a copy of it must not, gated on the two runs " +
			"actually differing — plus the whole-sequence shape that only two backing arrays are ever " +
			"returned, one window adjudicated against the independent power-iteration reference, and " +
			"the published serial-versus-parallel bit-identity claim swept across four worker counts",
		Mode:             ModeDeterministic,
		DefaultSeed:      pageRankRankerDefaultSeed,
		ClampsGOMAXPROCS: true,
		run:              runPageRankRankerScenario,
	}
}

// runPageRankRankerScenario is the scenario's run override: drive the run, then
// attach the measured evidence to whatever report came back so an operator
// reading only the log sees what the run exercised.
func runPageRankRankerScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	_, report, err := RunPageRankRanker(ctx, DefaultPageRankRankerConfig(seed))
	if err != nil {
		return nil, err
	}
	return report, nil
}
