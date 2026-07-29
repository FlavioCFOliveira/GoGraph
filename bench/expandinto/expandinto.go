// Package expandinto holds the permanent benchmarks for the bound-destination
// expand seek (rmp #2149) and the symmetric anchor swap (#2150), measured under
// #2152.
//
// # Why both arms run in ONE process
//
// Every benchmark here measures the two access paths against each other INSIDE a
// single binary, toggled by EngineOptions, rather than by comparing two commits.
// That is deliberate. The project's `bench-history.sh` recorder compares runs
// back-to-back by construction, and sprint 313 established empirically that a
// back-to-back A/B on this machine manufactures significance: a byte-identical
// control produced 22 of 36 "significant" rows spanning −11%…+4%, inventing two
// phantom regressions. A same-process A/B is immune to that class of error,
// because both arms see the same binary, the same fixture and the same thermal
// state, and `-count` interleaves them.
//
// # Why the sweep, not a point
//
// The claim this sprint makes about the closing hop is ASYMPTOTIC — Θ(d) per input
// row becoming O(log d + r) — not a constant factor. A single degree cannot show
// that, so the harness sweeps out-degree and [FitExponent] fits the growth
// exponent from the series. The exponent is the claim; individual points are
// evidence for it.
//
// # What the audit's numbers are worth
//
// The motivating audit (docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md §2.3)
// reported 625.8 ms / 9.68 M allocs at out-degree 8 and 41.68 s / 577.9 M allocs
// at 64, an exponent of 2.02. Those points are NOT reproducible at HEAD and must
// not be used as a baseline: they predate #2206, which stopped the operator
// building a row per neighbour, and #2142, which made the read-path probes
// O(log d). Re-running the audit's own harness at the sprint-314 base measured
// 71.98 ms / 686 K allocs and 2.981 s / 6.54 M allocs — 8.7× and 14.0× faster,
// with 14× and 88× fewer allocations — for an exponent of 1.79. The disabled arm
// of these benchmarks is the honest baseline.
package expandinto

import (
	"fmt"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ClosingQuery is the audit's §2.3 shape: a 2-cycle whose second hop has an
// already-bound destination. It is the shape the seek exists for, and the one
// whose end-to-end exponent the seek can actually move — see [TriangleQuery].
const ClosingQuery = `MATCH (a:P)-[:K]->(b:P)-[:K]->(a) RETURN count(*) AS n`

// TriangleQuery closes a 3-cycle. Its middle hop is OPEN and therefore
// materialises Θ(n·d²) intermediate rows no matter how the closing hop is
// executed, so its end-to-end exponent stays near 2 even with the seek engaged.
// It is measured to keep that limit on the record rather than to show a win: a
// blended exponent across both shapes would misstate the result.
const TriangleQuery = `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*) AS n`

// OpenControlQuery is the same two hops with the destination left free, so no
// bound-destination path applies. It bounds how much of a paired difference is the
// fixture rather than the access path.
const OpenControlQuery = `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P) RETURN count(*) AS n`

// SeedRing builds a labelled multigraph of n nodes, every node :P with an integer
// property i, where node k has out-edges of type K to k+1 … k+degree modulo n.
//
// mutual additionally adds the back-edge k → k−1, which is what makes 2-cycles
// and triangles genuinely EXIST. Without it a ring of small degree closes no
// cycle at all — d1+d2 < n — so the closing queries match nothing and the
// benchmark measures the wasted enumeration only. Both are worth measuring: pass
// false for the pure-waste worst case (the audit's own fixture) and true for a
// fixture that also emits rows.
func SeedRing(n, degree int, mutual bool) (*lpg.Graph[string, float64], error) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		k := "n" + itoa(i)
		keys[i] = k
		if err := g.AddNode(k); err != nil {
			return nil, fmt.Errorf("AddNode(%s): %w", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			return nil, fmt.Errorf("SetNodeLabel(%s): %w", k, err)
		}
		if err := g.SetNodeProperty(k, "i", lpg.Int64Value(int64(i))); err != nil {
			return nil, fmt.Errorf("SetNodeProperty(%s): %w", k, err)
		}
	}
	for i := 0; i < n; i++ {
		for d := 1; d <= degree; d++ {
			j := (i + d) % n
			if err := g.AddEdge(keys[i], keys[j], 1.0); err != nil {
				return nil, fmt.Errorf("AddEdge(%s->%s): %w", keys[i], keys[j], err)
			}
			g.SetEdgeLabel(keys[i], keys[j], "K")
		}
		if mutual {
			j := (i - 1 + n) % n
			if err := g.AddEdge(keys[i], keys[j], 1.0); err != nil {
				return nil, fmt.Errorf("AddEdge back(%s->%s): %w", keys[i], keys[j], err)
			}
			g.SetEdgeLabel(keys[i], keys[j], "K")
		}
	}
	return g, nil
}

