package sim

// search_ctx_cancel.go — a context-cancellation battery over EVERY public
// context-accepting entry point of the search, search/centrality,
// search/community, search/flow and search/extern packages (rmp #2489).
//
// Before this file the DST called exactly ONE of them — [search.FloydWarshallCtx],
// from [negWeightCycleCheck], and only because the non-Ctx form cannot surface
// [search.ErrNegativeCycle] in its signature. Nothing at the DST layer asserted
// that a cancelled context is honoured, that the honouring is visible in the
// result, or that the cancellable form computes what the plain form computes.
//
// # The inventory, and why it is not a name pattern
//
// There are 58 such entry points, not the "about 35" the gap was filed as and
// not the 53 a `...Ctx` name search finds. Enumerating on the NAME misses five,
// and they are not marginal:
//
//   - [search.KShortestPathsLooplessCtxWithOpts] is THE implementation. Both
//     [search.KShortestPathsLooplessCtx] and the deprecated
//     [search.EppsteinKShortestCtx] delegate to it, and it is the only one of the
//     three that can return a partial result beside its error.
//   - [centrality.PageRanker.Run] is a METHOD, and one of only two genuinely
//     independent implementations in the whole surface (see below).
//   - [search.AStarInto], [search.BellmanFordInto] and [search.DijkstraInto] are
//     the zero-allocation primitives, each taking a context as its first
//     parameter and each with no non-Ctx sibling.
//
// [TestSearchCtxCancel_TableCoversEveryEntryPoint] therefore enumerates on the
// PARAMETER TYPE — exported function, or method on an exported type, whose first
// parameter is a [context.Context] — and fails when the parsed set and the table
// below disagree in either direction. A hand-maintained list rots the moment the
// API grows; that test is the whole basis of the coverage claim.
//
// One more name-shaped assumption fails for a different reason:
// [search.DiameterCtx] spawns worker goroutines without carrying "Parallel" in
// its name, so scoping the teardown arm by name would have missed it. Nine rows
// spawn goroutines and the table marks each of them explicitly rather than
// inferring it.
//
// # The two regimes this file asserts
//
// Every row is driven under [context.Background] and under a context cancelled
// BEFORE the call. Five clauses:
//
//	bg-err      the entry point must not error under context.Background() — every
//	            fixture is built to be a valid input for the row it feeds
//	twin-err    the identity counterpart, where its signature can carry an error,
//	            must not error either
//	identity    the two must produce byte-identical output (see the vacuity
//	            section: for 52 of the 54 rows that have a counterpart this is
//	            guaranteed by construction, and this file says so rather than
//	            dressing it up)
//	cancel-err  the pre-cancelled call must return an error satisfying
//	            errors.Is(err, context.Canceled)
//	cancel-val  ...and must NOT return the values the uncancelled call returned
//
// A third arm, [ctxCancelPrecedenceViolations], covers a shape the row loop
// structurally cannot: an input that makes the entry point return a TERMINAL
// sentinel. The row loop's fixtures are valid inputs by construction — they have
// to be, or bg-err would mean nothing — so an entry point that decides the input
// is unusable before it consults its context is invisible to it. Two clauses:
//
//	prec-setup  under an uncancelled context the call must return the row's
//	            sentinel, proving the input really does reach the terminal path
//	prec-order  under a pre-cancelled context it must return the CONTEXT error
//	            instead, i.e. cancellation outranks the sentinel
//
// Two mechanics in those clauses are load-bearing and were both nearly got
// wrong:
//
//   - `errors.Is`, NEVER `==`. Four centrality entry points wrap the context
//     error ([centrality.ClosenessCtx], [centrality.HarmonicCtx],
//     [centrality.EigenvectorCtx], [centrality.KatzCtx]); the other 54 return it
//     raw. An identity comparison would pass 54 rows and fail exactly those four.
//     Note also that roughly forty godoc clauses across these packages claim
//     "wrapped ctx.Err()" and do NOT wrap: build assertions from the poll site,
//     never from the doc.
//
//   - ERROR IDENTITY, never non-nilness. `err != nil` is not the property under
//     test. [search.ErrCycle], [search.ErrNegativeCycle], `ErrNoPath` and
//     [search.ErrInvalidInput] are all non-nil and none of them is a
//     cancellation. The precedent in this repo shows how that goes wrong:
//     `search/centrality/security_brandes_goroutine_bound_test.go` accepts
//     `err != nil && !errors.Is(err, context.Canceled)` as its condition, which a
//     nil error satisfies — an oracle that cannot fail.
//
// cancel-val is the non-vacuity gate, and it is not decoration. Without it a row
// whose answer happens to be the zero value would pass cancel-err while proving
// nothing, and an entry point that ignored its context on a degenerate fixture
// would look compliant. It is why every fixture in [ctxFixtures] is constructed
// to have a NON-ZERO answer.
//
// # Determinism
//
// Every fixture is a pure function of the tick through a single [Seed] draw
// stream salted with [ctxCancelSalt], so the family's draws never correlate with
// another checker's for the same tick. There is no wall clock, no global rand and
// no map iteration in any output path: each digest walks dense indices in
// ascending order and renders floats with strconv's 'x' verb, so "identical"
// means bit-identical rather than identical to some printed precision. The
// temporary csrfile the extern rows need lives under a fresh directory whose PATH
// never enters a digest or a message.
//
// The flow rows build a FRESH [flow.Network] per call, which is not tidiness:
// [flow.MaxFlowCtx], [flow.EdmondsKarpCtx], [flow.MinCostMaxFlowCtx] and
// [flow.PushRelabelMaxFlowCtx] all leave the caller's network mutated after a
// cancelled call. Reusing one network across the uncancelled and the cancelled
// arm would compute a wrong flow on the second and read as an engine bug.
// [flow.StoerWagnerCtx] is the only flow entry point that copies its input.
// The three `*Into` rows allocate fresh caller-owned buffers per call for the
// same reason (see [ctxIntoBuffers]).
//
// # What the identity arm proves, and where it is vacuous
//
// The task this file implements was written on the premise that the `Ctx` form
// might be "a divergent code path". Reading the five packages inverts that. Of
// the 58 rows, 54 have a counterpart to compare against at all (the three
// `*Into` primitives and [search.KShortestPathsLooplessCtxWithOpts] have none),
// and for 52 of those 54 it is the NON-Ctx form that is the wrapper — its whole
// body is a tail-call to the context-aware form with [context.Background] — so a
// bitwise difference is impossible by construction and the arm is a structural
// tripwire, not a correctness oracle. This file labels it as such rather than
// counting it as coverage.
// [TestSearchCtxCancel_TwinIsAStructuralDelegation] checks the label in BOTH
// directions against the source, so neither a twin that stops delegating nor one
// that starts can leave the label stale.
//
// Two rows carry a REAL identity arm:
//
//   - [centrality.PageRanker.Run] against [centrality.PageRankCtx]. These are two
//     genuinely separate implementations, duplicated on purpose: PageRankCtx is
//     kept as one monolithic function because extracting it behind the PageRanker
//     method boundary was measured to regress the parallel SpMV by ~3%. Their
//     godoc claims the two are bit-for-bit identical. This is the one place in
//     the whole surface where an identity arm can catch real drift, and it is the
//     clause that tests that claim.
//   - [search.BFSDirectionOptCtx] against [search.BFSDirectionOpt], which does not
//     delegate to the Ctx form: both tail-call the shared unexported `bfsDoCore`,
//     the plain form passing [context.Background].
//
// A third, weaker sense in which the arm has teeth: the six concurrent-reduction
// rows (the `*Parallel*Ctx` set) run the same code twice, but two invocations are
// two independent work partitions. Delegation makes the code identical and says
// nothing about whether a partitioned float accumulation is reproducible run to
// run, so for those rows the comparison is a determinism check. It has held on
// every tick tried.
//
// Finally, 27 of the 54 counterparts CANNOT report an error at all —
// [search.FloydWarshall] discards [search.ErrNegativeCycle] exactly this way,
// which is why the DST had to reach for FloydWarshallCtx to see that sentinel.
// For those rows the identity arm compares values only, and the swallowing itself
// is pinned by [TestSearchCtxCancel_TwinErrorArityMatchesSource].
//
// # The sub-stride blindness this battery found, and the one still open
//
// When first written, seven of the 58 rows FAILED both pre-cancelled clauses at
// every tick tried: [search.BellmanFordCtx], [search.BellmanFordInto],
// [search.KCoreCtx], [search.KShortestPathsLooplessCtx],
// [search.KShortestPathsLooplessCtxWithOpts], the deprecated
// [search.EppsteinKShortestCtx] that delegates to it, and
// [flow.PushRelabelMaxFlowCtx]. All four underlying cores incremented their work
// counter BEFORE masking it (`c++; if c&0xFFF == 0`), so the first ctx.Err() poll
// happened only once 4096 units of work had been done and any input below that
// stride returned the complete answer with a nil error under a dead context. Those
// sites were corrected under rmp #2593 (check-then-increment, matching
// `search/dijkstra.go` and `search/prim.go`) and all seven rows now assert the
// full contract.
//
// A SIXTH site of the same defect survived that first pass and this battery found
// it: cancellation did not outrank [search.ErrNegativeCycle] in
// [search.JohnsonAPSPCtx] or [search.JohnsonAPSPParallelCtx], because
// `bellmanFordVirtualSource` (`search/johnson.go`) incremented before masking and
// Johnson runs that reweighting prologue BEFORE its per-source poll. They were 2
// of 9 APSP / shortest-path entry points to get it wrong; their four siblings —
// [search.BellmanFordCtx], [search.FloydWarshallCtx],
// [search.FloydWarshallParallelCtx] and [search.DijkstraAPSPCtx] — already
// returned context.Canceled on the same fixture. #2593 was extended to that site
// and to both Johnson godocs, which had described SPFA-on-a-deque as "every
// relaxation-round boundary" and promised a wrapped ctx.Err() that never wrapped.
//
// Note WHY the main row loop could not see it, because the same blind spot will
// recur: that loop's fixtures are valid inputs by construction — they must be, or
// bg-err would mean nothing — and Johnson does poll before its per-source loop, so
// on a valid graph it looked compliant. The defect lived only on the path where a
// prologue decides the input is unusable before the entry point consults its
// context. [ctxCancelPrecedenceViolations] is the arm for that shape, and both
// Johnson rows are now in it as the regression guard.
//
// The precedence arm's TopologicalSortCtx row overlaps
// `search/ctx_cancel_entry_test.go`'s `TestTopologicalSortCtx_CancelBeatsCycle`,
// deliberately. What the DST layer adds is not a second opinion on that one entry
// point: it is that the ordering is re-checked on every search-battery
// invocation — including after every crash and WAL recovery — across seven entry
// points, behind one shared non-vacuity gate.
//
// # Regimes this family does NOT reach, and why
//
// Read this before assuming the context surface is fully certified.
//
//   - NO MID-RUN CANCELLATION, AND THEREFORE NO PROMPTNESS CLAIM. Both regimes
//     cancel before the call. Neither says anything about WHERE inside a running
//     algorithm the poll happens, nor how much work can still be done after
//     cancellation is signalled. A promptness assertion phrased as elapsed time,
//     visited-node count, or how many events land in a fixed window would be a
//     threshold on a quantity the RUNTIME decides, which this package has removed
//     three times already (rmp #2587, #2589, #2591).
//
//     Worse, the obvious deterministic mechanism does not work here. The
//     in-package harnesses `cancelAfterFirstCheck`
//     (`search/dijkstra_ctx_cancel_test.go`) and `cancelAfterNCalls`
//     (`search/ctx_cancel_inner_test.go`) override only `Err()` over an embedded
//     [context.Background]. Four of the parallel entry points derive their own
//     child context with [context.WithCancel] and poll the CHILD; because
//     `Background().Done()` is nil the child is never linked to the parent, and a
//     `cancelCtx`'s Err() never consults its parent — so the fake's Err() is
//     never called at all. Measured: those four return a complete result with a
//     nil error under that fake. A counted mid-run arm built on it would assert
//     Canceled and receive nil, and would have been a clause that cannot fail.
//
//   - THE TEARDOWN ARM PROVES NO LEAK, NOT PROMPT JOINING. Nine rows start
//     goroutines. [TestSearchCtxCancel_NoGoroutineLeak] drives them all under
//     both regimes inside [goleak], which proves the pools do not leak
//     indefinitely. It does NOT prove they are joined before the call returns:
//     goleak samples the goroutine set at one instant and retries briefly, so a
//     worker that exits slightly late still passes. A goroutine-COUNT comparison
//     is not the fix — the count is runtime-decided and flakes in both directions.
//
//     That gap is not hypothetical: `pageRankEngine.close()`
//     (`search/centrality/pagerank.go`) does not join its workers, and was
//     measured returning with up to GOMAXPROCS still-live goroutines on 39 of 40
//     runs. goleak tolerates that correctly — a late exit is not a leak — which is
//     exactly why it is the right instrument for "does not leak" and the wrong one
//     for "is joined before returning". A goroutine-count delta was measured to
//     flake in BOTH directions on the same code: 39/40 positive deltas for
//     PageRankCtx, and a false positive on a provably-joined pool.
//
//   - SEVERAL ENTRY POINTS HAVE NO INNER POLL AT ALL, AND THIS FAMILY CANNOT SEE
//     IT. [search.HopcroftKarpCtx] polls per phase and
//     [search.HopcroftTarjanBCCCtx] per DFS root, so on a connected graph the
//     whole O(V+E) traversal is one uninterruptible window; [search.WCCCtx] and
//     [search.WCCParallelCtx] are checkable at two points only, because
//     `wccUnionEdgeRange` takes no context at all; and [centrality.ClosenessCtx]
//     and [centrality.HarmonicCtx] poll every 1024 SOURCES rather than at every
//     source as their godoc says. All six honour a PRE-CALL cancellation, so all
//     six are green here. That, and nothing more, is what this family claims about
//     them.
//
//   - CANCELLATION IS NOT DELIVERED THROUGH Done(). Both regimes use a real
//     [context.WithCancel] context, so Done() closes AND Err() reports. An entry
//     point that selected on Done() and one that polls Err() are
//     indistinguishable to this family.
//
//   - THE FIXTURES ARE SMALL BY DESIGN, WHICH BOUNDS WHAT THE ARMS SEE. Each is a
//     few tens of nodes, so the whole 58-way sweep costs ~10-25 ms and runs on the
//     search battery's cadence. That is enough for the pre-cancelled regime
//     wherever the entry point polls before doing work, which after rmp #2593 is
//     all 58. A poll that exists only behind a 4096-unit stride is still not
//     reached, and reaching it is a mid-run question, not a pre-cancelled one.
//     Sizing up is not free either:
//     [search.FloydWarshallCtx] alone costs ~9.6 s at MaxNodeID 4352.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
	"github.com/FlavioCFOliveira/GoGraph/search/community"
	"github.com/FlavioCFOliveira/GoGraph/search/extern"
	"github.com/FlavioCFOliveira/GoGraph/search/flow"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// ctxCancelSalt is XORed with the tick to derive this family's seed, keeping its
