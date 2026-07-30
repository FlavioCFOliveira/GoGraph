// Package cyclicjoin holds the permanent benchmarks for the fused cyclic expand
// (rmp #2157, measured under #2159).
//
// # Both arms run in ONE process
//
// Every benchmark here measures the fused operator against the two-Expand plan
// INSIDE a single binary, toggled by EngineOptions.EnableCyclicIntersect, rather
// than by comparing two commits. That is deliberate and follows sprint 313's
// finding: `bench-history.sh` compares runs back-to-back by construction, and on
// this machine a byte-identical control produced 22 of 36 "significant" rows
// spanning −11%…+4%, inventing two phantom regressions. A same-process A/B is
// immune to that class of error because both arms see the same binary, the same
// fixture and the same thermal state, and `-count` interleaves them.
//
// The consequence for the record: a same-process run must NOT take a numbered
// LEDGER row. Sprint 314's row is deliberately unnumbered for this reason —
// `bench-history.sh` uses the previous numbered file as its baseline, so a
// heterogeneous benchmark set in that chain leaves the next curated run with no
// common benchmark names and an empty comparison.
//
// # The queries must be UNLABELLED
//
// bench/expandinto's queries are labelled (`(a:P)-[:K]->…`) and would measure
// nothing here: a label predicate interposes a Selection between the hops, so
// ir.Expand.Child is not an *ir.Expand and the fusion correctly DECLINES. The
// fusing queries below are therefore type-only or untyped. LabelledTriangleQuery is
// kept precisely as the non-qualifying control that proves the predicate leaves
// declined shapes untouched.
//
// # What the claim is, and what it is NOT
//
// The sprint was opened on `Θ(m²) → Θ(m^1.5)`. SPIKE #2155 REFUTED that as a
// description of GoGraph's cost: the per-graph work terms are
// `Σ_v d_in(v)·d_out(v)` for the binary-join plan and
// `Σ_(a,b) min(d_out(b), d_in(a))` for the intersection, and those are EXACTLY
// EQUAL on any regular graph. Measured exponents in m were 1.000/1.000 on a
// uniform fixture and 1.112/1.008 on a power-law one. So these benchmarks do not
// try to demonstrate an asymptotic win. They measure what the SPIKE established is
// actually available — fewer materialised intermediates and a sequential merge in
// place of a per-candidate probe — and they report the fitted exponents so the
// record states the shape of the cost rather than a single ratio.
package cyclicjoin

