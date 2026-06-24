package sim

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// eulerSeedSalt is XORed into the tick before deriving this checker's Seed so the
// EULER battery draws an independent random stream from the other per-tick
// search checks (which salt the same tick with their own distinct constants).
// Sharing the raw tick would couple the fixtures of unrelated checks.
const eulerSeedSalt uint64 = 0x5e1b_e017_3a9d_c4f1

// eulerMaxCycles bounds how many edge-disjoint cycles the seed-derived complex
// Eulerian fixture stitches together, keeping every per-tick fixture small and
// the check cost O(E) on a graph whose edge count never exceeds a few hundred.
const eulerMaxCycles = 6

// eulerMaxCycleLen bounds the length of each seed-derived cycle, for the same
// reason: a bounded, reproducible fixture.
const eulerMaxCycleLen = 7

// eulerEdge is a directed edge (Src -> Dst) in an EULER fixture, expressed in
// the integer NodeID space the CSR builder consumes.
type eulerEdge struct {
	Src graph.NodeID
	Dst graph.NodeID
}

// eulerFixture is one deterministic directed graph the EULER battery probes,
// carrying its edge list, the count of distinct NodeIDs it spans, a human label
// for diagnostics, and whether it is an Eulerian CIRCUIT (every vertex
// in-degree == out-degree AND connected, so the trail returns to its start).
//
// circuit is set only on fixtures the checker expects Hierholzer to ACCEPT and
// for which it additionally asserts trail[0] == trail[last]; non-Eulerian
// fixtures (expected to yield search.ErrNoEulerian) leave it false.
type eulerFixture struct {
	name    string
	order   int // number of distinct NodeIDs, 0..order-1
	edges   []eulerEdge
	circuit bool
}

// eulerViolations runs the Hierholzer (Eulerian circuit/path) correctness
// battery for one simulation tick. It builds deterministic directed fixtures of
// two kinds and cross-checks the algorithm against an independent reference:
//
//   - EULERIAN fixtures (a single directed cycle, a figure-eight of two cycles
//     sharing one vertex, a self-loop, and a seed-derived graph assembled from
//     edge-disjoint cycles through a shared hub): every such graph has in-degree
//     == out-degree at every vertex and is connected through its non-zero-degree
//     vertices, so it admits an Eulerian CIRCUIT. The checker requires
//     Hierholzer to return a trail and then VALIDATES that trail independently
//     (length E+1; every consecutive pair is a real directed edge; every
//     directed edge is used EXACTLY once as a multiset; and, for a circuit,
//     first == last). The trail is not unique, so validating the invariant — not
//     comparing against one canonical answer — is the only sound check.
//
//   - NON-EULERIAN fixtures (a degree-imbalanced graph, two disconnected
//     cycles, and a graph with more than two odd-balance vertices): each fails a
//     necessary Eulerian precondition, so the checker requires Hierholzer to
//     return search.ErrNoEulerian.
//
// All randomness flows from a single Seed derived from tick, no map-iteration
// order influences any output, and node ids are plain integers, so a violation
// at a given tick is reproducible bit-for-bit. A clean run returns nil;
// divergences are returned tagged ViolationSearchDivergence with Op
// "search:Hierholzer".
func eulerViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ eulerSeedSalt)

	var vs []Violation
	for _, f := range eulerFixtures(seed) {
		c := eulerBuildCSR(f)
		trail, err := search.Hierholzer(c)

		if f.circuit {
			// Expect an Eulerian circuit: a trail, validated independently.
			if err != nil {
				vs = append(vs, eulerDiverge(tick, fmt.Sprintf(
					"%s: expected an Eulerian circuit but Hierholzer returned error: %v", f.name, err))...)
				continue
			}
			vs = append(vs, eulerValidateTrail(tick, f, trail)...)
			continue
		}

		// Expect a non-Eulerian graph: ErrNoEulerian, nothing else.
		if !errors.Is(err, search.ErrNoEulerian) {
			vs = append(vs, eulerDiverge(tick, fmt.Sprintf(
				"%s: expected search.ErrNoEulerian but Hierholzer returned (trailLen=%d, err=%v)",
				f.name, len(trail), err))...)
		}
	}
	return vs
}

