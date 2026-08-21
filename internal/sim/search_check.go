package sim

import (
	"fmt"
	"slices"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// maxDivergenceSamples bounds how many differing names/edges a parity violation
// message lists, so a wholesale divergence cannot produce an unbounded report.
const maxDivergenceSamples = 8

// maxDenseCheckNodes caps the node count at which the O(n^2) checks (those that
// materialise the full forward-reachability matrix: SCC and transitive closure)
// run. Above it those two are skipped to keep the per-check cost bounded; the
// cheaper checks still run. The search scenario's graph stays far below this, so
// the cap is a safety valve, not a coverage limit in practice.
const maxDenseCheckNodes = 4000

// CheckSearch runs the search-algorithm battery against the current graph and
// returns any violations it finds (a clean check returns nil). It performs two
// independent families of check:
//
//   - Structural parity between the engine graph — extracted via the public
//     Cypher read path, the same path the workload uses — and the oracle's
//     shadow model: a full node-set and edge-set equality. This is strictly
//     stronger than the base checker's count-plus-sample probes, and proving it
//     lets the algorithm checks run on the model as a faithful stand-in for the
//     engine's contents.
//   - Algorithm correctness: each search/ algorithm run on the oracle graph is
//     compared to an independent naive reference on an invariant of the answer
//     (reachable set, partition up to relabelling) rather than a non-unique
//     witness.
//   - The context-cancellation contract of every public context-accepting entry
//     point in the five search families ([searchCtxCancelViolations]). This
//     family reads no graph: it derives its own fixtures from the tick, because
//     what it adjudicates is whether a cancelled context is honoured, whether
//     the cancellable form computes what the plain form computes, and whether
//     cancellation outranks a terminal sentinel such as [search.ErrCycle].
//
// CheckSearch must be called from the single simulation goroutine: it issues
// read queries that require a consistent, quiescent view of the engine, which
// the deterministic tick loop guarantees. Running it under the concurrent modes
// would race the writers and is not supported.
func CheckSearch(tick int64, oracle *GraphOracle, engine Engine) []Violation {
	ora := oracleNameGraph(oracle)
	eng, err := engineNameGraph(engine)
	if err != nil {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "search:extract",
			Message: fmt.Sprintf("engine graph extraction failed: %v", err),
		}}
	}
	vs := structuralParity(tick, eng, ora)
	// The algorithms run on the oracle graph (the ground truth): structural
	// parity has already established it equals the engine's contents, so an
	// algorithm divergence here is a bug in the algorithm, not in the engine.
	vs = append(vs, searchAlgorithmViolations(tick, ora)...)
	// Shaped-fixture algorithm families that need a specific graph structure
	// (flow networks, bipartite/assignment, Eulerian) generate their own
	// deterministic fixtures from the tick rather than the live graph; see
	// search_flow.go etc.
	vs = append(vs, flowViolations(tick)...)
	vs = append(vs, matchingViolations(tick)...)
	vs = append(vs, eulerViolations(tick)...)
	vs = append(vs, eulerUndirectedViolations(tick)...)
	vs = append(vs, centralityViolations(tick)...)
	vs = append(vs, centralityMeasureViolations(tick)...)
	vs = append(vs, pagerankViolations(tick)...)
	vs = append(vs, communityViolations(tick)...)
	vs = append(vs, kshortestViolations(tick)...)
	vs = append(vs, negWeightViolations(tick)...)
	vs = append(vs, topoDAGViolations(tick)...)
	vs = append(vs, diameterViolations(tick)...)
	vs = append(vs, bfsDoViolations(tick)...)
	vs = append(vs, externViolations(tick)...)
	// The cancellation battery: every public context-accepting entry point of
	// search, search/centrality, search/community, search/flow and search/extern,
	// driven under context.Background() against its non-Ctx counterpart and under
	// a pre-cancelled context. It builds its own deterministic fixtures from the
	// tick (see search_ctx_cancel.go) rather than reading the live graph, because
	// what it adjudicates is the context contract, not an algorithm's answer.
	vs = append(vs, searchCtxCancelViolations(tick)...)
	return vs
}

