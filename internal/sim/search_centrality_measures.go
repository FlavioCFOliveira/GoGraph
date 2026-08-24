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

// Op labels for the SHIPPED-DEFAULT regimes, kept distinct from the
// literal-option ones so a divergence report names WHICH regime diverged (and
// so the shrinker's violation signature does not conflate the two).
const (
	opEigenvectorDefaults = "search:Eigenvector/defaults"
	opKatzDefaults        = "search:Katz/defaults"
	opPPRDefaults         = "search:PersonalisedPushPageRank/defaults"
)

// katzAutoAlphaNumerator is the constant [centrality.KatzOptions] documents for
// the auto-selected attenuation factor, alpha = 0.85 / (1 + maxInDegree). It is
// restated here so [katzAutoAlphaReference] derives the value from the
// FIXTURE's own arcs rather than reading it back out of the library.
const katzAutoAlphaNumerator = 0.85

// Tolerances for the SHIPPED-DEFAULT regimes. These are deliberately NOT the
// literal-option tolerances above. The shipped defaults stop far earlier than
// the hand-written options this file also drives — tolerance 1e-6 instead of
// [spectralRefTol] for the two spectral measures, push epsilon 1e-6 instead of
// [pprPushEpsilon] — so the residual gap to the near-exact reference is
// correspondingly larger and reusing [spectralEps] would simply make the checks
// fail. Every constant below was MEASURED before it was chosen; the note on
// each records the observed maximum, the headroom, and the size of defect it
// still catches. Re-measure if the fixture set changes.
const (
	// eigenvectorDefaultEps bounds the gap between [centrality.Eigenvector]
	// under [centrality.DefaultEigenvectorOptions] (100 iterations, tolerance
	// 1e-6) and the near-exact [eigenvectorReference].
	//
	// Measured: worst |difference| 5.75e-06 (fixture random-bridged-5-4) over
	// 25,000 runs — 5,000 ticks x the 5 undirected fixtures the measure applies
	// to. 1e-4 leaves 17.4x headroom. That sweep is EXHAUSTIVE rather than a
	// sample: eigenvector centrality reads only the adjacency (arc weights are
	// ignored), and [centralityFixtures] yields exactly 23 distinct shapes —
	// 7 fixed plus the 16 (a,b) size combinations of [centralityRandomBridged].
	// 20 of those 23 are undirected and non-empty, which is the subset
	// [centralitySpectralApplicable] admits, and the sweep covered every one.
	//
	// Still catches: the vector is L2-normalised over at most 12 live nodes, so
	// a typical component is 0.1-0.7. Against a component of 0.4, 1e-4 flags any
	// error above 2.5e-4 relative (0.025%) — far below the 1e-2-and-larger
	// shifts caused by the defects that matter here: the wrong edge orientation,
	// a mis-seeded start vector, a live-mask entry dropped, a self-loop or
	// parallel edge lost from A.
	eigenvectorDefaultEps = 1e-4

	// katzDefaultEps bounds the gap between [centrality.Katz] under
	// [centrality.DefaultKatzOptions] (auto alpha, beta 1, 1000 iterations,
	// tolerance 1e-6) and [katzReference] driven with the same independently
	// re-derived alpha.
	//
	// Measured: worst |difference| 1.00e-07 (fixture random-bridged-5-4) over
	// 40,000 runs — 5,000 ticks x all 8 fixtures, Katz being well-defined
	// everywhere. 1e-6 leaves 9.96x headroom. Exhaustive for the same reason as
	// above: Katz also reads only the adjacency. Katz converges far closer than
	// eigenvector under the same 1e-6 tolerance because the auto alpha makes the
	// iteration a hard contraction (rho = alpha*lambda_max < 0.85), which is why
	// this constant is two orders tighter than [eigenvectorDefaultEps].
	//
	// Still catches: against a component of 0.4, any error above 2.5e-6 relative
	// (0.00025%) — which includes a beta floor applied to the wrong node set, a
	// transposed accumulation, and any change to the auto-alpha formula large
	// enough to move the fixed point at all.
	katzDefaultEps = 1e-6

	// pprDefaultEps bounds the gap between
	// [centrality.PersonalisedPushPageRank] under
	// [centrality.DefaultPPRPushOptions] (damping 0.85, epsilon 1e-6, 1e7 steps)
	// and the exact [pprReference] fixpoint.
	//
	// Measured: worst |difference| 3.06e-06 over 5,000 runs, so 1e-4 leaves
	// 32.7x headroom on the observation. Unlike the two spectral measures this sweep
	// is a SAMPLE — the PPR fixture varies in order (n in [5,9]) and in its
	// seed-chosen extra arcs — so the constant is placed above the STRUCTURAL
	// bound instead of merely above the sample: local push leaves at most
	// epsilon*deg(v) of residue un-pushed at each node, hence at most
	// epsilon*|E| of mass unaccounted for overall, and |E| never exceeded 17
	// across the sweep, giving 1e-6 * 17 = 1.7e-05. 1e-4 sits 5.9x above that
	// worst case the algorithm is permitted to leave behind.
	//
	// Still catches: the vector is a probability distribution summing to 1 with
	// the seed itself holding ~0.46, so 1e-4 flags the misplacement of 0.01% of
	// the total mass — far below a dangling-mass teleport sent to the wrong
	// node, a wrong damping factor, or an edge dropped from a push.
	pprDefaultEps = 1e-4
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
// Eigenvector, Katz and personalised PageRank are each driven TWICE: once with
// the hand-written literal options above, and once more under the regime the
// library actually ships — [centrality.DefaultEigenvectorOptions],
// [centrality.DefaultKatzOptions], [centrality.DefaultPPRPushOptions]. The two
// are not interchangeable. The literal options drive each algorithm far past its
// shipped stopping point (tolerance 1e-13 against 1e-6, push epsilon 1e-7
// against 1e-6, and an explicit Katz alpha instead of the auto-selected one), so
// a check that only ever ran them would leave the configuration every ordinary
// caller gets — including the auto-alpha branch and the 100-iteration
// eigenvector cap — entirely undriven. Both regimes are judged against the SAME
// independent references, each within its own measured tolerance
// ([eigenvectorDefaultEps], [katzDefaultEps], [pprDefaultEps]).
//
// All randomness flows from a single seed; the fixture set and order are fixed,
// so a divergence replays bit-for-bit. Divergences are tagged
// ViolationSearchDivergence with the measure's Op.
func centralityMeasureViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ centralityMeasureSalt)
	// prealloc would have this be make([]Violation, 0, <fixture count>), which
	// allocates a backing array on EVERY tick. A clean tick — the overwhelmingly
	// common case for a checker the simulator runs on every step — should return
	// a nil slice rather than a heap-allocated empty one, and the fixture count
	// is not the violation count in any case: each fixture contributes zero
	// violations whenever the engine agrees. Appending lazily leaves vs nil on
	// that path.
	var vs []Violation //nolint:prealloc // a clean tick must not allocate; see above
	for _, f := range centralityFixtures(seed) {
		c := centralityBuildCSR(f)
		vs = append(vs, centralityFixtureViolations(tick, f, c)...)
	}
	vs = append(vs, pprMeasureViolations(tick)...)
	return vs
}

