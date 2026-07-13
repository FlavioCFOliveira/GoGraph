package sim

import (
	"fmt"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// centralityMeasureSalt keeps this checker's draw stream disjoint from the
// betweenness checker (which salts the same tick with centralitySeedSalt) and
// every other per-tick search check.
const centralityMeasureSalt uint64 = 0xa17e_9c3d_51b0_7e42

// pprMeasureSalt salts the tick for the personalised-PageRank fixture, keeping
// it independent of the other measure fixtures for the same tick.
const pprMeasureSalt uint64 = 0x6d2f_0a91_c4e8_3b57

// Tolerances for the CENTRALITY-measure battery. Closeness and harmonic are
// computed by the same float division on both sides, so they agree to a tight
// absolute+relative epsilon; eigenvector and Katz are power-iteration fixpoints,
// so the library (driven to a tight tolerance here) and the independent
// near-exact reference agree within a looser spectral epsilon. Comparing with a
// tolerance — never exact equality — is mandatory for any iterative float result.
const (
	closenessHarmonicEps = 1e-9
	spectralEps          = 1e-6
	spectralRefTol       = 1e-13
	spectralRefMaxIter   = 5000
	spectralLibMaxIter   = 1000
	katzMeasureAlpha     = 0.1 // < 1/λ_max for every fixture (densest is K5, λ_max = 4)
	katzMeasureBeta      = 1.0
)

// pprPushEpsilon is the push-residue threshold handed to
// [centrality.PersonalisedPushPageRank]. It is tight enough that the un-pushed
// residual mass (bounded by ~epsilon * Σ degree over a tiny fixture) is far
// below [pagerankEpsilon], so the push vector matches the exact PPR fixpoint the
// power-iteration reference computes, yet loose enough that the push terminates
// in a few hundred operations on these small graphs.
const pprPushEpsilon = 1e-7

// centralityMeasureViolations runs the CENTRALITY-measure battery for one tick:
// closeness, harmonic, eigenvector, and Katz over the same deterministic
// fixtures the betweenness battery uses ([centralityFixtures]), plus a
// personalised-PageRank fixture. Each measure is cross-checked against an
// INDEPENDENT reference derived from the measure's definition (never the
// library's code):
//
//   - Closeness / Harmonic: an all-pairs BFS reference over the fixture's own
//     out-adjacency reproduces the Wasserman-Faust closeness and the harmonic
//     sum exactly (both cheap, run on every fixture).
//   - Eigenvector / Katz: a dense power-iteration reference (I+A for eigenvector,
//     the Katz fixed point x = αAᵀx + β·1 for Katz) driven to a near-exact
//     residual. Eigenvector is well-defined only on a connected undirected graph
//     (Perron-Frobenius), so it is gated to those fixtures; Katz's β floor makes
//     it well-defined everywhere. Both additionally assert the library converged
//     strictly inside its iteration cap.
//   - Personalised PageRank: the local-push vector is compared against a damped
//     power-iteration PPR with a single-source teleport (dangling mass teleported
//     back to the source, matching the ACL model the push implements).
//
// All randomness flows from a single seed; the fixture set and order are fixed,
// so a divergence replays bit-for-bit. Divergences are tagged
// ViolationSearchDivergence with the measure's Op.
func centralityMeasureViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ centralityMeasureSalt)
	var vs []Violation
	for _, f := range centralityFixtures(seed) {
		c := centralityBuildCSR(f)
		vs = append(vs, closenessViolations(tick, f, c)...)
		vs = append(vs, harmonicViolations(tick, f, c)...)
		vs = append(vs, katzViolations(tick, f, c)...)
		if centralitySpectralApplicable(f) {
			vs = append(vs, eigenvectorViolations(tick, f, c)...)
		}
	}
	vs = append(vs, pprViolations(tick)...)
	return vs
}

// centralitySpectralApplicable reports whether eigenvector centrality is
// well-defined on f: a connected UNDIRECTED graph with at least one edge. On a
// directed acyclic fixture the measure is degenerate and on an edgeless one it
// is all-zero, both of which the library documents as out of scope for a
// meaningful cross-check. Every undirected fixture the battery builds is
// connected, so undirected + non-empty is the exact predicate.
func centralitySpectralApplicable(f centralityFixture) bool {
	return !f.directed && len(f.arcs) > 0
}

// --- Closeness / Harmonic ------------------------------------------------------