// structuralParity compares the engine and oracle graphs for an exact node-set
// and edge-set match, returning one violation per divergence class found.
func structuralParity(tick int64, eng, ora *nameGraph) []Violation {
	var vs []Violation
	// Only the engine graph's sawUnknownEndpoint is meaningful: oracleNameGraph
	// pre-filters edges whose endpoints are not live, so the oracle side can never
	// flag one. A flag here means the engine served an edge to a node it did not
	// return as a Person — a structural anomaly.
	if eng.sawUnknownEndpoint {
		vs = append(vs, Violation{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "search:edges",
			Message: "engine returned a KNOWS edge with an endpoint absent from the Person node set",
		})
	}
	if !slices.Equal(eng.names, ora.names) {
		missing := setDiff(ora.names, eng.names)
		extra := setDiff(eng.names, ora.names)
		vs = append(vs, Violation{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "search:nodes",
			Message: fmt.Sprintf("node-set divergence: %d missing-in-engine %s, %d extra-in-engine %s",
				len(missing), sampleNames(missing), len(extra), sampleNames(extra)),
		})
	}
	engEdges := eng.edgeKeys()
	oraEdges := ora.edgeKeys()
	if !slices.Equal(engEdges, oraEdges) {
		missing := setDiff(oraEdges, engEdges)
		extra := setDiff(engEdges, oraEdges)
		vs = append(vs, Violation{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "search:edges",
			Message: fmt.Sprintf("edge-set divergence: %d missing-in-engine %s, %d extra-in-engine %s",
				len(missing), sampleNames(renderEdgeKeys(missing)),
				len(extra), sampleNames(renderEdgeKeys(extra))),
		})
	}
	return vs
}

// searchAlgorithmViolations runs the connectivity battery on the graph and
// compares each algorithm's answer to an independent naive reference.
func searchAlgorithmViolations(tick int64, g *nameGraph) []Violation {
	n := len(g.names)
	if n == 0 {
		return nil
	}
	var vs []Violation
	c := g.toCSR()

	// Weakly-connected components: compared as a partition up to relabelling.
	// The independent union-find reference is computed once and reused for both
	// the serial and the parallel WCC comparison, so the parallel variant is
	// held to the same ground truth as the serial one (which transitively pins
	// serial == parallel without trusting either against the other).
	refWCCSig := componentPartitionSig(g.naiveWCC())
	if comp, _, err := search.WCC(c); err != nil {
		vs = append(vs, searchDeviation(tick, "WCC", err))
	} else if componentPartitionSig(comp) != refWCCSig {
		vs = append(vs, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:WCC",
			Message: "WCC partition disagrees with the union-find reference",
		})
	}

	// Parallel WCC (pinned workers) must induce the same partition as the
	// independent union-find reference — connected components are a function of
	// the edge set alone, so the worker count must not change the answer.
	if comp, _, err := search.WCCParallel(c, searchParallelWorkers); err != nil {
		vs = append(vs, searchDeviation(tick, "WCCParallel", err))
	} else if componentPartitionSig(comp) != refWCCSig {
		vs = append(vs, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:WCCParallel",
			Message: "WCCParallel partition disagrees with the union-find reference (serial WCC's ground truth)",
		})
	}

	// BFS and DFS reachability from a few deterministic sources: the reachable
	// SET is invariant to traversal order, so both must equal the reference.
	for _, src := range g.checkSources() {
		want := g.naiveReachable(src)
		vs = appendReachabilityViolation(vs, tick, "BFS", g.names[src], want, bfsReachable(c, src, n))
		vs = appendReachabilityViolation(vs, tick, "DFS", g.names[src], want, dfsReachable(c, src, n))
	}

	// Ordering and strong connectivity: topological sort is validated (its order
	// is not unique); SCC and transitive closure are compared to references built
	// from the forward-reachability matrix, which is O(n^2) and so gated by the
	// node cap (the search scenario stays well under it).
	vs = append(vs, topoViolations(tick, g, c)...)
	if n <= maxDenseCheckNodes {
		fwd := g.forwardReachAll()
		vs = append(vs, sccViolations(tick, g, c, fwd)...)
		vs = append(vs, tcViolations(tick, g, c, fwd)...)
	}

	// Weighted shortest paths: SSSP/APSP over a deterministically-weighted view
	// of the same graph (see search_sssp.go), compared on distance maps.
	vs = append(vs, ssspViolations(tick, g)...)

	// Minimum spanning forest over the undirected weighted view (see
	// search_mst.go), compared on total weight plus spanning-forest validity.
	vs = append(vs, mstViolations(tick, g)...)

	// k-core and biconnected components over the undirected view (see
	// search_kcore_bcc.go), compared on coreness and the articulation/bridge sets
	// against definition-based references.
	vs = append(vs, kcoreViolations(tick, g)...)
	vs = append(vs, bccViolations(tick, g)...)

	// Triangle counting over the simple undirected view (see search_triangles.go),
	// cross-checked against an O(V*d^2) definition reference and serial == parallel.
	vs = append(vs, triangleViolations(tick, g)...)

	// Bidirectional BFS point-to-point over the unweighted directed CSR (see
	// search_bibfs.go), compared on hop distance against the plain-BFS reference.
	vs = append(vs, bibfsViolations(tick, g, c)...)
	return vs
}