// draw stream disjoint from every other per-tick search checker.
const ctxCancelSalt uint64 = 0xc7a2_ce11_5a1d_0f19

// ctxCancelWorkers is the worker count handed to every `*Parallel*Ctx` entry
// point. It is a fixed constant rather than 0 (which several of them read as
// GOMAXPROCS) so the partitioning — and therefore the reduction order the
// identity arm compares — is a property of the fixture and not of the host.
const ctxCancelWorkers = 4

// ctxCancelDigestSample bounds how much of a digest a mismatch message quotes,
// so a wholesale divergence cannot produce an unbounded report.
const ctxCancelDigestSample = 240

// ctxFamily names the package a `...Ctx` entry point belongs to. It exists so
// the report can say which of the five families a divergence came from without
// parsing the entry point's name.
type ctxFamily string

// The five families the task names.
const (
	famSearch     ctxFamily = "search"
	famCentrality ctxFamily = "search/centrality"
	famCommunity  ctxFamily = "search/community"
	famFlow       ctxFamily = "search/flow"
	famExtern     ctxFamily = "search/extern"
)

// ctxEntry describes ONE public context-accepting entry point and how to drive
// it.
//
// Name is the qualified Go name as it appears in the package (`BFSCtx`,
// `SSSP.FromCtx`, `PageRanker.Run`), NOT prefixed by the package: Family carries
// that. [ctxEntryKey] joins the two into the key the coverage tripwire compares
// against the parsed source.
//
// Note that "context-accepting" is deliberately wider than "named `...Ctx`":
// five of the entry points below are not, and one of them
// ([centrality.PageRanker.Run]) is the SECOND of only two genuinely independent
// implementations in the whole surface. A name-pattern inventory would have
// missed all five — see [TestSearchCtxCancel_TableCoversEveryEntryPoint], which
// enumerates on the parameter type instead.
//
// run invokes the entry point with the supplied context and returns a lossless
// digest of every value it returned, plus the error. It must render the values
// even on the error path, so the pre-cancelled arm can compare them against the
// Background run's.
//
// twin invokes the counterpart the identity arm compares against — usually the
// non-Ctx sibling, and for [centrality.PageRanker.Run] the rival implementation
// [centrality.PageRankCtx]. It is nil when the entry point has no counterpart at
// all (the three `*Into` primitives and
// [search.KShortestPathsLooplessCtxWithOpts]), in which case the identity arm is
// skipped for that row rather than faked.
//
// twinDecl names the counterpart's SOURCE-LEVEL declaration, for the tripwires
// that parse it. It may be left empty when the counterpart's name is Name with
// the trailing "Ctx" removed, which covers every row but
// [centrality.PageRanker.Run], whose counterpart is [centrality.PageRankCtx].
//
// twinReportsErr records whether the twin's signature can carry an error at all.
// When it cannot, the twin SWALLOWS whatever the Ctx form would have returned —
// [search.FloydWarshall] discards [search.ErrNegativeCycle] this way — so the
// identity arm compares values only, and the swallowing itself is the property
// [TestSearchCtxCancel_TwinErrorArityMatchesSource] pins.
//
// twinIsDelegation records that the twin reaches its result by running the SAME
// code with a background context, which makes the identity arm harness-
// guaranteed rather than a correctness oracle.
// [TestSearchCtxCancel_TwinIsAStructuralDelegation] is what keeps that label
// honest.
//
// concurrentReduction marks the entry points whose two identity-arm invocations
// are two INDEPENDENT concurrent reductions. For those the arm is load-bearing
// whatever twinIsDelegation says, because delegation makes the code identical
// and says nothing about whether a work-partitioned float accumulation is
// reproducible run to run.
//
// spawns marks the entry points that start goroutines, and is what scopes the
// goleak teardown arm. It is NOT derivable from the name: [search.DiameterCtx]
// fans out across workers without carrying `Parallel` in its name.

type ctxEntry struct {
	run                 func(ctx context.Context, f *ctxFixtures) (string, error)
	twin                func(f *ctxFixtures) (string, error)
	Name                string
	Family              ctxFamily
	twinDecl            string
	twinReportsErr      bool
	twinIsDelegation    bool
	concurrentReduction bool
	spawns              bool
}

// ctxEntryKey is the "family.Name" key the coverage tripwire matches against the
// exported `...Ctx` declarations parsed out of the five packages.
func ctxEntryKey(e *ctxEntry) string { return string(e.Family) + "." + e.Name }