// pprMeasureViolations runs BOTH personalised-PageRank regimes for one tick —
// the literal-option check ([pprViolations]) and the shipped-default one
// ([pprDefaultRegimeViolations]) — against the graph the tick's own seed derives.
//
// They are grouped behind a single call deliberately. A top-level "append the
// result of check X" line is invisible to every test here once deleted: nothing
// diverges on a clean fixture, so nothing goes red, and a whole check
// disappears silently. Grouping means the default regime cannot be dropped on
// its own — the only line that removes it removes the long-standing literal
// check with it — and the grouping itself is injection-tested through
// [pprMeasureRegimeViolations].
func pprMeasureViolations(tick int64) []Violation {
	seed := NewSeed(uint64(tick) ^ pprMeasureSalt)
	n, edges := pagerankGenGraph(seed)
	return pprMeasureRegimeViolations(tick, pagerankBuildCSR(n, edges), n, edges)
}

// pprMeasureRegimeViolations is the body of [pprMeasureViolations], taking the
// ENGINE's input (the CSR c) and the REFERENCE's input (n, edges) separately so
// a test can hand the two different graphs and observe which regimes speak up.
// [pprViolations] derives its own fixture from the tick and is left exactly as
// it was, so an injection here moves only the default regime.
func pprMeasureRegimeViolations(tick int64, c *csr.CSR[float64], n int, edges [][2]int) []Violation {
	vs := pprViolations(tick)
	return append(vs, pprDefaultRegimeViolations(tick, c, n, edges)...)
}