// SeedReverseHub builds the anchor-swap fixture: one :Hub node with hubOut
// outgoing R-edges — all but one to :Other nodes — and nLeaf :Leaf nodes, exactly
// one of which receives an R-edge from the Hub.
//
// The written form `MATCH (a:Hub)-[:R]->(b:Leaf)` anchors the Hub and walks every
// one of its out-edges. Re-rooting onto Leaf walks the single in-edge instead, at
// the price of scanning nLeaf nodes and probing the Hub's forward range for that
// edge. Before #2150 that re-rooting was vetoed for being reverse-introducing.
func SeedReverseHub(hubOut, nLeaf int) (*lpg.Graph[string, float64], error) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("hub"); err != nil {
		return nil, fmt.Errorf("AddNode(hub): %w", err)
	}
	if err := g.SetNodeLabel("hub", "Hub"); err != nil {
		return nil, fmt.Errorf("SetNodeLabel(Hub): %w", err)
	}
	if err := g.SetNodeProperty("hub", "tag", lpg.Int64Value(0)); err != nil {
		return nil, fmt.Errorf("SetNodeProperty(tag): %w", err)
	}
	for i := 0; i < hubOut-1; i++ {
		k := "o" + itoa(i)
		if err := g.AddNode(k); err != nil {
			return nil, fmt.Errorf("AddNode(%s): %w", k, err)
		}
		if err := g.SetNodeLabel(k, "Other"); err != nil {
			return nil, fmt.Errorf("SetNodeLabel(Other): %w", err)
		}
		if err := g.AddEdge("hub", k, 1.0); err != nil {
			return nil, fmt.Errorf("AddEdge(hub->%s): %w", k, err)
		}
		g.SetEdgeLabel("hub", k, "R")
	}
	for i := 0; i < nLeaf; i++ {
		k := "l" + itoa(i)
		if err := g.AddNode(k); err != nil {
			return nil, fmt.Errorf("AddNode(%s): %w", k, err)
		}
		if err := g.SetNodeLabel(k, "Leaf"); err != nil {
			return nil, fmt.Errorf("SetNodeLabel(Leaf): %w", err)
		}
		if err := g.SetNodeProperty(k, "i", lpg.Int64Value(int64(i))); err != nil {
			return nil, fmt.Errorf("SetNodeProperty(i): %w", err)
		}
	}
	if err := g.AddEdge("hub", "l0", 1.0); err != nil {
		return nil, fmt.Errorf("AddEdge(hub->l0): %w", err)
	}
	g.SetEdgeLabel("hub", "l0", "R")
	return g, nil
}

// SwapQuery is the single-edge pattern whose cheaper anchor requires a reverse
// expand, so it is admissible only since #2150.
const SwapQuery = `MATCH (a:Hub)-[:R]->(b:Leaf) RETURN a.tag AS at, b.i AS bi`

// FitExponent returns the least-squares slope of log(cost) against log(degree) —
// the empirical growth exponent — over parallel degree and cost series.
//
// It is the figure the sprint's claim is made in, so it is computed here rather
// than eyeballed from a table: an exponent read off two endpoints is at the mercy
// of whichever endpoint carries the most fixed cost, and a least-squares fit over
// the whole sweep is not.
//
// Degrees and costs must be the same length, at least two points, all strictly
// positive, and the degrees must contain at least two DISTINCT values. It returns
// NaN otherwise rather than a plausible-looking number.
//
// The distinctness check is explicit and not left to the denominator. With no
// spread in x the slope is mathematically undefined, but `n·Σx² − (Σx)²` does not
// evaluate to exactly zero in floating point — for degrees {4, 4} it rounds to a
// tiny non-zero value and the division returns a finite, entirely plausible 0.25.
// A harness that reports a confident number for an undefined quantity is precisely
// the failure this package's comments warn about, so the degenerate case is
// rejected on the inputs instead.
func FitExponent(degrees []int, costs []float64) float64 {
	if len(degrees) != len(costs) || len(degrees) < 2 {
		return math.NaN()
	}
	var sx, sy, sxx, sxy float64
	n := float64(len(degrees))
	distinct := false
	for i, d := range degrees {
		if d <= 0 || costs[i] <= 0 {
			return math.NaN()
		}
		if d != degrees[0] {
			distinct = true
		}
		x, y := math.Log(float64(d)), math.Log(costs[i])
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	if !distinct {
		return math.NaN()
	}
	den := n*sxx - sx*sx
	if den == 0 || math.IsNaN(den) || math.IsInf(den, 0) {
		return math.NaN()
	}
	return (n*sxy - sx*sy) / den
}

// itoa avoids pulling strconv into a hot fixture loop's import set for one call.
func itoa(i int) string { return fmt.Sprintf("%d", i) }
