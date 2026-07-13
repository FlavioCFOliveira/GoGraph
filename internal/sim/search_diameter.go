package sim

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/search"
)

// diameterSalt keeps the diameter check's draw stream disjoint from the other
// per-tick search checks (it consumes draws only for the seed-derived sizes).
const diameterSalt uint64 = 0xd1a3_e7c2_5f80_ab19

// diameterFixture is one shaped undirected graph the diameter check probes,
// carrying its symmetric edge list, order, and human label. The TRUE diameter is
// not stored — it is recomputed independently by all-pairs BFS so a wrong closed
// form cannot bias the check.
type diameterFixture struct {
	name  string
	order int
	edges [][2]int
}

// diameterViolations cross-checks [search.Diameter]'s (lo, hi, exact) bounds
// against the TRUE diameter computed independently by all-pairs BFS, on shaped
// undirected fixtures with well-understood diameters (paths, cycles, stars):
//
//   - lo <= true <= hi always (the estimate must bracket the true value);
//   - exact => lo == hi == true (a claimed-exact result must actually be exact);
//   - lo <= hi always (a well-formed bound interval).
//
// Diameter expects a symmetric (undirected) CSR, which [symCSRFromEdges] builds.
// The true diameter is the maximum finite eccentricity over all sources, from a
// from-scratch BFS ([bfsDistancesFromAdj]) that shares no code with the 2-sweep
// / iFUB estimator under test. Fixed sizes plus one seed-derived size per shape
// keep the fixtures varied yet bounded.
func diameterViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ diameterSalt)
	var vs []Violation
	for _, f := range diameterFixtures(seed) {
		c := symCSRFromEdges(f.order, f.edges)
		lo, hi, exact := search.Diameter(c)
		trueDiam := diameterReference(f.order, f.edges)

		if lo > hi {
			vs = append(vs, diameterDiverge(tick, fmt.Sprintf(
				"%s: lo %d > hi %d (malformed bound interval)", f.name, lo, hi)))
		}
		if lo > trueDiam || hi < trueDiam {
			vs = append(vs, diameterDiverge(tick, fmt.Sprintf(
				"%s: bounds [%d,%d] do not bracket the true diameter %d", f.name, lo, hi, trueDiam)))
		}
		if exact && (lo != trueDiam || hi != trueDiam) {
			vs = append(vs, diameterDiverge(tick, fmt.Sprintf(
				"%s: exact=true but [lo=%d, hi=%d] != true diameter %d", f.name, lo, hi, trueDiam)))
		}
	}
	return vs
}

// diameterFixtures returns the deterministic battery: fixed path/cycle/star
// shapes plus one seed-derived size for each. The order is fixed and the only
// random draws are the three sizes, so the battery replays from the seed.
func diameterFixtures(seed *Seed) []diameterFixture {
	pathN := 4 + seed.IntN(6)  // 4..9
	cycleN := 4 + seed.IntN(6) // 4..9
	starK := 3 + seed.IntN(6)  // 3..8 leaves
	return []diameterFixture{
		diameterPath("path5", 5),
		diameterPath("path8", 8),
		diameterCycle("cycle5", 5),
		diameterCycle("cycle8", 8),
		diameterStar("star6", 6),
		diameterPath(fmt.Sprintf("path-rand-%d", pathN), pathN),
		diameterCycle(fmt.Sprintf("cycle-rand-%d", cycleN), cycleN),
		diameterStar(fmt.Sprintf("star-rand-%d", starK+1), starK+1),
	}
}

// diameterPath builds the undirected path 0-1-...-(n-1); its true diameter is
// n-1. n must be >= 2.
func diameterPath(name string, n int) diameterFixture {
	edges := make([][2]int, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, [2]int{i, i + 1})
	}
	return diameterFixture{name: name, order: n, edges: edges}
}

// diameterCycle builds the undirected cycle 0-1-...-(n-1)-0; its true diameter
// is floor(n/2). n must be >= 3.
func diameterCycle(name string, n int) diameterFixture {
	edges := make([][2]int, 0, n)
	for i := 0; i < n; i++ {
		edges = append(edges, [2]int{i, (i + 1) % n})
	}
	return diameterFixture{name: name, order: n, edges: edges}
}

// diameterStar builds the undirected star with hub 0 and n-1 leaves; its true
// diameter is 2 (any leaf-to-leaf path via the hub) for n >= 3, or 1 for n == 2.
func diameterStar(name string, n int) diameterFixture {
	edges := make([][2]int, 0, n-1)
	for i := 1; i < n; i++ {
		edges = append(edges, [2]int{0, i})
	}
	return diameterFixture{name: name, order: n, edges: edges}
}

// diameterReference computes the true diameter of the undirected graph as the
// maximum finite eccentricity over all sources, via an all-pairs BFS that shares
// no code with search.Diameter.
func diameterReference(n int, undirected [][2]int) int {
	adj := symAdjacency(n, undirected)
	best := 0
	for src := 0; src < n; src++ {
		dist := bfsDistancesFromAdj(adj, n, src)
		for _, d := range dist {
			if d > best {
				best = d
			}
		}
	}
	return best
}

// diameterDiverge builds a single diameter divergence violation.
func diameterDiverge(tick int64, msg string) Violation {
	return Violation{Kind: ViolationSearchDivergence, Tick: tick, Op: "search:Diameter", Message: msg}
}