// ---------------------------------------------------------------------------
// Digest helpers.
//
// Every digest is a plain string built by walking dense indices in ascending
// order, so it is a pure function of the values and never of map order. Floats
// are rendered with strconv's 'x' verb — the exact hexadecimal form — so
// "identical" means bit-identical rather than identical to some printed
// precision.
// ---------------------------------------------------------------------------

// ctxF renders one float64 exactly.
func ctxF(v float64) string { return strconv.FormatFloat(v, 'x', -1, 64) }

// ctxFloats renders a float64 slice, distinguishing nil from empty.
func ctxFloats(v []float64) string {
	if v == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(ctxF(x))
	}
	b.WriteByte(']')
	return b.String()
}

// ctxInts renders an int slice, distinguishing nil from empty.
func ctxInts(v []int) string {
	if v == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(x))
	}
	b.WriteByte(']')
	return b.String()
}

// ctxInt64s renders an int64 slice, distinguishing nil from empty.
func ctxInt64s(v []int64) string {
	if v == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(x, 10))
	}
	b.WriteByte(']')
	return b.String()
}

// ctxIDs renders a NodeID slice, distinguishing nil from empty.
func ctxIDs(v []graph.NodeID) string {
	if v == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	}
	b.WriteByte(']')
	return b.String()
}

// ctxDistances renders a [search.Distances] over the whole NodeID universe of c
// (source, then per-node (distance, reachable)). A nil result renders as "nil",
// so "cancelled" and "computed" can never collide.
func ctxDistances(d *search.Distances[float64], maxID uint64) string {
	if d == nil {
		return "nil"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "src=%d{", uint64(d.Source()))
	for id := uint64(0); id < maxID; id++ {
		w, ok := d.Distance(graph.NodeID(id))
		if ok {
			b.WriteString(ctxF(w))
		} else {
			b.WriteByte('-')
		}
		b.WriteByte(';')
	}
	b.WriteByte('}')
	return b.String()
}

// ctxAPSP renders a [search.APSP] over every ordered pair in the NodeID
// universe. maxID is small by fixture design, so the quadratic rendering is
// bounded and cheap.
func ctxAPSP(a *search.APSP[float64], maxID uint64) string {
	if a == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := uint64(0); i < maxID; i++ {
		for j := uint64(0); j < maxID; j++ {
			w, ok := a.At(graph.NodeID(i), graph.NodeID(j))
			if ok {
				b.WriteString(ctxF(w))
			} else {
				b.WriteByte('-')
			}
			b.WriteByte(';')
		}
	}
	b.WriteByte('}')
	return b.String()
}