// eulerFixtures returns the deterministic battery of fixtures for one tick: the
// fixed Eulerian and non-Eulerian shapes plus two seed-derived graphs (a random
// single cycle and a random multi-cycle Eulerian graph). The order is fixed and
// independent of any map iteration, so the draw stream — and therefore the whole
// battery — is reproducible from the seed alone.
func eulerFixtures(seed *Seed) []eulerFixture {
	// The two seed-derived fixtures come last and consume the only random draws
	// of the battery; composite-literal elements are evaluated left-to-right, so
	// their construction order is fixed and the battery replays from the seed.
	return []eulerFixture{
		eulerSingleCycle("cycle3", 3),
		eulerFigureEight("figure8"),
		eulerSelfLoop("selfloop"),
		eulerImbalanced("imbalanced-degree-gap"),
		eulerTwoDisconnectedCycles("two-disconnected-cycles"),
		eulerThreeImbalancedVertices("three-odd-balance-vertices"),
		eulerRandomCycle(seed),
		eulerRandomMultiCycle(seed),
	}
}

// --- Eulerian fixtures (expect a valid circuit) --------------------------------

// eulerSingleCycle builds the directed cycle 0->1->...->(n-1)->0. Every vertex
// has in-degree == out-degree == 1 and the graph is a single strongly-connected
// ring, so it admits an Eulerian circuit. n must be >= 2.
func eulerSingleCycle(name string, n int) eulerFixture {
	edges := make([]eulerEdge, 0, n)
	for i := 0; i < n; i++ {
		edges = append(edges, eulerEdge{Src: graph.NodeID(i), Dst: graph.NodeID((i + 1) % n)})
	}
	return eulerFixture{name: name, order: n, edges: edges, circuit: true}
}

// eulerFigureEight builds two directed cycles sharing the single vertex 0:
// 0->1->2->0 and 0->3->4->0. Vertex 0 has in-degree == out-degree == 2, every
// other vertex has 1, and the whole graph is connected through 0, so it admits
// an Eulerian circuit (a non-trivial one that re-enters the hub).
func eulerFigureEight(name string) eulerFixture {
	edges := []eulerEdge{
		{Src: 0, Dst: 1}, {Src: 1, Dst: 2}, {Src: 2, Dst: 0}, // cycle A
		{Src: 0, Dst: 3}, {Src: 3, Dst: 4}, {Src: 4, Dst: 0}, // cycle B
	}
	return eulerFixture{name: name, order: 5, edges: edges, circuit: true}
}

// eulerSelfLoop builds the single self-loop 0->0. Vertex 0 has in-degree ==
// out-degree == 1, so it admits the trivial Eulerian circuit [0, 0].
func eulerSelfLoop(name string) eulerFixture {
	return eulerFixture{name: name, order: 1, edges: []eulerEdge{{Src: 0, Dst: 0}}, circuit: true}
}

// eulerRandomCycle builds a single directed cycle of a seed-chosen length in
// [2, eulerMaxCycleLen]. It is Eulerian by construction (a ring), exercising the
// circuit path on a size the seed varies tick to tick.
func eulerRandomCycle(seed *Seed) eulerFixture {
	n := 2 + seed.IntN(eulerMaxCycleLen-1) // [2, eulerMaxCycleLen]
	return eulerSingleCycle(fmt.Sprintf("random-cycle-%d", n), n)
}