// closenessViolations cross-checks [centrality.Closeness] against the all-pairs
// BFS reference. The measure is length c.MaxNodeID(); the reference produces the
// same length from the fixture order.
func closenessViolations(tick int64, f centralityFixture, c *csr.CSR[float64]) []Violation {
	got := centrality.Closeness(c)
	want := closenessReference(f)
	return measureCompare(tick, "search:Closeness", f, want, got, closenessHarmonicEps, closenessHarmonicEps)
}

// harmonicViolations cross-checks [centrality.Harmonic] against the all-pairs
// BFS reference.
func harmonicViolations(tick int64, f centralityFixture, c *csr.CSR[float64]) []Violation {
	got := centrality.Harmonic(c)
	want := harmonicReference(f)
	return measureCompare(tick, "search:Harmonic", f, want, got, closenessHarmonicEps, closenessHarmonicEps)
}

// closenessReference computes Wasserman-Faust closeness for every vertex from
// scratch: for each source a BFS over the out-adjacency counts the reachable
// others r and their total hop distance Σd, giving C(u) = (r/(n-1))·(r/Σd), and
// 0 when the source reaches nobody. n is the fixture order (== c.Order()).
func closenessReference(f centralityFixture) []float64 {
	n := f.order
	adj, _ := centralityAdjacency(f)
	out := make([]float64, n)
	if n <= 1 {
		return out
	}
	for src := 0; src < n; src++ {
		reach, sumDist, _ := measureBFS(adj, n, src)
		if reach == 0 || sumDist == 0 {
			continue
		}
		r := float64(reach)
		out[src] = (r / float64(n-1)) * (r / float64(sumDist))
	}
	return out
}

// harmonicReference computes harmonic centrality for every vertex from scratch:
// H(u) = (Σ 1/d(u,v)) / (n-1) over the reachable others found by BFS.
func harmonicReference(f centralityFixture) []float64 {
	n := f.order
	adj, _ := centralityAdjacency(f)
	out := make([]float64, n)
	if n <= 1 {
		return out
	}
	norm := 1.0 / float64(n-1)
	for src := 0; src < n; src++ {
		_, _, recip := measureBFS(adj, n, src)
		out[src] = recip * norm
	}
	return out
}

// measureBFS runs a BFS from src over the out-adjacency and returns the number
// of reachable other nodes, the sum of their hop distances, and the sum of the
// reciprocal hop distances. It is the shared primitive for the closeness and
// harmonic references.
func measureBFS(adj [][]int, n, src int) (reach int, sumDist int64, recipSum float64) {
	dist := make([]int, n)
	for i := range dist {
		dist[i] = -1
	}
	dist[src] = 0
	queue := []int{src}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, v := range adj[u] {
			if dist[v] != -1 {
				continue
			}
			dist[v] = dist[u] + 1
			queue = append(queue, v)
			reach++
			sumDist += int64(dist[v])
			recipSum += 1.0 / float64(dist[v])
		}
	}
	return reach, sumDist, recipSum
}

// --- Eigenvector ---------------------------------------------------------------

// eigenvectorViolations cross-checks [centrality.Eigenvector] against the dense
// (I+A) power-iteration reference and asserts the library converged strictly
// inside its iteration cap. Both are driven to a tight tolerance so the L2-
// normalised dominant eigenvectors agree within [spectralEps].
func eigenvectorViolations(tick int64, f centralityFixture, c *csr.CSR[float64]) []Violation {
	got, iters, err := centrality.Eigenvector(c, centrality.EigenvectorOptions{
		MaxIterations: spectralLibMaxIter, Tolerance: spectralRefTol,
	})
	if err != nil {
		return centralityDiverge(tick, "search:Eigenvector", fmt.Sprintf(
			"%s: Eigenvector returned error on a connected undirected graph: %v", f.name, err))
	}
	if iters >= spectralLibMaxIter {
		return centralityDiverge(tick, "search:Eigenvector", fmt.Sprintf(
			"%s: Eigenvector did not converge within %d iterations", f.name, spectralLibMaxIter))
	}
	want := eigenvectorReference(f)
	return measureCompare(tick, "search:Eigenvector", f, want, got, spectralEps, spectralEps)
}

