package csr

// intersect.go — the multi-way sorted-set intersection primitive (rmp #2156).
//
// # What it is for
//
// Intersecting several sorted neighbour ranges is the primitive a cyclic-pattern
// join reduces to: for a triangle, the candidates for the closing variable are
// exactly N_out(b) ∩ N_in(a). The design SPIKE (#2155,
// docs/design-wcoj-cyclic-patterns.md) measured this against the binary-join plan
// it replaces — enumerate one leg and probe the other, once per candidate — and
// found the intersection 1.45×–1.56× faster on the fixture built to be maximally
// hostile to it, rising to 4.4×–6.0× once a per-slot type check stops being a map
// probe. Both arms returned identical results at every size.
//
// It lives in graph/csr, not in the Cypher layer, for two reasons the SPIKE made
// explicit: it must be unit-testable without an engine, and graph algorithms
// (triangle counting, clustering coefficients, motif search) want the same
// primitive.
//
// # Why leapfrog, and why it MUST gallop
//
// The cursor furthest behind is advanced by SEEKING to the current candidate
// rather than by stepping to the next slot, so the cost tracks the SMALLEST
// participating set instead of the sum of all of them.
//
// This is not a micro-optimisation, it is the whole point, and getting it wrong
// silently produces a primitive with no advantage at all. A plain sorted MERGE
// costs O(p+q), and summed over a triangle's driving edges
// Σ_(a,b)(d_out(b)+d_in(a)) = Σ_v d(v)² — which is *identically* the number of
// two-paths the binary-join plan enumerates. A merge-based primitive would
// therefore deliver exactly zero asymptotic benefit. Galloping is what makes the
// cost O(p·log(q/p)) for p ≤ q (Hwang–Lin 1972; instance-optimal refinement in
// Demaine–López-Ortiz–Munro, SODA 2000).
//
// # Ordering
//
// Init orders the ranges SHORTEST FIRST, so the shortest set drives and the
// galloping seeks skip through the longer ones. The ordering key is the range
// length — `vertices[v+1] − vertices[v]`, an O(1) subtraction on data already in
// cache. The SPIKE established that this is strictly better than the count store
// for the purpose: run lengths are exact per-vertex and can never be stale,
// whereas a count-store triple count is a global per-(label, type, direction)
// aggregate that cannot distinguish a hub from a leaf. No count cell is consulted
// here, so nothing here can be wrong when one is dirty.
//
// # Distinct destinations
//
// The intersection is over DISTINCT destinations. A CSR run orders slots by the
// total key (destination, handle), so parallel edges to one destination form a
// contiguous block; this primitive yields such a destination ONCE. That is the
// correct contract for the cyclic-pattern operator, which must then re-expand each
// leg's handle run to recover openCypher's parallel-edge multiplicity — see
// docs/design-wcoj-cyclic-patterns.md §5.2. A caller that needs multiplicity must
// ask for it explicitly; silently returning duplicates here would make the
// primitive useless for set semantics and would not by itself give correct
// multiplicity either.
//
// # Allocation
//
// [Intersector] holds its cursors in fixed-size arrays and is designed to be
// reused: Init resets it in place and performs no allocation, so a caller that
// keeps one per query (or pools it) pays nothing per intersection. Ranges are
// passed as a caller-owned slice so no variadic backing array is allocated.
//
// # Concurrency
//
// An Intersector is NOT safe for concurrent use — it is per-cursor mutable state.
// The ranges it reads are immutable CSR snapshots, so any number of Intersectors
// may read the same CSR concurrently without synchronisation.

import "github.com/FlavioCFOliveira/GoGraph/graph"

// MaxIntersectWays bounds how many ranges one [Intersector] can intersect.
//
// The bound is explicit rather than dynamic because the project forbids unbounded
// per-operation state, and because the pattern shapes that reach this primitive are
// small by construction: a variable in a Cypher pattern acquires one participating
// range per already-bound neighbour, so a triangle or a square needs 2 and a
// densely chorded pattern needs 3–4. Eight leaves generous headroom while keeping
// the cursor arrays inside a couple of cache lines.
const MaxIntersectWays = 8

// Range is a half-open [Start, End) window into one source's neighbour slots.
//
// Edges must be ascending by destination over that window, which every CSR built
// by this package guarantees in BOTH directions: BuildFromAdjList and
// BuildFromAdjListLive call OrderRuns explicitly, and BuildReverse's runs come out
// ordered by construction — an invariant pinned by
// TestBuildReverse_RunsAreNeighbourAndHandleOrdered_2151 precisely because a
// binary search over an unordered run returns wrong rows rather than failing.
type Range struct {
	Edges      []graph.NodeID
	Start, End uint64
}

// Len returns the number of slots in the range.
func (r Range) Len() uint64 {
	if r.End <= r.Start {
		return 0
	}
	return r.End - r.Start
}

// Intersector yields the destinations present in every one of its ranges, in
// ascending order, each exactly once.
//
// Not safe for concurrent use. Reuse it across intersections to stay
// allocation-free; Init resets all state.
type Intersector struct {
	edges [MaxIntersectWays][]graph.NodeID
	pos   [MaxIntersectWays]uint64
	end   [MaxIntersectWays]uint64
	n     int
	done  bool
}