// eulerRandomMultiCycle builds an Eulerian graph as a seed-chosen number of
// edge-disjoint directed cycles, every one routed through the shared hub vertex
// 0. Each added cycle leaves in-degree == out-degree intact at every vertex
// (it is itself balanced) and keeps the graph connected (it touches the hub),
// so the union is guaranteed Eulerian regardless of the random parameters. The
// per-cycle vertices are fresh, so the cycles are edge-disjoint and the only
// shared vertex is the hub — a faithful, larger analogue of the figure-eight.
func eulerRandomMultiCycle(seed *Seed) eulerFixture {
	cycles := 2 + seed.IntN(eulerMaxCycles-1) // [2, eulerMaxCycles]
	var edges []eulerEdge
	next := 1 // node 0 is the hub; fresh interior vertices start at 1
	for ci := 0; ci < cycles; ci++ {
		// interior is the count of NEW vertices on this cycle, in [1, len-1];
		// the cycle is hub -> v1 -> ... -> v_interior -> hub.
		interior := 1 + seed.IntN(eulerMaxCycleLen-1) // [1, eulerMaxCycleLen-1]
		prev := graph.NodeID(0)
		for k := 0; k < interior; k++ {
			v := graph.NodeID(next)
			next++
			edges = append(edges, eulerEdge{Src: prev, Dst: v})
			prev = v
		}
		edges = append(edges, eulerEdge{Src: prev, Dst: 0}) // close back to the hub
	}
	return eulerFixture{
		name:    fmt.Sprintf("random-multicycle-%dcyc-%dnode", cycles, next),
		order:   next,
		edges:   edges,
		circuit: true,
	}
}

// --- Non-Eulerian fixtures (expect search.ErrNoEulerian) -----------------------

// eulerImbalanced builds 0->1, 0->2: vertex 0 has out-degree 2 and in-degree 0,
// a balance gap of 2. No vertex can be a valid Eulerian start/end with a gap
// exceeding 1, so the graph admits neither circuit nor path.
func eulerImbalanced(name string) eulerFixture {
	return eulerFixture{name: name, order: 3, edges: []eulerEdge{{Src: 0, Dst: 1}, {Src: 0, Dst: 2}}}
}

// eulerTwoDisconnectedCycles builds two independent directed 2-cycles,
// {0->1->0} and {2->3->2}. Every vertex is individually balanced (in == out),
// but the graph is disconnected, so any single trail can cover only one
// component and Hierholzer detects the shortfall (trail length != E+1).
func eulerTwoDisconnectedCycles(name string) eulerFixture {
	edges := []eulerEdge{
		{Src: 0, Dst: 1}, {Src: 1, Dst: 0}, // component A
		{Src: 2, Dst: 3}, {Src: 3, Dst: 2}, // component B
	}
	return eulerFixture{name: name, order: 4, edges: edges}
}

// eulerThreeImbalancedVertices builds three vertex-disjoint single edges,
// 0->1, 2->3, 4->5. Each source has out-in == +1 and each sink in-out == +1, so
// there are three "start" and three "end" candidates. An Eulerian path admits at
// most one of each; three of each violates the precondition outright.
func eulerThreeImbalancedVertices(name string) eulerFixture {
	edges := []eulerEdge{{Src: 0, Dst: 1}, {Src: 2, Dst: 3}, {Src: 4, Dst: 5}}
	return eulerFixture{name: name, order: 6, edges: edges}
}

// --- CSR construction ----------------------------------------------------------

// eulerBuildCSR materialises f as an immutable directed CSR[float64].
//
// It computes the length-(order+1) offsets array (vertices) and the
// source-grouped flat edge array exactly as csr.BuildFromAdjList would, via a
// counting pass over the out-degrees followed by a scatter. Building the offsets
// programmatically — rather than hand-writing them per fixture — eliminates the
// off-by-one class of bug that an Eulerian fixture is especially prone to: every
// destination, including a sink that has no out-edges, must own an offset slot,
// or the algorithm's in-degree pass indexes out of range. order must be strictly
// greater than every NodeID that appears as a source OR a destination.
func eulerBuildCSR(f eulerFixture) *csr.CSR[float64] {
	order := f.order
	// Offsets: vertices[i] = start of node i's out-edges; vertices[order] = E.
	vertices := make([]uint64, order+1)
	for _, e := range f.edges {
		vertices[int(e.Src)+1]++ // tally out-degree into the next slot
	}
	for i := 1; i <= order; i++ {
		vertices[i] += vertices[i-1] // prefix sum -> offsets
	}
	// Scatter edges into source-grouped slots using a per-source cursor.
	edges := make([]graph.NodeID, len(f.edges))
	cursor := make([]uint64, order)
	for _, e := range f.edges {
		s := int(e.Src)
		pos := vertices[s] + cursor[s]
		edges[pos] = e.Dst
		cursor[s]++
	}
	return csr.FromArrays[float64](vertices, edges, nil, uint64(order), uint64(len(edges)))
}