// ctxTC renders a [search.TC] as the full reachability bitmap over the NodeID
// universe.
func ctxTC(t *search.TC, maxID uint64) string {
	if t == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := uint64(0); i < maxID; i++ {
		for j := uint64(0); j < maxID; j++ {
			if t.Reachable(graph.NodeID(i), graph.NodeID(j)) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
	}
	b.WriteByte('}')
	return b.String()
}

// ctxYenPaths renders a k-shortest-path result: cost and node sequence per path.
func ctxYenPaths(p []search.YenPath[float64]) string {
	if p == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i := range p {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(ctxF(p[i].Cost))
		b.WriteByte(':')
		b.WriteString(ctxIDs(p[i].Nodes))
	}
	b.WriteByte(']')
	return b.String()
}

// ctxComponents renders a component list (Tarjan SCC, BCC) losslessly, in the
// order the algorithm returned it — the order is part of the output being
// compared, so it is deliberately NOT canonicalised.
func ctxComponents(cs [][]graph.NodeID) string {
	if cs == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i := range cs {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(ctxIDs(cs[i]))
	}
	b.WriteByte(']')
	return b.String()
}

// ctxVisitTrace collects the (node, depth) pairs a traversal's visit callback
// received, in call order. It is the ONLY observable output of [search.BFSCtx],
// [search.DFSCtx], [search.BFSDirectionOptCtx] and [extern.BFSCtx], so it is
// what their digests compare — and what makes their pre-cancelled arm
// non-vacuous, because a cancelled traversal must produce a shorter trace.
type ctxVisitTrace struct {
	b strings.Builder
}

func (t *ctxVisitTrace) visit(node graph.NodeID, depth int) bool {
	t.b.WriteString(strconv.FormatUint(uint64(node), 10))
	t.b.WriteByte(':')
	t.b.WriteString(strconv.Itoa(depth))
	t.b.WriteByte(';')
	return true
}

func (t *ctxVisitTrace) String() string { return t.b.String() }

// ctxSample truncates a digest for a violation message.
func ctxSample(s string) string {
	if len(s) <= ctxCancelDigestSample {
		return s
	}
	return s[:ctxCancelDigestSample] + fmt.Sprintf("...(+%d bytes)", len(s)-ctxCancelDigestSample)
}

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

// Fixture size bounds. Every one is small and fixed: the digests of the
// all-pairs entry points are quadratic in the node count, and the whole 58-way
// sweep runs on the search battery's cadence, so the fixtures are sized for
// cheapness and for guaranteeing a NON-DEGENERATE answer (see [ctxFixtures]),
// not for reaching size-gated inner poll strides.
const (
	ctxFixMinN = 10
	ctxFixMaxN = 16
	// ctxFixWeightMax bounds the strictly positive integer edge weight the
	// weighted fixtures emit. Positive rather than non-negative so that a
	// shortest-path answer, an MST total and a weighted-betweenness score are
	// all non-zero, which is what makes the pre-cancelled arm's value clause
	// able to fail.
	ctxFixWeightMax = 9
)

// ctxFixtures is the complete, deterministic input set the 58 entry points are
// driven over. It is built once per family invocation by [newCtxFixtures].
//
// Every graph fixture is chosen so that the entry points reading it return a
// NON-ZERO answer: dir is strongly connected (so every shortest path exists and
// every SCC is the whole graph), sym is connected and holds triangles (so
// triangle counts, coreness, betweenness and community structure are all
// non-trivial), dag is a spanning DAG (so a topological order exists), the Euler
// fixtures admit a circuit, the bipartite fixture admits at least one matched
// pair, and the flow fixtures carry a positive-capacity spine. That is a
// precondition of the pre-cancelled arm's value clause, which compares the
// cancelled result against the Background one: on a fixture whose answer is the
// zero value the two would coincide and the clause could not fail.
//
// The immutable CSRs are shared across calls. The flow inputs are kept as
// (n, edges) generator output rather than built networks because the max-flow
// routines mutate residual capacities in place, so each call builds its own.
type ctxFixtures struct {
	dir    *csr.CSR[float64]
	dirRev *csr.CSR[float64]
	sssp   *search.SSSP[float64]
	sym    *csr.CSR[float64]
	dag    *csr.CSR[float64]

	eulerDir   *csr.CSR[float64]
	eulerUndir *csr.CSR[float64]

	bip *csr.CSR[float64]

	reader *csrfile.Reader

	hungCost  []float64
	flowEdges []flowEdge
	costEdges []flowCostEdge
	swWeights []int

	dirMax   uint64
	symMax   uint64
	dagMax   uint64
	eulerMax uint64
	eundMax  uint64
	bipMax   uint64

	dirSrc, dirDst graph.NodeID
	bipLeft        int
	hungN          int
	flowN          int
	costN          int
	swN            int
}

// newCtxFixtures derives the whole fixture set from tick. It returns the
// fixtures, a cleanup that must always be called, and any violation raised by a
// harness failure.
//
// # The temp directory is removed on EVERY exit path
//
// The extern rows need a real mmap-backed [csrfile.Reader], and [csrfile.Open]
// binds the OS backend, so this is the one fixture that cannot live on a
// [SimDisk]: it goes to `os.MkdirTemp("", "sim-ctxcancel-*")`, registered in
// `internal/tmphygiene`'s ownedTempPrefixes as its own non-nesting prefix. Since
// the battery runs on the search battery's cadence — periodically, after every
// crash+recovery, and terminally, across every seed of every search-scenario run —
// a directory leaked once is a directory leaked thousands of times, which is
// exactly the accumulation rmp #2527/#2586 exist to catch. Every path out of this
// function is therefore accounted for:
//
//	NewSSSP fails            returns BEFORE MkdirTemp; no directory exists yet
//	MkdirTemp fails          no directory was created
//	csrfile.WriteToFile fails cleanup() is called here, then a no-op is returned
//	csrfile.Open fails       cleanup() is called here, then a no-op is returned
//	success                  the caller's `defer cleanup()` owns it — all three
//	                         call sites (the driver, and two tests) defer it on
//	                         the line after the error check
//
// cleanup closes the Reader BEFORE removing the tree, so the mmap is unmapped
// first, and it is safe to call with a nil Reader (the two failure paths above).
// On a non-nil violation slice the fixtures are unusable and cleanup has already
// run, so the returned no-op must not be relied on for anything.
func newCtxFixtures(tick int64) (*ctxFixtures, func(), []Violation) {
	seed := NewSeed(uint64(tick) ^ ctxCancelSalt)
	f := &ctxFixtures{}

	// --- dir: strongly connected, positive weights, src=0 dst=n-1 -----------
	dirN := ctxFixMinN + seed.IntN(ctxFixMaxN-ctxFixMinN+1)
	dirEdges := make([][2]int, 0, dirN*2)
	dirW := make([]float64, 0, dirN*2)
	addDir := func(u, v int) {
		dirEdges = append(dirEdges, [2]int{u, v})
		dirW = append(dirW, float64(1+seed.IntN(ctxFixWeightMax)))
	}
	for i := 0; i < dirN-1; i++ {
		addDir(i, i+1)
	}
	// The single back edge makes the graph strongly connected, so TarjanSCC
	// returns one component spanning every node and every APSP entry is finite.
	addDir(dirN-1, 0)
	// Seed-chosen forward chords, at most one per ordered pair, so the answer
	// varies tick to tick while the reachability guarantee above is untouched.
	seenDir := make(map[[2]int]struct{}, dirN*2)
	for u := 0; u < dirN; u++ {
		for v := u + 2; v < dirN; v++ {
			if seed.IntN(3) == 0 {
				if _, dup := seenDir[[2]int{u, v}]; !dup {
					seenDir[[2]int{u, v}] = struct{}{}
					addDir(u, v)
				}
			}
		}
	}
	f.dir = ctxWeightedCSR(dirN, dirEdges, dirW)
	f.dirRev = f.dir.BuildReverse()
	f.dirMax = uint64(f.dir.MaxNodeID())
	f.dirSrc, f.dirDst = 0, graph.NodeID(dirN-1)

	sssp, err := search.NewSSSP(f.dir)
	if err != nil {
		return nil, func() {}, []Violation{ctxCancelDeviate(tick, "search.NewSSSP", fmt.Sprintf("fixture construction failed: %v", err))}
	}
	f.sssp = sssp

	// --- sym: connected simple undirected with triangles --------------------
	symN := ctxFixMinN + seed.IntN(ctxFixMaxN-ctxFixMinN+1)
	symPairs := make([][2]int, 0, symN*3)
	symW := make([]float64, 0, symN*6)
	seenSym := make(map[[2]int]struct{}, symN*3)
	addSym := func(u, v int) {
		if u == v {
			return
		}
		if u > v {
			u, v = v, u
		}
		if _, dup := seenSym[[2]int{u, v}]; dup {
			return
		}
		seenSym[[2]int{u, v}] = struct{}{}
		symPairs = append(symPairs, [2]int{u, v})
	}
	// A spanning cycle keeps the graph connected; the +2 chords plant triangles
	// (i, i+1, i+2) so CountTriangles cannot read zero.
	for i := 0; i < symN; i++ {
		addSym(i, (i+1)%symN)
	}
	for i := 0; i < symN; i++ {
		addSym(i, (i+2)%symN)
	}
	for u := 0; u < symN; u++ {
		for v := u + 3; v < symN; v++ {
			if seed.IntN(4) == 0 {
				addSym(u, v)
			}
		}
	}
	for range symPairs {
		w := float64(1 + seed.IntN(ctxFixWeightMax))
		symW = append(symW, w, w)
	}
	f.sym = ctxSymWeightedCSR(symN, symPairs, symW)
	f.symMax = uint64(f.sym.MaxNodeID())

	// --- dag: spanning DAG for TopologicalSort ------------------------------
	dagN, dagEdges := topoGenDAG(seed)
	dagW := make([]float64, len(dagEdges))
	for i := range dagW {
		dagW[i] = float64(1 + seed.IntN(ctxFixWeightMax))
	}
	f.dag = ctxWeightedCSR(dagN, dagEdges, dagW)
	f.dagMax = uint64(f.dag.MaxNodeID())

	// --- Euler fixtures -----------------------------------------------------
	f.eulerDir = eulerBuildCSR(eulerRandomCycle(seed))
	f.eulerMax = uint64(f.eulerDir.MaxNodeID())
	// An undirected cycle: every degree is 2, so an Eulerian circuit exists.
	eundN := ctxFixMinN + seed.IntN(ctxFixMaxN-ctxFixMinN+1)
	eundPairs := make([][2]int, 0, eundN)
	for i := 0; i < eundN; i++ {
		eundPairs = append(eundPairs, [2]int{i, (i + 1) % eundN})
	}
	f.eulerUndir = symCSRFromEdges(eundN, eundPairs)
	f.eundMax = uint64(f.eulerUndir.MaxNodeID())

	// --- bipartite fixture for HopcroftKarp ---------------------------------
	bipLeft := 3 + seed.IntN(4)
	bipRight := 3 + seed.IntN(4)
	bipAdj := make([][]int, bipLeft)
	total := 0
	for u := 0; u < bipLeft; u++ {
		for r := 0; r < bipRight; r++ {
			// The diagonal is forced so a perfect-on-the-diagonal matching of at
			// least min(bipLeft,bipRight) edges always exists; the rest is
			// seed-chosen.
			if u == r || seed.IntN(3) == 0 {
				bipAdj[u] = append(bipAdj[u], r)
				total++
			}
		}
	}
	f.bip = matchingBuildBipartiteCSR(bipLeft, bipRight, bipAdj, total)
	f.bipLeft = bipLeft
	f.bipMax = uint64(f.bip.MaxNodeID())

	// --- Hungarian cost matrix (square, strictly positive) ------------------
	hungN := 3 + seed.IntN(3)
	cost := make([]float64, hungN*hungN)
	for i := range cost {
		cost[i] = float64(1 + seed.IntN(ctxFixWeightMax))
	}
	f.hungCost, f.hungN = cost, hungN

	// --- flow fixtures ------------------------------------------------------
	f.flowN, f.flowEdges = flowGenNetwork(seed)
	f.costN, f.costEdges = flowGenCostNetwork(seed)
	f.swN, f.swWeights = flowGenWeightMatrix(seed)

	// --- extern: a real csrfile over dir ------------------------------------
	dir, err := os.MkdirTemp("", "sim-ctxcancel-*")
	if err != nil {
		return nil, func() {}, []Violation{ctxCancelDeviate(tick, "extern:tempdir", fmt.Sprintf("MkdirTemp: %v", err))}
	}
	cleanup := func() {
		if f.reader != nil {
			_ = f.reader.Close()
		}
		_ = os.RemoveAll(dir)
	}
	path := filepath.Join(dir, "ctxcancel.csr")
	if _, err := csrfile.WriteToFile(path, f.dir); err != nil {
		cleanup()
		return nil, func() {}, []Violation{ctxCancelDeviate(tick, "extern:write", fmt.Sprintf("WriteToFile: %v", err))}
	}
	r, err := csrfile.Open(path)
	if err != nil {
		cleanup()
		return nil, func() {}, []Violation{ctxCancelDeviate(tick, "extern:open", fmt.Sprintf("Open: %v", err))}
	}
	f.reader = r
	return f, cleanup, nil
}

// ctxWeightedCSR builds a directed CSR[float64] over n dense NodeIDs from a
// directed edge list and a parallel weight slice, in a per-source counting
// order, so the layout is a pure function of the inputs.
func ctxWeightedCSR(n int, edges [][2]int, weights []float64) *csr.CSR[float64] {
	if n == 0 {
		return csr.FromArrays[float64]([]uint64{0}, nil, nil, 0, 0)
	}
	vertices := make([]uint64, n+1)
	for _, e := range edges {
		vertices[e[0]+1]++
	}
	for i := 1; i <= n; i++ {
		vertices[i] += vertices[i-1]
	}
	out := make([]graph.NodeID, len(edges))
	w := make([]float64, len(edges))
	cursor := make([]uint64, n)
	for i, e := range edges {
		pos := vertices[e[0]] + cursor[e[0]]
		out[pos] = graph.NodeID(e[1])
		w[pos] = weights[i]
		cursor[e[0]]++
	}
	return csr.FromArrays(vertices, out, w, uint64(n), uint64(len(edges)))
}

// ctxSymWeightedCSR materialises an UNDIRECTED weighted edge list as a symmetric
// directed CSR: each {u,v} becomes u->v and v->u carrying the same weight. The
// weights slice holds 2 entries per pair (u->v then v->u), which is what the
// caller builds so the two directions provably agree.
func ctxSymWeightedCSR(n int, pairs [][2]int, weights []float64) *csr.CSR[float64] {
	if n == 0 {
		return csr.FromArrays[float64]([]uint64{0}, nil, nil, 0, 0)
	}
	edges := make([][2]int, 0, 2*len(pairs))
	w := make([]float64, 0, 2*len(pairs))
	for i, p := range pairs {
		edges = append(edges, [2]int{p[0], p[1]}, [2]int{p[1], p[0]})
		w = append(w, weights[2*i], weights[2*i+1])
	}
	return ctxWeightedCSR(n, edges, w)
}

// ctxCancelDeviate reports a harness-side failure (fixture construction, I/O)
// rather than an engine divergence.
func ctxCancelDeviate(tick int64, op, msg string) Violation {
	return Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "search:ctxcancel:" + op, Message: msg}
}

// ctxCancelDiverge reports a cancellation-contract divergence for one entry
// point.
func ctxCancelDiverge(tick int64, e *ctxEntry, msg string) Violation {
	return Violation{
		Kind: ViolationSearchDivergence, Tick: tick,
		Op:      "search:ctxcancel:" + ctxEntryKey(e),
		Message: msg,
	}
}