// Init prepares it to intersect ranges and reports whether a non-empty result is
// still possible. It allocates nothing.
//
// It returns false — and leaves the Intersector exhausted — when there are no
// ranges, when more than [MaxIntersectWays] are supplied, or when ANY range is
// empty. The empty case is not an error: the intersection is provably empty, and
// short-circuiting it is exactly the "a cycle where one leg is empty" case the
// SPIKE identified as strictly cheaper than the plan it replaces, which would
// still scan the other leg in full.
//
// A single range is accepted and yields that range's distinct destinations.
func (it *Intersector) Init(ranges []Range) bool {
	it.n = 0
	it.done = true
	if len(ranges) == 0 || len(ranges) > MaxIntersectWays {
		return false
	}
	for _, r := range ranges {
		if r.Len() == 0 {
			return false
		}
	}
	for i, r := range ranges {
		it.edges[i] = r.Edges
		it.pos[i] = r.Start
		it.end[i] = r.End
	}
	it.n = len(ranges)
	it.done = false
	it.sortByLenAsc()
	return true
}

// sortByLenAsc orders the cursors shortest range first by insertion sort — n is at
// most MaxIntersectWays, so this beats anything asymptotically better, and it
// allocates nothing (sort.Slice would force the closure and the slice header to
// the heap, which is the allocation regression pattern this project has hit
// before).
func (it *Intersector) sortByLenAsc() {
	for i := 1; i < it.n; i++ {
		e, p, n := it.edges[i], it.pos[i], it.end[i]
		length := n - p
		j := i - 1
		for j >= 0 && (it.end[j]-it.pos[j]) > length {
			it.edges[j+1], it.pos[j+1], it.end[j+1] = it.edges[j], it.pos[j], it.end[j]
			j--
		}
		it.edges[j+1], it.pos[j+1], it.end[j+1] = e, p, n
	}
}

// Next returns the next destination common to every range, and whether one was
// found. Once it returns false it keeps returning false.
func (it *Intersector) Next() (graph.NodeID, bool) {
	if it.done {
		return 0, false
	}
	// cand is the largest destination any cursor currently sits on; every cursor
	// must reach it for a match. When a seek overshoots, cand grows and the other
	// cursors must be re-checked against the new value — which is what `agreed`
	// resetting to 1 expresses.
	cand := uint64(it.edges[0][it.pos[0]])
	agreed := 1
	i := 1
	for agreed < it.n {
		if i >= it.n {
			i = 0
		}
		v, ok := it.seek(i, cand)
		if !ok {
			it.done = true
			return 0, false
		}
		if v == cand {
			agreed++
		} else {
			// Overshot: v becomes the new candidate and only cursor i is known to
			// be on it.
			cand = v
			agreed = 1
		}
		i++
	}
	// Every cursor sits on cand. Advance the SHORTEST range past cand's whole run
	// so the next call makes progress and cannot re-emit it; the other cursors are
	// carried past it by their next seek.
	it.advancePast(0, cand)
	if it.pos[0] >= it.end[0] {
		it.done = true
	}
	return graph.NodeID(cand), true
}

// seek advances cursor i to the first slot whose destination is >= target and
// returns that destination. It reports false when the range is exhausted.
//
// The stride doubles before the bounded binary search, so a long skip costs
// O(log skip) rather than O(skip) — the leapfrog discipline. The first comparison
// short-circuits the overwhelmingly common case where the cursor already satisfies
// the target, keeping a matched cursor at one comparison.
func (it *Intersector) seek(i int, target uint64) (uint64, bool) {
	edges, pos, end := it.edges[i], it.pos[i], it.end[i]
	if pos >= end {
		return 0, false
	}
	if uint64(edges[pos]) >= target {
		return uint64(edges[pos]), true
	}
	lo, stride := pos, uint64(1)
	for {
		probe := lo + stride
		if probe >= end {
			break
		}
		if uint64(edges[probe]) >= target {
			break
		}
		lo = probe
		stride *= 2
	}
	hi := lo + stride
	if hi > end {
		hi = end
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if uint64(edges[mid]) < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	it.pos[i] = lo
	if lo >= end {
		return 0, false
	}
	return uint64(edges[lo]), true
}

// advancePast moves cursor i beyond every slot whose destination equals v.
//
// It walks rather than binary-searches: the equal slots are one parallel-edge
// group, which is a single slot in a simple graph and small in practice, so a walk
// is cheaper than another chain of dependent loads. This is the same reasoning
// cypher/exec/csrprobe.go's dstRun records for the same shape.
func (it *Intersector) advancePast(i int, v uint64) {
	edges, end := it.edges[i], it.end[i]
	p := it.pos[i]
	for p < end && uint64(edges[p]) == v {
		p++
	}
	it.pos[i] = p
}

// NeighbourRange returns the half-open slot window holding src's neighbours, and
// whether src is present in this CSR's vertex space.
//
// A cached CSR pair can legitimately be NARROWER than the live node space — a
// bare node CREATE does not change edge topology and so does not invalidate it —
// so callers must not index the offsets array unguarded. This accessor is that
// guard. A node with no edges correctly yields an empty range.
func (c *CSR[W]) NeighbourRange(src graph.NodeID) (Range, bool) {
	u := uint64(src)
	if u+1 >= uint64(len(c.vertices)) {
		return Range{}, false
	}
	return Range{Edges: c.edges, Start: c.vertices[u], End: c.vertices[u+1]}, true
}