import (
	"fmt"
	"math/rand"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TriangleQuery is the headline fusing shape: a directed 3-cycle whose last two
// hops fuse. Type-only, so no Selection is interposed.
const TriangleQuery = `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`

// ClosingQuery is the 2-cycle — the motivating audit's own §2.3 shape. It fuses
// too, which was not anticipated when the operator was written.
const ClosingQuery = `MATCH (a)-[:K]->(b)-[:K]->(a) RETURN count(*) AS n`

// SquareQuery closes a 4-cycle. Only its LAST hop closes, so the fusion is the
// same single 2-way intersection as the triangle's — measured to keep on record
// that a longer cycle adds open hops, not intersections.
const SquareQuery = `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(d)-[:K]->(a) RETURN count(*) AS n`

// LabelledTriangleQuery is a NON-QUALIFYING control: the label predicates
// interpose a Selection between the hops, so the fusion declines and both arms
// must measure the same plan.
const LabelledTriangleQuery = `MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*) AS n`

// AcyclicQuery is the other non-qualifying control: nothing closes, so no hop ever
// carries IntoVar and the predicate cannot fire.
const AcyclicQuery = `MATCH (a)-[:K]->(b)-[:K]->(c) RETURN count(*) AS n`

// SeedUniform builds a ring of n nodes where node k has out-edges to k+1 … k+degree
// plus a back-edge to k−1, so every node has the same in- and out-degree and
// triangles genuinely exist.
//
// A uniform-degree fixture is the honest FLOOR for this operator, not a flattering
// case: the SPIKE proved the two plans' work terms are exactly equal here, so
// whatever this measures is the constant-factor and materialisation difference with
// no skew advantage whatsoever.
func SeedUniform(n, degree int) (*lpg.Graph[string, float64], error) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = "n" + itoa(i)
		if err := g.AddNode(keys[i]); err != nil {
			return nil, fmt.Errorf("AddNode: %w", err)
		}
		if err := g.SetNodeLabel(keys[i], "P"); err != nil {
			return nil, fmt.Errorf("SetNodeLabel: %w", err)
		}
	}
	link := func(i, j int) error {
		if err := g.AddEdge(keys[i], keys[j], 1.0); err != nil {
			return fmt.Errorf("AddEdge: %w", err)
		}
		g.SetEdgeLabel(keys[i], keys[j], "K")
		return nil
	}
	for i := 0; i < n; i++ {
		for d := 1; d <= degree; d++ {
			if err := link(i, (i+d)%n); err != nil {
				return nil, err
			}
		}
		if err := link(i, (i-1+n)%n); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// SeedPowerLaw builds a Holme-Kim style graph — preferential attachment with
// triadic closure — which yields a heavy-tailed degree distribution AND real
// triangles. This is the realistic social shape, and the only regime in which the
// SPIKE measured any asymptotic separation at all (exponent 1.112 against 1.008).
//
// The RNG is explicitly seeded so the fixture is deterministic and the numbers are
// reproducible; Date.now-style nondeterminism in a benchmark fixture would make
// every recorded figure unreproducible.
func SeedPowerLaw(n, mEdges int, triadP float64, seed int64) (*lpg.Graph[string, float64], error) {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic fixture, not security
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = "n" + itoa(i)
		if err := g.AddNode(keys[i]); err != nil {
			return nil, fmt.Errorf("AddNode: %w", err)
		}
		if err := g.SetNodeLabel(keys[i], "P"); err != nil {
			return nil, fmt.Errorf("SetNodeLabel: %w", err)
		}
	}
	repeated := make([]int, 0, 2*n*mEdges)
	adjacency := make([][]int, n)
	linked := make(map[[2]int]bool, n*mEdges)

	addPair := func(u, v int) error {
		if u == v || linked[[2]int{u, v}] {
			return nil
		}
		linked[[2]int{u, v}] = true
		linked[[2]int{v, u}] = true
		for _, e := range [][2]int{{u, v}, {v, u}} {
			if err := g.AddEdge(keys[e[0]], keys[e[1]], 1.0); err != nil {
				return fmt.Errorf("AddEdge: %w", err)
			}
			g.SetEdgeLabel(keys[e[0]], keys[e[1]], "K")
		}
		repeated = append(repeated, u, v)
		adjacency[u] = append(adjacency[u], v)
		adjacency[v] = append(adjacency[v], u)
		return nil
	}

	seedClique := mEdges + 1
	if seedClique > n {
		seedClique = n
	}
	for i := 0; i < seedClique; i++ {
		for j := i + 1; j < seedClique; j++ {
			if err := addPair(i, j); err != nil {
				return nil, err
			}
		}
	}
	for v := seedClique; v < n; v++ {
		last := -1
		for e := 0; e < mEdges; e++ {
			target := -1
			if last >= 0 && len(adjacency[last]) > 0 && rng.Float64() < triadP {
				target = adjacency[last][rng.Intn(len(adjacency[last]))]
			}
			if target < 0 || target == v || linked[[2]int{v, target}] {
				if len(repeated) == 0 {
					break
				}
				target = repeated[rng.Intn(len(repeated))]
			}
			if target == v || linked[[2]int{v, target}] {
				continue
			}
			if err := addPair(v, target); err != nil {
				return nil, err
			}
			last = target
		}
	}
	return g, nil
}

// itoa avoids a strconv import in a hot fixture loop.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