// eigenvectorReference computes the L2-normalised dominant eigenvector of A via
// the NetworkX (I+A) recurrence, accumulating each node's score from its
// IN-neighbours (matching the library), starting from a uniform positive vector
// over the live nodes and iterating to a near-exact residual. Using (I+A) rather
// than plain A converges on bipartite fixtures (path, even cycle) that plain
// power iteration would oscillate on. Independent of the CSR code path (it
// traverses the fixture adjacency directly).
func eigenvectorReference(f centralityFixture) []float64 {
	n := f.order
	adj, _ := centralityAdjacency(f)
	live, liveCount := measureLiveMask(f)
	cur := make([]float64, n)
	if liveCount == 0 {
		return cur
	}
	start := 1.0 / math.Sqrt(float64(liveCount))
	for i := 0; i < n; i++ {
		if live[i] {
			cur[i] = start
		}
	}
	next := make([]float64, n)
	for iter := 0; iter < spectralRefMaxIter; iter++ {
		copy(next, cur) // I·cur
		for u := 0; u < n; u++ {
			xs := cur[u]
			for _, v := range adj[u] {
				next[v] += xs // A·cur accumulated over in-edges (v gains u's score)
			}
		}
		var norm float64
		for _, v := range next {
			norm += v * v
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			return make([]float64, n)
		}
		inv := 1.0 / norm
		var change float64
		for i := range next {
			next[i] *= inv
			change += math.Abs(next[i] - cur[i])
		}
		cur, next = next, cur
		if change < float64(n)*spectralRefTol {
			break
		}
	}
	return cur
}

// --- Katz ----------------------------------------------------------------------

// katzViolations cross-checks [centrality.Katz] against the Katz fixed-point
// reference with the same explicit α and β, and asserts convergence inside the
// iteration cap. An explicit α (below 1/λ_max for every fixture) is passed so
// both sides use the identical attenuation and the auto-α path is not a source
// of divergence.
func katzViolations(tick int64, f centralityFixture, c *csr.CSR[float64]) []Violation {
	opts := centrality.KatzOptions{
		Alpha: katzMeasureAlpha, Beta: katzMeasureBeta,
		MaxIterations: spectralLibMaxIter, Tolerance: spectralRefTol,
	}
	got, iters, err := centrality.Katz(c, opts)
	if err != nil {
		return centralityDiverge(tick, "search:Katz", fmt.Sprintf(
			"%s: Katz returned error with explicit alpha=%g: %v", f.name, katzMeasureAlpha, err))
	}
	if iters >= spectralLibMaxIter {
		return centralityDiverge(tick, "search:Katz", fmt.Sprintf(
			"%s: Katz did not converge within %d iterations", f.name, spectralLibMaxIter))
	}
	want := katzReference(f, katzMeasureAlpha, katzMeasureBeta)
	return measureCompare(tick, "search:Katz", f, want, got, spectralEps, spectralEps)
}

// katzReference computes the L2-normalised Katz fixed point x = α·Aᵀ·x + β·1 on
// the live nodes from scratch, iterating to a near-exact residual. It mirrors
// the library's live-node seeding (β on participating nodes) and final L2
// normalisation, but traverses the fixture adjacency directly.
func katzReference(f centralityFixture, alpha, beta float64) []float64 {
	n := f.order
	adj, _ := centralityAdjacency(f)
	live, liveCount := measureLiveMask(f)
	cur := make([]float64, n)
	if liveCount == 0 {
		return cur
	}
	for i := 0; i < n; i++ {
		if live[i] {
			cur[i] = beta
		}
	}
	next := make([]float64, n)
	for iter := 0; iter < spectralRefMaxIter; iter++ {
		for i := 0; i < n; i++ {
			if live[i] {
				next[i] = beta
			} else {
				next[i] = 0
			}
		}
		for u := 0; u < n; u++ {
			xs := alpha * cur[u]
			for _, v := range adj[u] {
				next[v] += xs
			}
		}
		var change float64
		for i := 0; i < n; i++ {
			change += math.Abs(next[i] - cur[i])
		}
		cur, next = next, cur
		if change < float64(n)*spectralRefTol {
			break
		}
	}
	return normaliseL2Ref(cur)
}

// measureLiveMask reproduces the library's participating-node mask: a node is
// live iff it has at least one incident arc (as source or destination). It
// mirrors centrality.liveMask over the fixture's arc set.
func measureLiveMask(f centralityFixture) (live []bool, count int) {
	live = make([]bool, f.order)
	for _, a := range f.arcs {
		live[int(a.Src)] = true
		live[int(a.Dst)] = true
	}
	for _, l := range live {
		if l {
			count++
		}
	}
	return live, count
}

// normaliseL2Ref scales v to unit L2 norm in place and returns it (a zero vector
// is returned unchanged), mirroring the library's final normalisation.
func normaliseL2Ref(v []float64) []float64 {
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return v
	}
	inv := 1.0 / norm
	for i := range v {
		v[i] *= inv
	}
	return v
}

// --- Personalised push PageRank ------------------------------------------------