// centralityFixtureViolations runs every per-fixture measure check against one
// fixture: closeness, harmonic, Katz in both its literal and its shipped-default
// regime, and — where the measure is well-defined — eigenvector in both regimes.
//
// The CSR arrives as a parameter rather than being built here so that a test can
// hand the ENGINE one graph while the references describe another and observe
// which checks speak up. That is the wiring assertion: it is what stops a check
// being silently dropped from this list, which no "clean on fixtures" run could
// ever detect on its own.
func centralityFixtureViolations(tick int64, f centralityFixture, c *csr.CSR[float64]) []Violation {
	vs := closenessViolations(tick, f, c)
	vs = append(vs, harmonicViolations(tick, f, c)...)
	vs = append(vs, katzViolations(tick, f, c)...)
	vs = append(vs, katzDefaultViolations(tick, f, c)...)
	if centralitySpectralApplicable(f) {
		vs = append(vs, eigenvectorViolations(tick, f, c)...)
		vs = append(vs, eigenvectorDefaultViolations(tick, f, c)...)
	}
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

// --- Shipped default regimes ---------------------------------------------------
//
// The three checks below drive the SAME algorithms as the literal-option checks
// above, but configured exactly as [centrality.DefaultEigenvectorOptions],
// [centrality.DefaultKatzOptions] and [centrality.DefaultPPRPushOptions] ship
// them. They are additional, never a replacement: the literal options remain the
// tight regime that pins each algorithm against a near-exact fixpoint, while
// these pin the configuration an ordinary caller actually receives — a looser
// stopping rule, a much smaller eigenvector iteration budget, and (for Katz) the
// auto-selected attenuation factor that the literal regime deliberately bypasses.
//
// Each is judged against the SAME independent reference the literal check uses.
// The references are exact fixpoints, so they are regime-independent by
// construction; only the tolerance differs, and each tolerance was measured
// rather than guessed (see [eigenvectorDefaultEps] and its siblings).

// eigenvectorDefaultViolations cross-checks [centrality.Eigenvector] under the
// shipped [centrality.DefaultEigenvectorOptions] against the same near-exact
// [eigenvectorReference] the literal-option check uses, within the measured
// [eigenvectorDefaultEps].
//
// It also asserts the library converged STRICTLY inside the shipped iteration
// cap. That is a real claim about the default, not a formality: the default caps
// the power iteration at 100 steps — a tenth of the literal regime's budget —
// so a fixture with a small enough spectral gap could legitimately exhaust it
// and return [centrality.ErrMaxStepsExceeded]. It was measured before being
// asserted: across 25,000 runs (5,000 ticks x the 5 applicable fixtures, which
// covers every distinct shape the fixture family can produce) the worst case
// used 45 iterations, 2.2x inside the cap, and the cap was never reached. A
// future fixture with a tighter spectral gap would trip this assertion, and that
// is the correct outcome: it means the shipped default no longer converges on
// the battery, which is a fact the DST should surface rather than tolerate.
func eigenvectorDefaultViolations(tick int64, f centralityFixture, c *csr.CSR[float64]) []Violation {
	opts := centrality.DefaultEigenvectorOptions()
	got, iters, err := centrality.Eigenvector(c, opts)
	if err != nil {
		return centralityDiverge(tick, opEigenvectorDefaults, fmt.Sprintf(
			"%s: Eigenvector under DefaultEigenvectorOptions (max %d iterations, tolerance %g) returned an error on a connected undirected graph: %v",
			f.name, opts.MaxIterations, opts.Tolerance, err))
	}
	if iters >= opts.MaxIterations {
		return centralityDiverge(tick, opEigenvectorDefaults, fmt.Sprintf(
			"%s: Eigenvector under DefaultEigenvectorOptions did not converge strictly inside the shipped cap of %d iterations (used %d)",
			f.name, opts.MaxIterations, iters))
	}
	want := eigenvectorReference(f)
	return measureCompare(tick, opEigenvectorDefaults, f, want, got, eigenvectorDefaultEps, eigenvectorDefaultEps)
}

// katzDefaultViolations cross-checks [centrality.Katz] under the shipped
// [centrality.DefaultKatzOptions] — whose Alpha is the 0 sentinel that selects
// the attenuation factor automatically — and makes two distinct assertions.
//
//  1. CORRECTNESS. The result must agree with [katzReference] within the
//     measured [katzDefaultEps]. The reference is driven with alpha
//     re-derived from the fixture's own arcs by [katzAutoAlphaReference], never
//     read back from the library, so the comparison stays an independent oracle.
//
//  2. THE AUTO-ALPHA CONTRACT. Handing that same re-derived alpha back as an
//     EXPLICIT option must reproduce the auto run bit for bit — identical
//     iteration count, identical scores. This is a differential assertion about
//     a documented contract, not a correctness oracle: it is the only check that
//     can pin the exact formula [centrality.KatzOptions] publishes, because the
//     chosen alpha is never surfaced in the result. Bit-exactness is the right
//     predicate because both runs then execute the identical serial code on
//     identical scalars; it was measured over 40,000 runs with zero mismatches.
//     The two assertions are complementary: (1) alone would miss a formula
//     change too small to move the fixed point past the tolerance, and (2) alone
//     would be satisfied by any formula at all as long as both paths used it.
//
// Convergence inside the iteration cap is asserted as well; the shipped cap is
// 1000 and the observed worst case was 31 iterations.
func katzDefaultViolations(tick int64, f centralityFixture, c *csr.CSR[float64]) []Violation {
	opts := centrality.DefaultKatzOptions()
	got, iters, err := centrality.Katz(c, opts)
	if err != nil {
		return centralityDiverge(tick, opKatzDefaults, fmt.Sprintf(
			"%s: Katz under DefaultKatzOptions (auto alpha, beta %g, max %d iterations, tolerance %g) returned an error: %v",
			f.name, opts.Beta, opts.MaxIterations, opts.Tolerance, err))
	}
	if iters >= opts.MaxIterations {
		return centralityDiverge(tick, opKatzDefaults, fmt.Sprintf(
			"%s: Katz under DefaultKatzOptions did not converge strictly inside the shipped cap of %d iterations (used %d)",
			f.name, opts.MaxIterations, iters))
	}

	alpha := katzAutoAlphaReference(f)
	if vs := katzAutoAlphaContract(tick, f, c, opts, alpha, got, iters); len(vs) != 0 {
		return vs
	}

	want := katzReference(f, alpha, opts.Beta)
	return measureCompare(tick, opKatzDefaults, f, want, got, katzDefaultEps, katzDefaultEps)
}

// katzAutoAlphaContract asserts that re-running [centrality.Katz] with the
// independently re-derived alpha supplied EXPLICITLY reproduces the auto-alpha
// run exactly. See assertion (2) in [katzDefaultViolations] for why this is a
// contract pin rather than a correctness oracle.
func katzAutoAlphaContract(tick int64, f centralityFixture, c *csr.CSR[float64], opts centrality.KatzOptions, alpha float64, autoScores []float64, autoIters int) []Violation {
	explicit := opts
	explicit.Alpha = alpha
	got, iters, err := centrality.Katz(c, explicit)
	if err != nil {
		return centralityDiverge(tick, opKatzDefaults, fmt.Sprintf(
			"%s: Katz rejected the re-derived auto alpha %.17g supplied explicitly: %v", f.name, alpha, err))
	}
	if iters != autoIters {
		return centralityDiverge(tick, opKatzDefaults, fmt.Sprintf(
			"%s: auto-alpha contract broken: explicit alpha %.17g converged in %d iterations, the auto path in %d",
			f.name, alpha, iters, autoIters))
	}
	if len(got) != len(autoScores) {
		return centralityDiverge(tick, opKatzDefaults, fmt.Sprintf(
			"%s: auto-alpha contract broken: explicit-alpha result length %d, auto-path length %d",
			f.name, len(got), len(autoScores)))
	}
	for i := range got {
		if got[i] != autoScores[i] {
			return centralityDiverge(tick, opKatzDefaults, fmt.Sprintf(
				"%s: auto-alpha contract broken: value[%d] = %.17g with explicit alpha %.17g, %.17g on the auto path (0.85/(1+maxInDegree) is no longer the selected attenuation)",
				f.name, i, got[i], alpha, autoScores[i]))
		}
	}
	return nil
}

// katzAutoAlphaReference re-derives, from the FIXTURE's own arc set, the
// attenuation factor [centrality.KatzOptions] documents for the Alpha <= 0
// sentinel: alpha = 0.85 / (1 + maxInDegree).
//
// "In-degree" here is the number of arcs whose DESTINATION is the node — the
// same orientation Katz accumulates over, and the orientation the library's own
// degree bound uses. For an undirected fixture the arcs are materialised in both
// directions, so in- and out-degree coincide; for a directed one they do not,
// which is exactly why the orientation has to be stated rather than assumed. An
// arc-free fixture has no degree bound at all, and the library falls back to
// 0.85 outright, so this does too.
//
// It never reads the value back from the library — the chosen alpha is not
// surfaced in the result — so this is an independent statement of the published
// contract rather than a restatement of the implementation.
func katzAutoAlphaReference(f centralityFixture) float64 {
	adj, _ := centralityAdjacency(f)
	indeg := make([]int, f.order)
	for u := range adj {
		for _, v := range adj[u] {
			indeg[v]++
		}
	}
	var maxIn int
	for _, d := range indeg {
		if d > maxIn {
			maxIn = d
		}
	}
	if maxIn == 0 {
		return katzAutoAlphaNumerator
	}
	return katzAutoAlphaNumerator / float64(1+maxIn)
}

// pprDefaultRegimeViolations cross-checks [centrality.PersonalisedPushPageRank]
// under the shipped [centrality.DefaultPPRPushOptions] against the same
// [pprReference] fixpoint the literal-option check uses, within the measured
// [pprDefaultEps].
//
// It takes the ENGINE's input (the CSR c) and the REFERENCE's input (n, edges)
// as separate arguments. Production always passes the two views of one graph;
// keeping them separate is what makes the oracle falsifiable, since a test can
// hand the engine one graph and the reference another and observe that the check
// speaks up — the only way to show this comparison is not a check that can never
// fail. It mirrors the shape [eigenvectorDefaultViolations] and
// [katzDefaultViolations] already have, where the fixture (reference input) and
// the CSR (engine input) likewise arrive as two parameters.
//
// Because [pprMeasureViolations] derives that graph from the same salted seed as
// [pprViolations], both regimes are judged on the identical graph for a given
// tick, so a divergence in one but not the other isolates the epsilon rather
// than the graph. The reference's damping is taken from the shipped options
// rather than repeated as a literal, so the two can never drift apart.
func pprDefaultRegimeViolations(tick int64, c *csr.CSR[float64], n int, edges [][2]int) []Violation {
	const src = 0

	opts := centrality.DefaultPPRPushOptions()
	got, err := centrality.PersonalisedPushPageRank(c, graph.NodeID(src), opts)
	if err != nil {
		return centralityDiverge(tick, opPPRDefaults, fmt.Sprintf(
			"PersonalisedPushPageRank under DefaultPPRPushOptions (damping %g, epsilon %g, max %d steps) returned an error on a well-formed graph: %v",
			opts.Damping, opts.Epsilon, opts.MaxSteps, err))
	}
	want := pprReference(n, edges, opts.Damping, src)

	for v := 0; v < n; v++ {
		if v >= len(got) {
			break
		}
		if math.Abs(got[v]-want[v]) > pprDefaultEps {
			return []Violation{{
				Kind: ViolationSearchDivergence, Tick: tick, Op: opPPRDefaults,
				Message: fmt.Sprintf("ppr[%d] got %.9f want %.9f (n=%d, |diff|=%.3g exceeds %g)",
					v, got[v], want[v], n, math.Abs(got[v]-want[v]), pprDefaultEps),
			}}
		}
	}
	return nil
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