// searchParallelWorkers is the worker count pinned for the parallel search
// variants driven on the live graph (WCCParallel, CountTrianglesParallel). It is
// fixed so a parallel run is reproducible for a given graph; integer-monoid
// reductions (component labels, triangle counts) are order-independent, so the
// pinned count only bounds fan-out — it never changes the answer.
const searchParallelWorkers = 4

// reachResult is a traversal's reachable set plus whether the traversal yielded
// any NodeID outside the dense [0,n) range (which must never happen for a graph
// built from the dense labelling, and is therefore reported loudly rather than
// silently dropped).
type reachResult struct {
	ids        []int
	outOfRange bool
}

// appendReachabilityViolation appends a violation when a traversal's reachable
// set diverges from the reference, or when it yielded an out-of-range NodeID.
func appendReachabilityViolation(vs []Violation, tick int64, algo, src string, want []int, got reachResult) []Violation {
	switch {
	case got.outOfRange:
		return append(vs, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo,
			Message: fmt.Sprintf("%s from %q yielded a NodeID outside the dense [0,n) range", algo, src),
		})
	case !slices.Equal(got.ids, want):
		return append(vs, Violation{
			Kind: ViolationSearchDivergence, Tick: tick, Op: "search:" + algo,
			Message: fmt.Sprintf("%s reachable set from %q disagrees with the reference (got %d, want %d)",
				algo, src, len(got.ids), len(want)),
		})
	default:
		return vs
	}
}

// bfsReachable returns the sorted dense ids the search-package BFS reaches from
// src, flagging any NodeID that falls outside the dense [0,n) range instead of
// silently dropping it.
func bfsReachable(c *csr.CSR[float64], src, n int) reachResult {
	seen := make([]bool, n)
	var oor bool
	search.BFS(c, graph.NodeID(src), func(node graph.NodeID, _ int) bool {
		if int(node) < n {
			seen[int(node)] = true
		} else {
			oor = true
		}
		return true
	})
	return reachResult{ids: boolsToSortedIDs(seen), outOfRange: oor}
}

// dfsReachable returns the sorted dense ids the search-package DFS reaches from
// src, flagging any out-of-range NodeID like [bfsReachable].
func dfsReachable(c *csr.CSR[float64], src, n int) reachResult {
	seen := make([]bool, n)
	var oor bool
	search.DFS(c, graph.NodeID(src), func(node graph.NodeID, _ int) bool {
		if int(node) < n {
			seen[int(node)] = true
		} else {
			oor = true
		}
		return true
	})
	return reachResult{ids: boolsToSortedIDs(seen), outOfRange: oor}
}

// searchDeviation builds a violation for an unexpected error returned by a
// search algorithm.
func searchDeviation(tick int64, algo string, err error) Violation {
	return Violation{
		Kind: ViolationOracleDeviation, Tick: tick, Op: "search:" + algo,
		Message: fmt.Sprintf("%s returned an unexpected error: %v", algo, err),
	}
}

// setDiff returns the elements of a that are absent from b. Both inputs may be
// in any order; the result preserves a's order.
func setDiff(a, b []string) []string {
	bs := make(map[string]struct{}, len(b))
	for _, x := range b {
		bs[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := bs[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}

// sampleNames renders up to maxDivergenceSamples entries for a message, with a
// trailing ellipsis when truncated.
func sampleNames(xs []string) string {
	if len(xs) == 0 {
		return "[]"
	}
	total := len(xs)
	if total > maxDivergenceSamples {
		xs = xs[:maxDivergenceSamples]
	}
	shown := strings.Join(xs, ",")
	if total > maxDivergenceSamples {
		shown += ",..."
	}
	return "[" + shown + "]"
}

// renderEdgeKeys turns the internal "src\x00dst" edge keys into readable
// "src->dst" form for a message.
func renderEdgeKeys(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = strings.Replace(k, "\x00", "->", 1)
	}
	return out
}