// pprViolations cross-checks [centrality.PersonalisedPushPageRank] seeded at
// node 0 against a damped power-iteration PPR reference with a single-source
// teleport. The push epsilon is tight ([pprPushEpsilon]) so the un-pushed
// residual is negligible relative to [pagerankEpsilon], and the fixture is a
// directed graph with a cycle and a dangling sink (reusing the PageRank
// generator) so the teleport and dangling-mass paths are exercised.
func pprViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ pprMeasureSalt)
	n, edges := pagerankGenGraph(seed)
	c := pagerankBuildCSR(n, edges)
	const src = 0

	got, err := centrality.PersonalisedPushPageRank(c, graph.NodeID(src), centrality.PPRPushOptions{
		Damping: 0.85, Epsilon: pprPushEpsilon,
	})
	if err != nil {
		return centralityDiverge(tick, "search:PersonalisedPushPageRank", fmt.Sprintf(
			"PersonalisedPushPageRank returned error on a well-formed graph: %v", err))
	}
	want := pprReference(n, edges, 0.85, src)

	var vs []Violation
	for v := 0; v < n; v++ {
		if v >= len(got) {
			break
		}
		if math.Abs(got[v]-want[v]) > pagerankEpsilon {
			vs = append(vs, Violation{
				Kind: ViolationSearchDivergence, Tick: tick, Op: "search:PersonalisedPushPageRank",
				Message: fmt.Sprintf("ppr[%d] got %.9f want %.9f (n=%d, |diff|=%.3g exceeds %g)",
					v, got[v], want[v], n, math.Abs(got[v]-want[v]), pagerankEpsilon),
			})
			break
		}
	}
	return vs
}

// pprReference computes the personalised-PageRank stationary vector π seeded at
// src by damped power iteration, mirroring the ACL model the push implements:
// walk with probability d, teleport to src with probability 1-d, and the mass
// held by dangling (out-degree-0) nodes teleports back to src. It iterates to a
// near-exact residual. This is the independent oracle the push vector is judged
// against.
func pprReference(n int, edges [][2]int, d float64, src int) []float64 {
	outAdj := make([][]int, n)
	outdeg := make([]int, n)
	for _, e := range edges {
		outAdj[e[0]] = append(outAdj[e[0]], e[1])
		outdeg[e[0]]++
	}
	pi := make([]float64, n)
	pi[src] = 1
	next := make([]float64, n)
	for iter := 0; iter < pagerankRefMaxIter; iter++ {
		var dangling float64
		for i := 0; i < n; i++ {
			if outdeg[i] == 0 {
				dangling += pi[i]
			}
		}
		for i := range next {
			next[i] = 0
		}
		next[src] += 1 - d        // teleport to the seed
		next[src] += d * dangling // dangling mass teleports to the seed
		for u := 0; u < n; u++ {
			if outdeg[u] == 0 {
				continue
			}
			share := d * pi[u] / float64(outdeg[u])
			for _, v := range outAdj[u] {
				next[v] += share
			}
		}
		var delta float64
		for i := 0; i < n; i++ {
			delta += math.Abs(next[i] - pi[i])
		}
		pi, next = next, pi
		if delta < pagerankRefTolerance {
			break
		}
	}
	return pi
}

// --- Comparison ----------------------------------------------------------------

// measureCompare checks a per-node measure result (got) against the reference
// (want) within a combined absolute+relative epsilon, returning a violation for
// the first NodeID that disagrees (ascending id, so the report is deterministic)
// or a length-parity violation. It reuses the tolerance shape of the betweenness
// comparison ([centralityApproxEqualEps]).
func measureCompare(tick int64, op string, f centralityFixture, want, got []float64, absEps, relEps float64) []Violation {
	if len(want) != len(got) {
		return centralityDiverge(tick, op, fmt.Sprintf(
			"%s: result length = %d, want %d", f.name, len(got), len(want)))
	}
	for v := range want {
		if !centralityApproxEqualEps(want[v], got[v], absEps, relEps) {
			return centralityDiverge(tick, op, fmt.Sprintf(
				"%s: value[%d] = %.17g, reference = %.17g (|diff| = %.3g exceeds abs %g + rel %g)",
				f.name, v, got[v], want[v], math.Abs(got[v]-want[v]), absEps, relEps))
		}
	}
	return nil
}

// centralityApproxEqualEps reports whether a and b agree within the given
// combined absolute+relative tolerance (the parameterised form of
// [centralityApproxEqual]).
func centralityApproxEqualEps(a, b, absEps, relEps float64) bool {
	diff := math.Abs(a - b)
	if diff <= absEps {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff <= relEps*scale
}