// ctxBools renders a bool slice, distinguishing nil from empty.
func ctxBools(v []bool) string {
	if v == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for _, x := range v {
		if x {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	b.WriteByte(']')
	return b.String()
}

// ctxIDPairs renders a NodeID-pair slice (BCC bridges), distinguishing nil from
// empty.
func ctxIDPairs(v [][2]graph.NodeID) string {
	if v == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d-%d", uint64(v[i][0]), uint64(v[i][1]))
	}
	b.WriteByte(']')
	return b.String()
}

// ctxBCC renders a [search.BCCResult] losslessly.
func ctxBCC(r search.BCCResult) string {
	return "comp=" + ctxComponents(r.Components) +
		" bridges=" + ctxIDPairs(r.Bridges) +
		" artic=" + ctxIDs(r.Articulation)
}

// ctxMSTEdges renders a Kruskal edge list losslessly, in the returned order.
func ctxMSTEdges(es []search.MSTEdge[float64]) string {
	if es == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i := range es {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d-%d@%s", uint64(es[i].From), uint64(es[i].To), ctxF(es[i].Weight))
	}
	b.WriteByte(']')
	return b.String()
}

// ctxZeroH is the admissible zero heuristic A* is driven with: it makes A*
// equivalent to Dijkstra, which keeps the fixture's answer well-defined without
// the family having to reason about heuristic admissibility.
func ctxZeroH(graph.NodeID) float64 { return 0 }

// ctxIntoBuffers allocates the caller-owned dist/parent/found buffers the three
// `*Into` primitives write through. FRESH buffers per call are load-bearing: a
// set shared between the uncancelled and the cancelled invocation would carry
// the completed answer into the cancelled digest, and the cancel-val clause
// would then read as a pass on an entry point that had done nothing.
func ctxIntoBuffers(maxID uint64) (dist []float64, parent []graph.NodeID, found []bool) {
	return make([]float64, maxID), make([]graph.NodeID, maxID), make([]bool, maxID)
}

// ctxEntries returns the table of every public `...Ctx` entry point in the five
// families, together with how to drive it and how to drive its non-Ctx twin.
//
// The table is the family's coverage claim, and
// [TestSearchCtxCancel_TableCoversEveryEntryPoint] is what turns that claim into
// an assertion: it parses the five packages with go/ast and fails if any
// exported `...Ctx` function or exported-receiver method is missing from — or
// absent in — this table. A hand-maintained list would silently rot as the API
// grows; the tripwire is what stops it.
//
// The order is fixed (family, then declaration name ascending) so a report is
// reproducible.
func ctxEntries() []ctxEntry {
	return []ctxEntry{
		// ---------------------------------------------------------------- search
		{
			Name: "AStarCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, cost, err := search.AStarCtx(ctx, f.dir, f.dirSrc, f.dirDst, ctxZeroH)
				return ctxIDs(p) + "/" + ctxF(cost), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				p, cost, err := search.AStar(f.dir, f.dirSrc, f.dirDst, ctxZeroH)
				return ctxIDs(p) + "/" + ctxF(cost), err
			},
		},
		{
			// AStarInto is the zero-allocation primitive behind AStarCtx: the caller
			// owns dist/parent/found and the path buffer. It has NO non-Ctx sibling,
			// so the identity arm is skipped (twin == nil) rather than invented.
			// Fresh buffers per call are mandatory — a shared set would let the
			// uncancelled run's contents leak into the cancelled run's digest and
			// make the cancel-val clause read as a pass.
			Name: "AStarInto", Family: famSearch,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				dist, parent, found := ctxIntoBuffers(f.dirMax)
				var path []graph.NodeID
				cost, err := search.AStarInto(ctx, f.dir, f.dirSrc, f.dirDst, ctxZeroH, dist, parent, found, &path)
				return ctxF(cost) + "/" + ctxIDs(path) + "/" + ctxFloats(dist) + "/" + ctxIDs(parent) + "/" + ctxBools(found), err
			},
		},
		{
			Name: "BFSCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				var t ctxVisitTrace
				err := search.BFSCtx(ctx, f.dir, f.dirSrc, t.visit)
				return t.String(), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				var t ctxVisitTrace
				search.BFS(f.dir, f.dirSrc, t.visit)
				return t.String(), nil
			},
		},
		{
			Name: "BFSDirectionOptCtx", Family: famSearch,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				var t ctxVisitTrace
				err := search.BFSDirectionOptCtx(ctx, f.sym, 0, t.visit)
				return t.String(), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				var t ctxVisitTrace
				search.BFSDirectionOpt(f.sym, 0, t.visit)
				return t.String(), nil
			},
		},
		{
			Name: "BellmanFordCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				d, err := search.BellmanFordCtx(ctx, f.dir, f.dirSrc)
				return ctxDistances(d, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				d, err := search.BellmanFord(f.dir, f.dirSrc)
				return ctxDistances(d, f.dirMax), err
			},
		},
		{
			// BellmanFordInto shares bellmanFordCore with BellmanFordCtx, so it
			// inherits that core's poll placement — which is why both rows failed
			// the pre-cancelled clauses before rmp #2593 and both pass now. No
			// non-Ctx sibling exists.
			Name: "BellmanFordInto", Family: famSearch,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				dist, parent, found := ctxIntoBuffers(f.dirMax)
				err := search.BellmanFordInto(ctx, f.dir, f.dirSrc, dist, parent, found)
				return ctxFloats(dist) + "/" + ctxIDs(parent) + "/" + ctxBools(found), err
			},
		},
		{
			Name: "BiBFSCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, err := search.BiBFSCtx(ctx, f.dir, f.dirSrc, f.dirDst)
				return ctxIDs(p), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				p, err := search.BiBFS(f.dir, f.dirSrc, f.dirDst)
				return ctxIDs(p), err
			},
		},
		{
			Name: "BiBFSOnCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, err := search.BiBFSOnCtx(ctx, f.dir, f.dirRev, f.dirSrc, f.dirDst)
				return ctxIDs(p), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				p, err := search.BiBFSOn(f.dir, f.dirRev, f.dirSrc, f.dirDst)
				return ctxIDs(p), err
			},
		},
		{
			Name: "BidirectionalDijkstraOnCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, cost, err := search.BidirectionalDijkstraOnCtx(ctx, f.dir, f.dirRev, f.dirSrc, f.dirDst)
				return ctxIDs(p) + "/" + ctxF(cost), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				p, cost, err := search.BidirectionalDijkstraOn(f.dir, f.dirRev, f.dirSrc, f.dirDst)
				return ctxIDs(p) + "/" + ctxF(cost), err
			},
		},
		{
			Name: "CountTrianglesCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				total, per, err := search.CountTrianglesCtx(ctx, f.sym)
				return strconv.FormatInt(total, 10) + "/" + ctxInt64s(per), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				total, per := search.CountTriangles(f.sym)
				return strconv.FormatInt(total, 10) + "/" + ctxInt64s(per), nil
			},
		},
		{
			Name: "CountTrianglesParallelCtx", Family: famSearch, twinIsDelegation: true, concurrentReduction: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				total, per, err := search.CountTrianglesParallelCtx(ctx, f.sym, ctxCancelWorkers)
				return strconv.FormatInt(total, 10) + "/" + ctxInt64s(per), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				total, per := search.CountTrianglesParallel(f.sym, ctxCancelWorkers)
				return strconv.FormatInt(total, 10) + "/" + ctxInt64s(per), nil
			},
		},
		{
			Name: "DFSCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				var t ctxVisitTrace
				err := search.DFSCtx(ctx, f.dir, f.dirSrc, t.visit)
				return t.String(), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				var t ctxVisitTrace
				search.DFS(f.dir, f.dirSrc, t.visit)
				return t.String(), nil
			},
		},
		{
			Name: "DiameterCtx", Family: famSearch, twinIsDelegation: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				lo, hi, exact, err := search.DiameterCtx(ctx, f.sym)
				return fmt.Sprintf("%d/%d/%t", lo, hi, exact), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				lo, hi, exact := search.Diameter(f.sym)
				return fmt.Sprintf("%d/%d/%t", lo, hi, exact), nil
			},
		},
		{
			Name: "DijkstraAPSPCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				a, err := search.DijkstraAPSPCtx(ctx, f.dir)
				return ctxAPSP(a, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				a, err := search.DijkstraAPSP(f.dir)
				return ctxAPSP(a, f.dirMax), err
			},
		},
		{
			Name: "DijkstraCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				d, err := search.DijkstraCtx(ctx, f.dir, f.dirSrc)
				return ctxDistances(d, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				d, err := search.Dijkstra(f.dir, f.dirSrc)
				return ctxDistances(d, f.dirMax), err
			},
		},
		{
			// DijkstraInto shares dijkstraCore with DijkstraCtx and SSSP.FromCtx.
			// No non-Ctx sibling exists, so the identity arm is skipped.
			Name: "DijkstraInto", Family: famSearch,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				dist, parent, found := ctxIntoBuffers(f.dirMax)
				err := search.DijkstraInto(ctx, f.dir, f.dirSrc, dist, parent, found)
				return ctxFloats(dist) + "/" + ctxIDs(parent) + "/" + ctxBools(found), err
			},
		},
		{
			Name: "EppsteinKShortestCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, err := search.EppsteinKShortestCtx(ctx, f.dir, f.dirSrc, f.dirDst, 3) //nolint:staticcheck // the deprecated alias is part of the public Ctx surface this family must cover
				return ctxYenPaths(p), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxYenPaths(search.EppsteinKShortest(f.dir, f.dirSrc, f.dirDst, 3)), nil //nolint:staticcheck // ditto
			},
		},
		{
			Name: "FloydWarshallCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				a, err := search.FloydWarshallCtx(ctx, f.dir)
				return ctxAPSP(a, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxAPSP(search.FloydWarshall(f.dir), f.dirMax), nil
			},
		},
		{
			Name: "FloydWarshallParallelCtx", Family: famSearch, twinIsDelegation: true, concurrentReduction: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				a, err := search.FloydWarshallParallelCtx(ctx, f.dir, ctxCancelWorkers)
				return ctxAPSP(a, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxAPSP(search.FloydWarshallParallel(f.dir, ctxCancelWorkers), f.dirMax), nil
			},
		},
		{
			Name: "HierholzerCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				tr, err := search.HierholzerCtx(ctx, f.eulerDir)
				return ctxIDs(tr), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				tr, err := search.Hierholzer(f.eulerDir)
				return ctxIDs(tr), err
			},
		},
		{
			Name: "HierholzerUndirectedCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				tr, err := search.HierholzerUndirectedCtx(ctx, f.eulerUndir)
				return ctxIDs(tr), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				tr, err := search.HierholzerUndirected(f.eulerUndir)
				return ctxIDs(tr), err
			},
		},
		{
			Name: "HopcroftKarpCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				m, err := search.HopcroftKarpCtx(ctx, f.bip, f.bipLeft)
				return fmt.Sprintf("size=%d L=%s R=%s", m.Size, ctxIDs(m.MatchL), ctxIDs(m.MatchR)), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				m := search.HopcroftKarp(f.bip, f.bipLeft)
				return fmt.Sprintf("size=%d L=%s R=%s", m.Size, ctxIDs(m.MatchL), ctxIDs(m.MatchR)), nil
			},
		},
		{
			Name: "HopcroftTarjanBCCCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				r, err := search.HopcroftTarjanBCCCtx(ctx, f.sym)
				return ctxBCC(r), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxBCC(search.HopcroftTarjanBCC(f.sym)), nil
			},
		},
		{
			Name: "HungarianCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				a, err := search.HungarianCtx(ctx, f.hungCost, f.hungN, f.hungN)
				return ctxInts(a.RowToCol) + "/" + ctxF(a.TotalCost), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				a, err := search.Hungarian(f.hungCost, f.hungN, f.hungN)
				return ctxInts(a.RowToCol) + "/" + ctxF(a.TotalCost), err
			},
		},
		{
			Name: "JohnsonAPSPCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				a, err := search.JohnsonAPSPCtx(ctx, f.dir)
				return ctxAPSP(a, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				a, err := search.JohnsonAPSP(f.dir)
				return ctxAPSP(a, f.dirMax), err
			},
		},
		{
			Name: "JohnsonAPSPParallelCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true, concurrentReduction: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				a, err := search.JohnsonAPSPParallelCtx(ctx, f.dir, ctxCancelWorkers)
				return ctxAPSP(a, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				a, err := search.JohnsonAPSPParallel(f.dir, ctxCancelWorkers)
				return ctxAPSP(a, f.dirMax), err
			},
		},
		{
			Name: "KCoreCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				k, err := search.KCoreCtx(ctx, f.sym)
				return ctxInts(k), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxInts(search.KCore(f.sym)), nil
			},
		},
		{
			Name: "KShortestPathsLooplessCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, err := search.KShortestPathsLooplessCtx(ctx, f.dir, f.dirSrc, f.dirDst, 3)
				return ctxYenPaths(p), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxYenPaths(search.KShortestPathsLoopless(f.dir, f.dirSrc, f.dirDst, 3)), nil //nolint:staticcheck // the deprecated bare entry IS the identity counterpart of KShortestPathsLooplessCtx; the battery must call exactly what a caller of the deprecated API calls
			},
		},
		{
			// KShortestPathsLooplessCtxWithOpts is THE implementation: both
			// KShortestPathsLooplessCtx and the deprecated EppsteinKShortestCtx
			// delegate to it, and it is the only one of the three that can return a
			// partial result alongside ErrResourceBudgetExceeded. Driven with the
			// zero Opts, which is exactly what the two delegating forms pass, so the
			// row covers the shared body rather than a second configuration. All
			// three failed the pre-cancelled clauses until rmp #2593 gave this body
			// an entry poll.
			Name: "KShortestPathsLooplessCtxWithOpts", Family: famSearch,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, err := search.KShortestPathsLooplessCtxWithOpts(ctx, f.dir, f.dirSrc, f.dirDst, 3, search.KShortestPathsLooplessOpts{})
				return ctxYenPaths(p), err
			},
		},
		{
			Name: "KruskalMSTCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				es, total, err := search.KruskalMSTCtx(ctx, f.sym)
				return ctxMSTEdges(es) + "/" + ctxF(total), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				es, total, err := search.KruskalMST(f.sym)
				return ctxMSTEdges(es) + "/" + ctxF(total), err
			},
		},
		{
			Name: "PrimMSTCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				parent, found, total, err := search.PrimMSTCtx(ctx, f.sym, 0)
				return ctxIDs(parent) + "/" + ctxBools(found) + "/" + ctxF(total), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				parent, found, total, err := search.PrimMST(f.sym, 0)
				return ctxIDs(parent) + "/" + ctxBools(found) + "/" + ctxF(total), err
			},
		},
		{
			Name: "SSSP.FromCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				d, err := f.sssp.FromCtx(ctx, f.dirSrc)
				return ctxDistances(d, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				d, err := f.sssp.From(f.dirSrc)
				return ctxDistances(d, f.dirMax), err
			},
		},
		{
			Name: "TarjanSCCCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				cs, err := search.TarjanSCCCtx(ctx, f.dir)
				return ctxComponents(cs), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxComponents(search.TarjanSCC(f.dir)), nil
			},
		},
		{
			Name: "TopologicalSortCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				o, err := search.TopologicalSortCtx(ctx, f.dag)
				return ctxIDs(o), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				o, err := search.TopologicalSort(f.dag)
				return ctxIDs(o), err
			},
		},
		{
			Name: "TransitiveClosureCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				t, err := search.TransitiveClosureCtx(ctx, f.dir)
				return ctxTC(t, f.dirMax), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxTC(search.TransitiveClosure(f.dir), f.dirMax), nil
			},
		},
		{
			Name: "WCCCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				comp, k, err := search.WCCCtx(ctx, f.sym)
				return ctxInts(comp) + "/" + strconv.Itoa(k), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				comp, k, err := search.WCC(f.sym)
				return ctxInts(comp) + "/" + strconv.Itoa(k), err
			},
		},
		{
			Name: "WCCParallelCtx", Family: famSearch, twinReportsErr: true, twinIsDelegation: true, concurrentReduction: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				comp, k, err := search.WCCParallelCtx(ctx, f.sym, ctxCancelWorkers)
				return ctxInts(comp) + "/" + strconv.Itoa(k), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				comp, k, err := search.WCCParallel(f.sym, ctxCancelWorkers)
				return ctxInts(comp) + "/" + strconv.Itoa(k), err
			},
		},
		{
			Name: "YenKShortestCtx", Family: famSearch, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, err := search.YenKShortestCtx(ctx, f.dir, f.dirSrc, f.dirDst, 3)
				return ctxYenPaths(p), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxYenPaths(search.YenKShortest(f.dir, f.dirSrc, f.dirDst, 3)), nil
			},
		},

		// ------------------------------------------------------ search/centrality
		{
			Name: "BetweennessCtx", Family: famCentrality, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := centrality.BetweennessCtx(ctx, f.sym)
				return ctxFloats(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxFloats(centrality.Betweenness(f.sym)), nil
			},
		},
		{
			Name: "BetweennessParallelCtx", Family: famCentrality, twinIsDelegation: true, concurrentReduction: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := centrality.BetweennessParallelCtx(ctx, f.sym, ctxCancelWorkers)
				return ctxFloats(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxFloats(centrality.BetweennessParallel(f.sym, ctxCancelWorkers)), nil
			},
		},
		{
			Name: "ClosenessCtx", Family: famCentrality, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := centrality.ClosenessCtx(ctx, f.sym)
				return ctxFloats(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxFloats(centrality.Closeness(f.sym)), nil
			},
		},
		{
			Name: "EigenvectorCtx", Family: famCentrality, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, it, err := centrality.EigenvectorCtx(ctx, f.sym, centrality.DefaultEigenvectorOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				v, it, err := centrality.Eigenvector(f.sym, centrality.DefaultEigenvectorOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
		},
		{
			Name: "HarmonicCtx", Family: famCentrality, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := centrality.HarmonicCtx(ctx, f.sym)
				return ctxFloats(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return ctxFloats(centrality.Harmonic(f.sym)), nil
			},
		},
		{
			Name: "KatzCtx", Family: famCentrality, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, it, err := centrality.KatzCtx(ctx, f.sym, centrality.DefaultKatzOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				v, it, err := centrality.Katz(f.sym, centrality.DefaultKatzOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
		},
		{
			Name: "PageRankCtx", Family: famCentrality, twinReportsErr: true, twinIsDelegation: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, it, err := centrality.PageRankCtx(ctx, f.dir, centrality.DefaultPageRankOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				v, it, err := centrality.PageRank(f.dir, centrality.DefaultPageRankOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
		},
		{
			// PageRanker.Run is the ONE place in this whole surface where the identity
			// arm compares two genuinely independent implementations. PageRankCtx is
			// kept as a single monolithic function on purpose — extracting it behind
			// the PageRanker method boundary was measured to regress the parallel
			// SpMV by ~3% — and PageRanker.Run carries the reusable state machine.
			// Their godoc claims the two are bit-for-bit identical; this row is what
			// tests that claim, and it is NOT a delegation, so twinIsDelegation stays
			// false and the arm is load-bearing.
			//
			// A fresh PageRanker per call: Run's result aliases an internal buffer
			// invalidated by the next Run, and the type is documented as unsafe for
			// concurrent use.
			Name: "PageRanker.Run", Family: famCentrality, twinDecl: "PageRankCtx", twinReportsErr: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, it, err := centrality.NewPageRanker(f.dir).Run(ctx, centrality.DefaultPageRankOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				v, it, err := centrality.PageRankCtx(context.Background(), f.dir, centrality.DefaultPageRankOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
		},
		{
			Name: "PersonalisedPushPageRankCtx", Family: famCentrality, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := centrality.PersonalisedPushPageRankCtx(ctx, f.dir, f.dirSrc, centrality.DefaultPPRPushOptions())
				return ctxFloats(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				v, err := centrality.PersonalisedPushPageRank(f.dir, f.dirSrc, centrality.DefaultPPRPushOptions())
				return ctxFloats(v), err
			},
		},
		{
			Name: "WeightedBetweennessCtx", Family: famCentrality, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := centrality.WeightedBetweennessCtx(ctx, f.sym)
				return ctxFloats(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				v, err := centrality.WeightedBetweenness(f.sym)
				return ctxFloats(v), err
			},
		},
		{
			Name: "WeightedBetweennessParallelCtx", Family: famCentrality, twinReportsErr: true, twinIsDelegation: true, concurrentReduction: true, spawns: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := centrality.WeightedBetweennessParallelCtx(ctx, f.sym, ctxCancelWorkers)
				return ctxFloats(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				v, err := centrality.WeightedBetweennessParallel(f.sym, ctxCancelWorkers)
				return ctxFloats(v), err
			},
		},

		// ------------------------------------------------------- search/community
		{
			Name: "LabelPropagationCtx", Family: famCommunity, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, err := community.LabelPropagationCtx(ctx, f.sym, community.DefaultLabelPropagationOptions())
				return ctxInts(p.Community) + "/" + strconv.Itoa(p.NumCommunities), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				p := community.LabelPropagation(f.sym, community.DefaultLabelPropagationOptions())
				return ctxInts(p.Community) + "/" + strconv.Itoa(p.NumCommunities), nil
			},
		},
		{
			Name: "LeidenCtx", Family: famCommunity, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				p, err := community.LeidenCtx(ctx, f.sym, community.DefaultLeidenOptions())
				return ctxInts(p.Community) + "/" + strconv.Itoa(p.NumCommunities), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				p := community.Leiden(f.sym, community.DefaultLeidenOptions())
				return ctxInts(p.Community) + "/" + strconv.Itoa(p.NumCommunities), nil
			},
		},

		// ------------------------------------------------------------ search/flow
		{
			Name: "EdmondsKarpCtx", Family: famFlow, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := flow.EdmondsKarpCtx(ctx, flowBuildNetwork(f.flowN, f.flowEdges), 0, f.flowN-1)
				return strconv.Itoa(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return strconv.Itoa(flow.EdmondsKarp(flowBuildNetwork(f.flowN, f.flowEdges), 0, f.flowN-1)), nil
			},
		},
		{
			Name: "MaxFlowCtx", Family: famFlow, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := flow.MaxFlowCtx(ctx, flowBuildNetwork(f.flowN, f.flowEdges), 0, f.flowN-1)
				return strconv.Itoa(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return strconv.Itoa(flow.MaxFlow(flowBuildNetwork(f.flowN, f.flowEdges), 0, f.flowN-1)), nil
			},
		},
		{
			Name: "MinCostMaxFlowCtx", Family: famFlow, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				fl, cost, err := flow.MinCostMaxFlowCtx(ctx, flowBuildCostNetwork(f.costN, f.costEdges), 0, f.costN-1)
				return strconv.Itoa(fl) + "/" + strconv.Itoa(cost), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				fl, cost := flow.MinCostMaxFlow(flowBuildCostNetwork(f.costN, f.costEdges), 0, f.costN-1)
				return strconv.Itoa(fl) + "/" + strconv.Itoa(cost), nil
			},
		},
		{
			Name: "PushRelabelMaxFlowCtx", Family: famFlow, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, err := flow.PushRelabelMaxFlowCtx(ctx, flowBuildNetwork(f.flowN, f.flowEdges), 0, f.flowN-1)
				return strconv.Itoa(v), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				return strconv.Itoa(flow.PushRelabelMaxFlow(flowBuildNetwork(f.flowN, f.flowEdges), 0, f.flowN-1)), nil
			},
		},
		{
			Name: "StoerWagnerCtx", Family: famFlow, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				r, err := flow.StoerWagnerCtx(ctx, f.swWeights, f.swN)
				return ctxInts(r.A) + "/" + ctxInts(r.B) + "/" + strconv.Itoa(r.Weight), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				r := flow.StoerWagner(f.swWeights, f.swN)
				return ctxInts(r.A) + "/" + ctxInts(r.B) + "/" + strconv.Itoa(r.Weight), nil
			},
		},

		// ---------------------------------------------------------- search/extern
		{
			Name: "BFSCtx", Family: famExtern, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				var t ctxVisitTrace
				err := extern.BFSCtx(ctx, f.reader, f.dirSrc, t.visit)
				return t.String(), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				var t ctxVisitTrace
				err := extern.BFS(f.reader, f.dirSrc, t.visit)
				return t.String(), err
			},
		},
		{
			Name: "PageRankCtx", Family: famExtern, twinReportsErr: true, twinIsDelegation: true,
			run: func(ctx context.Context, f *ctxFixtures) (string, error) {
				v, it, err := extern.PageRankCtx(ctx, f.reader, extern.DefaultPageRankOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
			twin: func(f *ctxFixtures) (string, error) {
				v, it, err := extern.PageRank(f.reader, extern.DefaultPageRankOptions())
				return ctxFloats(v) + "/" + strconv.Itoa(it), err
			},
		},
	}
}

// searchCtxCancelViolations drives every public context-accepting entry point of
// the five search families through the two implemented cancellation regimes for
// one simulation tick, returning one Violation per clause broken.
//
// The result is a pure function of tick: the same tick yields the same fixtures,
// the same digests and hence the same verdict. It is called from [CheckSearch],
// so it runs on the search battery's cadence (periodically in the deterministic
// loop, after every crash+recovery, and once at the end of the run).
//
// The clauses, per entry point — the file header explains what each one is worth:
//
//	bg-err      the entry point under context.Background() must not error — every
//	            fixture is built to be a valid input for the row it feeds
//	twin-err    the identity counterpart, where its signature can carry an error,
//	            must not error either
//	identity    the two must produce byte-identical output. Harness-guaranteed for
//	            52 of the 54 rows that have a counterpart (a structural
//	            delegation); a real cross-implementation oracle for
//	            PageRanker.Run vs PageRankCtx and for BFSDirectionOpt; a
//	            run-to-run determinism check for the six concurrent-reduction rows.
//	            Four rows have no counterpart at all and skip the clause
//	cancel-err  the pre-cancelled call must return an error satisfying
//	            errors.Is(err, context.Canceled) — identity, not non-nilness
//	cancel-val  ...and must NOT return the values the uncancelled call returned.
//	            This is the arm's non-vacuity gate: it fails both when an entry
//	            point ignores its context and computes the answer anyway, and when
//	            the fixture is so degenerate that "cancelled" and "computed"
//	            render the same, which would leave cancel-err unfalsifiable
//
// It then runs [ctxCancelPrecedenceViolations], which covers the terminal-error
// shape the row loop cannot reach.

func searchCtxCancelViolations(tick int64) []Violation {
	entries := ctxEntries()
	if vs := ctxCancelTableSanity(tick, entries); len(vs) > 0 {
		return vs
	}
	f, cleanup, vs := newCtxFixtures(tick)
	if len(vs) > 0 {
		return vs
	}
	defer cleanup()

	out := make([]Violation, 0, 4)
	for i := range entries {
		out = append(out, ctxCancelCheckEntry(tick, &entries[i], f)...)
	}
	out = append(out, ctxCancelPrecedenceViolations(tick)...)
	return out
}

// ctxCancelPrecedenceRow is one cancellation-outranks-a-terminal-error case: an
// entry point together with an input that makes it return a terminal sentinel,
// and the sentinel it returns.
//
// The main row loop cannot cover this shape. Its fixtures are valid inputs by
// construction — they have to be, or the bg-err clause could not mean anything —
// so an entry point that checks a terminal condition BEFORE consulting its
// context is invisible to it. That is precisely the ordering
// [search.TopologicalSortCtx] documents ("a cancelled context outranks
// [search.ErrCycle]") and the ordering [search.JohnsonAPSPCtx] does NOT honour.
type ctxCancelPrecedenceRow struct {
	// run invokes the entry point over the terminal-condition input.
	run func(ctx context.Context) error
	// sentinel is the error the SAME call must return under an uncancelled
	// context. Checking it is the arm's non-vacuity gate: without it a fixture
	// that failed to trigger the terminal condition would leave the precedence
	// clause with nothing to outrank, and the clause would pass for the wrong
	// reason.
	sentinel error
	name     string
}

// ctxCancelPrecedenceRows returns the precedence table: seven rows over five
// entry points in `search` plus two more in the same package, every one of which
// must put the context error ahead of a terminal sentinel it would otherwise
// return.
//
// The two Johnson rows are the ones this arm was built for. They were the last
// cancellation blindness in the module, and they are now its strongest regression
// guard, because the shape they got wrong — a terminal condition decided in a
// prologue that runs before the entry point's own poll — is a shape the main row
// loop provably cannot see.
func ctxCancelPrecedenceRows() []ctxCancelPrecedenceRow {
	// A directed 3-cycle. With unit weights it is cyclic (no topological order);
	// with a -5 on the closing arc its total weight is negative, so it is a
	// negative cycle reachable from node 0. Both are hand-checkable, and both are
	// FIXED rather than seed-derived: the property under test is an ordering
	// between two error paths, and varying the input would add nothing but noise.
	cyclic := func() *csr.CSR[float64] {
		return ctxWeightedCSR(3, [][2]int{{0, 1}, {1, 2}, {2, 0}}, []float64{1, 1, 1})
	}
	negCycle := func() *csr.CSR[float64] {
		return ctxWeightedCSR(3, [][2]int{{0, 1}, {1, 2}, {2, 0}}, []float64{1, 1, -5})
	}
	return []ctxCancelPrecedenceRow{
		{
			name:     "search.TopologicalSortCtx vs ErrCycle",
			sentinel: search.ErrCycle,
			run: func(ctx context.Context) error {
				_, err := search.TopologicalSortCtx(ctx, cyclic())
				return err
			},
		},
		{
			name:     "search.BellmanFordCtx vs ErrNegativeCycle",
			sentinel: search.ErrNegativeCycle,
			run: func(ctx context.Context) error {
				_, err := search.BellmanFordCtx(ctx, negCycle(), 0)
				return err
			},
		},
		{
			name:     "search.FloydWarshallCtx vs ErrNegativeCycle",
			sentinel: search.ErrNegativeCycle,
			run: func(ctx context.Context) error {
				_, err := search.FloydWarshallCtx(ctx, negCycle())
				return err
			},
		},
		{
			name:     "search.FloydWarshallParallelCtx vs ErrNegativeCycle",
			sentinel: search.ErrNegativeCycle,
			run: func(ctx context.Context) error {
				_, err := search.FloydWarshallParallelCtx(ctx, negCycle(), ctxCancelWorkers)
				return err
			},
		},
		{
			// The two Johnson rows were the LAST cancellation blindness in the module,
			// and the reason this arm exists. Both run their Bellman-Ford reweighting
			// prologue (`bellmanFordVirtualSource`) before their per-source poll, and
			// that prologue incremented its work counter BEFORE masking it, so on a
			// graph whose reweighting finished in under 4096 dequeues the prologue's
			// ErrNegativeCycle beat the context error. They were 2 of 9 APSP /
			// shortest-path entry points to get this wrong. Corrected under rmp #2593
			// (check-then-increment, matching dijkstra.go and prim.go), together with
			// both Johnson godocs, which had described SPFA-on-a-deque as "every
			// relaxation-round boundary" and promised a wrapped ctx.Err() that never
			// wrapped.
			name:     "search.JohnsonAPSPCtx vs ErrNegativeCycle",
			sentinel: search.ErrNegativeCycle,
			run: func(ctx context.Context) error {
				_, err := search.JohnsonAPSPCtx(ctx, negCycle())
				return err
			},
		},
		{
			name:     "search.JohnsonAPSPParallelCtx vs ErrNegativeCycle",
			sentinel: search.ErrNegativeCycle,
			run: func(ctx context.Context) error {
				_, err := search.JohnsonAPSPParallelCtx(ctx, negCycle(), ctxCancelWorkers)
				return err
			},
		},
		{
			// The sentinel here is ErrNegativeEdgeAPSP, not the ErrNegativeWeight that
			// Dijkstra itself returns: DijkstraAPSP declares its own, and the two do
			// not wrap one another. The prec-setup clause caught the wrong choice on
			// its first run, which is the clause doing its job.
			name:     "search.DijkstraAPSPCtx vs ErrNegativeEdgeAPSP",
			sentinel: search.ErrNegativeEdgeAPSP,
			run: func(ctx context.Context) error {
				_, err := search.DijkstraAPSPCtx(ctx, negCycle())
				return err
			},
		},
	}
}

// ctxCancelPrecedenceOverride replaces the precedence table for the duration of a
// test. It exists so [TestSearchCtxCancel_PrecedenceClausesCanFail] can drive a
// deliberately-broken row through the REAL clause code rather than a copy of it —
// a clause proven to fire against a re-implementation proves nothing about the
// clause that ships. It is nil in every non-test path.
var ctxCancelPrecedenceOverride []ctxCancelPrecedenceRow

// ctxCancelPrecedenceViolations asserts, for every row of
// [ctxCancelPrecedenceRows], that a context cancelled before the call outranks the
// terminal error the input would otherwise produce.
//
// Two clauses per row, and the order matters:
//
//	prec-setup  under an UNCANCELLED context the call must return the row's
//	            sentinel. This is the non-vacuity gate — it proves the input really
//	            does reach the terminal path, so the cancelled call below had
//	            something to outrank
//	prec-order  under a pre-cancelled context the call must return an error
//	            satisfying errors.Is(err, context.Canceled), NOT the sentinel
//
// The result is a pure function of nothing but the fixed inputs above, so it is
// trivially reproducible; tick enters only the violation records.
func ctxCancelPrecedenceViolations(tick int64) []Violation {
	rows := ctxCancelPrecedenceRows()
	if ctxCancelPrecedenceOverride != nil {
		rows = ctxCancelPrecedenceOverride
	}
	if len(rows) == 0 {
		return []Violation{ctxCancelDeviate(tick, "precedence",
			"the cancellation-precedence table is empty, so nothing asserts that a cancelled context outranks a terminal sentinel")}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var vs []Violation
	for _, r := range rows {
		if err := r.run(context.Background()); !errors.Is(err, r.sentinel) {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "search:ctxcancel:precedence",
				Message: fmt.Sprintf("[prec-setup] %s: the uncancelled call returned err=%v, so the input no longer reaches the %v path "+
					"and the precedence clause below has nothing to outrank", r.name, err, r.sentinel),
			})
			continue
		}
		err := r.run(cancelled)
		if !errors.Is(err, context.Canceled) {
			vs = append(vs, Violation{
				Kind: ViolationSearchDivergence, Tick: tick, Op: "search:ctxcancel:precedence",
				Message: fmt.Sprintf("[prec-order] %s: a context cancelled BEFORE the call returned err=%v; a cancelled context must outrank "+
					"the terminal sentinel, which means the entry point consults its context before deciding the input is unusable", r.name, err),
			})
		}
	}
	return vs
}

// ctxCancelTableSanity guards the driver against becoming silently vacuous. The
// clauses below are STRUCTURAL — every family must contribute at least one entry
// point, at least one entry must be a concurrent reduction, and at least one must
// be a non-delegating twin — rather than a count, so they stay true as the API
// grows and still fail if a whole family or the only load-bearing identity arm is
// dropped by accident. The exact-coverage claim is asserted separately, by
// [TestSearchCtxCancel_TableCoversEveryEntryPoint], which can parse the source.
func ctxCancelTableSanity(tick int64, entries []ctxEntry) []Violation {
	seenFamily := map[ctxFamily]int{}
	reductions, loadBearing := 0, 0
	for _, e := range entries {
		seenFamily[e.Family]++
		if e.concurrentReduction {
			reductions++
		}
		if e.twin != nil && !e.twinIsDelegation {
			loadBearing++
		}
	}
	var vs []Violation
	for _, fam := range []ctxFamily{famSearch, famCentrality, famCommunity, famFlow, famExtern} {
		if seenFamily[fam] == 0 {
			vs = append(vs, ctxCancelDeviate(tick, "table",
				fmt.Sprintf("the %s family contributes no entry point to the cancellation table: the family is not covered", fam)))
		}
	}
	if reductions == 0 {
		vs = append(vs, ctxCancelDeviate(tick, "table",
			"no concurrent-reduction entry point is in the cancellation table: the identity arm has lost its run-to-run determinism case"))
	}
	if loadBearing == 0 {
		vs = append(vs, ctxCancelDeviate(tick, "table",
			"every identity arm in the table is a structural delegation: the cross-implementation case (PageRanker.Run vs PageRankCtx) is gone, so no identity arm can fail"))
	}
	return vs
}

// ctxCancelCheckEntry runs the Background-identity and pre-cancelled arms for one
// entry point. Every clause is evaluated even when an earlier one failed, so one
// broken entry point reports every way it broke rather than only the first.
func ctxCancelCheckEntry(tick int64, e *ctxEntry, f *ctxFixtures) []Violation {
	var vs []Violation

	// --- Background identity -------------------------------------------------
	bg, bgErr := e.run(context.Background(), f)
	if bgErr != nil {
		vs = append(vs, ctxCancelDiverge(tick, e, fmt.Sprintf(
			"[bg-err] the entry point errored under context.Background() on a fixture built to be valid for it: %v", bgErr)))
	}
	if e.twin != nil {
		twin, twinErr := e.twin(f)
		if e.twinReportsErr && twinErr != nil {
			vs = append(vs, ctxCancelDiverge(tick, e, fmt.Sprintf(
				"[twin-err] the identity counterpart errored on the same fixture: %v", twinErr)))
		}
		if bg != twin {
			vs = append(vs, ctxCancelDiverge(tick, e, fmt.Sprintf(
				"[identity] output under context.Background() differs from the identity counterpart's — %s.\n  this: %s\n  twin: %s",
				ctxIdentityWhy(e), ctxSample(bg), ctxSample(twin))))
		}
	}

	// --- Pre-cancelled -------------------------------------------------------
	// The context is cancelled BEFORE the call, so no scheduling decision, no
	// elapsed time and no amount of progress enters the verdict: either the entry
	// point consults the context it was handed or it does not.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, gotErr := e.run(ctx, f)

	// errors.Is, never ==: four centrality entry points wrap the context error
	// (closeness, harmonic, eigenvector, katz), so an identity comparison would
	// pass the other rows and fail exactly those four.
	if !errors.Is(gotErr, context.Canceled) {
		vs = append(vs, ctxCancelDiverge(tick, e, fmt.Sprintf(
			"[cancel-err] a context cancelled BEFORE the call returned err=%v; want an error satisfying errors.Is(err, context.Canceled). "+
				"Note that non-nilness is NOT the property under test: ErrCycle, ErrNegativeCycle, ErrNoPath and ErrInvalidInput are all non-nil and none of them is a cancellation",
			gotErr)))
	}
	if got == bg {
		vs = append(vs, ctxCancelDiverge(tick, e, fmt.Sprintf(
			"[cancel-val] the pre-cancelled call returned the SAME values as the uncancelled one, so cancellation changed nothing observable. "+
				"Either the entry point ignores its context, or this fixture's answer is the zero value and the cancel-err clause above cannot fail on it.\n  both: %s",
			ctxSample(got))))
	}
	return vs
}

// ctxIdentityWhy explains, in the violation message, what an identity failure
// means for THIS entry point — which differs by kind and is the difference
// between a real defect and a harness bug.
func ctxIdentityWhy(e *ctxEntry) string {
	switch {
	case e.concurrentReduction:
		return "this entry point runs a concurrent reduction: its two invocations are two independent work partitions, so a difference here means the parallel result is NOT reproducible run to run"
	case !e.twinIsDelegation:
		return "the two sides are genuinely independent implementations, so this is a real cross-implementation divergence"
	default:
		return "the counterpart is a structural delegation that runs the same code with context.Background(), so a difference here is a harness defect or a compiler/runtime anomaly, not an algorithm bug"
	}
}