// --- Independent trail validation ----------------------------------------------

// eulerValidateTrail independently verifies that trail is a genuine Eulerian
// trail of f, without trusting the algorithm that produced it. Because an
// Eulerian trail is not unique, this asserts the defining INVARIANT rather than
// comparing against a canonical answer. It checks, in order:
//
//   - length is exactly E+1;
//   - every NodeID in the trail is in range [0, order);
//   - every consecutive pair (trail[i], trail[i+1]) is a real directed edge of
//     f, and each directed edge of f is traversed EXACTLY once — verified by
//     decrementing a multiset of the graph's edges and requiring it to empty;
//   - for a circuit, the trail is closed (first == last).
//
// The first failing assertion short-circuits with a single violation.
func eulerValidateTrail(tick int64, f eulerFixture, trail []graph.NodeID) []Violation {
	if len(trail) != len(f.edges)+1 {
		return eulerDiverge(tick, fmt.Sprintf(
			"%s: trail length = %d, want E+1 = %d", f.name, len(trail), len(f.edges)+1))
	}
	for _, nid := range trail {
		if int(nid) < 0 || int(nid) >= f.order {
			return eulerDiverge(tick, fmt.Sprintf(
				"%s: trail contains out-of-range NodeID %d (order=%d)", f.name, int(nid), f.order))
		}
	}

	// Multiset of the graph's directed edges; each must be consumed exactly once
	// by a consecutive pair of the trail. A map keyed by an integer pair carries
	// no iteration-order dependence into any output.
	remaining := make(map[eulerEdge]int, len(f.edges))
	for _, e := range f.edges {
		remaining[e]++
	}
	for i := 0; i+1 < len(trail); i++ {
		step := eulerEdge{Src: trail[i], Dst: trail[i+1]}
		if remaining[step] == 0 {
			return eulerDiverge(tick, fmt.Sprintf(
				"%s: trail step %d uses %d->%d which is not an unused directed edge of the graph",
				f.name, i, int(step.Src), int(step.Dst)))
		}
		remaining[step]--
	}
	// Report any un-traversed edges deterministically: collect and sort the
	// leftover keys (a map range alone would let Go's randomised iteration order
	// pick which edge a failing run names, breaking message reproducibility).
	var leftover []eulerEdge
	for e, n := range remaining {
		if n != 0 {
			leftover = append(leftover, e)
		}
	}
	if len(leftover) > 0 {
		slices.SortFunc(leftover, func(a, b eulerEdge) int {
			if a.Src != b.Src {
				return cmp.Compare(a.Src, b.Src)
			}
			return cmp.Compare(a.Dst, b.Dst)
		})
		e := leftover[0]
		return eulerDiverge(tick, fmt.Sprintf(
			"%s: directed edge %d->%d was not traversed exactly once (%d edge(s) left unused)",
			f.name, int(e.Src), int(e.Dst), len(leftover)))
	}

	if f.circuit && trail[0] != trail[len(trail)-1] {
		return eulerDiverge(tick, fmt.Sprintf(
			"%s: Eulerian circuit must be closed but first=%d != last=%d",
			f.name, int(trail[0]), int(trail[len(trail)-1])))
	}
	return nil
}

// eulerDiverge builds a single Hierholzer divergence violation.
func eulerDiverge(tick int64, msg string) []Violation {
	return []Violation{{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:Hierholzer", Message: msg}}
}
