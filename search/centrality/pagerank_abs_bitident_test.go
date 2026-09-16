package centrality

import (
	"math"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// absBySignBranch is the form runRange carried before rmp #2869: take the
// difference, then negate it when it is negative. It is kept here as the
// reference the intrinsic is measured against, so the claim "math.Abs is
// bit-identical to the branch" is asserted rather than asserted-by-comment.
func absBySignBranch(d float64) float64 {
	if d < 0 {
		d = -d
	}
	return d
}

// TestPageRankAbsForm_BitIdentical measures the substitution against the
// branch it replaced, over the domain runRange can actually produce, and
// records the two values where the two forms are NOT the same.
//
// What was measured, not assumed:
//
//   - For every finite float64 other than negative zero the two forms
//     produce the same IEEE-754 bit pattern. math.Abs clears the sign
//     bit; the branch negates, and for a finite non-zero value those
//     agree bit for bit.
//   - For -0.0 they differ: math.Abs yields +0.0, the branch leaves
//     -0.0. The ACCUMULATION is still identical, because runRange's
//     running total starts at +0.0 and +0.0 + -0.0 is +0.0.
//   - For a NaN they differ in the SIGN bit: a comparison against NaN is
//     false, so the branch is a no-op and keeps a negative NaN negative,
//     while math.Abs clears its sign. This one does reach the
//     accumulation on arm64, which propagates the NaN operand's sign.
//
// Neither divergent value can arise in runRange. The !isLive arm takes
// the magnitude of cur[v], which is exactly +0.0 for every node
// PageRankCtx treats as not live; the live arm takes sum - cur[v], where
// sum is a finite positive (baseShare plus non-negative in-edge terms)
// and cur[v] a finite non-negative, so the difference is finite and
// IEEE-754 subtraction of equal operands yields +0.0, never -0.0. A NaN
// would additionally have to survive validatePageRankOptions, which
// rejects a non-finite damping or tolerance. The end-to-end consequence
// is pinned separately by TestPageRankScores_BitPin_PullPath.
func TestPageRankAbsForm_BitIdentical(t *testing.T) {
	t.Parallel()

	sameBits := func(t *testing.T, x float64) {
		t.Helper()
		gotBits := math.Float64bits(math.Abs(x))
		wantBits := math.Float64bits(absBySignBranch(x))
		if gotBits != wantBits {
			t.Fatalf("abs(%#016x): math.Abs = %#016x, sign branch = %#016x",
				math.Float64bits(x), gotBits, wantBits)
		}
		// The accumulation, not just the magnitude: runRange adds the
		// value into a running localDelta that starts at +0.
		var viaAbs, viaBranch float64
		viaAbs += math.Abs(x)
		viaBranch += absBySignBranch(x)
		if math.Float64bits(viaAbs) != math.Float64bits(viaBranch) {
			t.Fatalf("accumulating abs(%#016x): math.Abs = %#016x, sign branch = %#016x",
				math.Float64bits(x), math.Float64bits(viaAbs), math.Float64bits(viaBranch))
		}
	}

	t.Run("finite-domain", func(t *testing.T) {
		t.Parallel()
		for _, x := range []float64{
			0, 1, -1, 0.5, -0.5,
			math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64,
			math.MaxFloat64, -math.MaxFloat64,
			math.Inf(1), math.Inf(-1),
		} {
			sameBits(t, x)
		}
		//nolint:gosec // G404: math/rand/v2 PCG with a fixed seed; this test asserts a reproducible corpus, which a CSPRNG would destroy.
		r := rand.New(rand.NewPCG(0x2869, 0x5EED))
		checked := 0
		for checked < 200000 {
			x := math.Float64frombits(r.Uint64())
			if math.IsNaN(x) || (x == 0 && math.Signbit(x)) {
				continue // the two carve-outs, covered by their own subtests
			}
			sameBits(t, x)
			checked++
		}
		// Realistic PageRank deltas: small magnitudes of either sign around
		// the tolerance, where a lost bit would change the iteration count.
		for i := 0; i < 20000; i++ {
			x := (r.Float64() - 0.5) * math.Pow(10, float64(r.IntN(24)-18))
			if x == 0 {
				continue
			}
			sameBits(t, x)
		}
	})

	t.Run("negative-zero-differs-but-accumulates-alike", func(t *testing.T) {
		t.Parallel()
		negZero := math.Copysign(0, -1)
		if math.Float64bits(math.Abs(negZero)) == math.Float64bits(absBySignBranch(negZero)) {
			t.Fatal("expected math.Abs(-0.0) and the sign branch to differ in the sign bit; the carve-out this test records no longer holds")
		}
		// runRange's accumulator starts at +0.0, which is what makes the
		// divergence unobservable.
		var viaAbs, viaBranch float64
		viaAbs += math.Abs(negZero)
		viaBranch += absBySignBranch(negZero)
		if math.Float64bits(viaAbs) != math.Float64bits(viaBranch) {
			t.Fatalf("accumulating abs(-0.0): math.Abs = %#016x, sign branch = %#016x",
				math.Float64bits(viaAbs), math.Float64bits(viaBranch))
		}
	})

	t.Run("nan-differs-and-is-unreachable", func(t *testing.T) {
		t.Parallel()
		negNaN := math.Float64frombits(0xFFF8000000000000)
		if math.Float64bits(math.Abs(negNaN)) == math.Float64bits(absBySignBranch(negNaN)) {
			t.Fatal("expected math.Abs and the sign branch to differ on a negative NaN; the carve-out this test records no longer holds")
		}
		// Both remain NaN, so the convergence test delta < Tolerance is
		// false either way and the iteration count cannot diverge.
		if !math.IsNaN(math.Abs(negNaN)) || !math.IsNaN(absBySignBranch(negNaN)) {
			t.Fatal("both forms must keep a NaN a NaN")
		}
		// The reachability claim: a non-finite option is rejected at the
		// boundary, so no NaN can enter the iteration.
		if _, _, err := PageRank(pageRankPinGraph(t), PageRankOptions{
			Damping:       math.NaN(),
			MaxIterations: 4,
			Tolerance:     1e-9,
		}); err == nil {
			t.Fatal("PageRank accepted a NaN damping: a NaN could reach runRange's delta")
		}
	})
}

// pageRankPinGraph builds a deterministic, dense-id directed graph large
// enough to take the parallel pull path: a circulant core plus dangling
// sinks plus isolated slots, so the pinned run exercises every arm of
// runRange — the live arm with its in-edge sum, the dangling contribution
// to baseShare, and the !isLive arm whose delta is |0 - cur[v]|.
func pageRankPinGraph(t testing.TB) *csr.CSR[struct{}] {
	t.Helper()
	const (
		core     = 3000 // 0..2989 have out-edges; 2990..2999 are sinks
		sinkFrom = 2990
		span     = 3200 // 3000..3199 are isolated slots
	)
	vertices := make([]uint64, span+1)
	var edges []graph.NodeID
	for v := 0; v < span; v++ {
		vertices[v] = uint64(len(edges))
		if v >= sinkFrom || v >= core {
			continue // dangling sink, or an isolated slot
		}
		// Three strides, so in-degree varies and the reverse-CSR runs are
		// non-trivial.
		edges = append(edges,
			graph.NodeID((v+1)%core),
			graph.NodeID((v+7)%core),
			graph.NodeID((v*31+13)%core),
		)
	}
	vertices[span] = uint64(len(edges))
	return csr.FromArrays[struct{}](vertices, edges, nil, uint64(core), uint64(len(edges)))
}

// pageRankBitsDigest is an order-sensitive FNV-1a over the exact IEEE-754
// bit patterns of every score. A digest rather than a table of 3200
// values: it pins every bit of every score, and any single-bit change in
// any score changes it.
func pageRankBitsDigest(ranks []float64) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, r := range ranks {
		b := math.Float64bits(r)
		for s := 0; s < 64; s += 8 {
			h ^= (b >> s) & 0xFF
			h *= prime
		}
	}
	return h
}

// pageRankPinDigest is the digest of the pinned run's scores, captured at
// commit 8affe124 — the arm that used the sign branch — and asserted here
// against the arm that uses math.Abs.
//
// It is worker-count independent by construction: every next[v] sums its
// in-edges in the fixed reverse-CSR order whatever the partition, and the
// options below run a FIXED iteration count (the tolerance is far below
// any delta the run can reach), so the only worker-dependent quantity in
// PageRankCtx — the L1 delta reduction that decides early convergence —
// cannot influence the result. Verified at GOMAXPROCS 2, 3, 4, 7 and 10.
const (
	pageRankPinDigest     = uint64(0x65217e41338db49b)
	pageRankPinIterations = 12
)

// TestPageRankScores_BitPin_PullPath is the end-to-end pin for rmp #2869:
// the parallel pull path's scores are bit-for-bit what they were before
// math.Abs replaced the sign branch.
//
// It is also the guard on the change this task deliberately did NOT make.
// Precomputing damping/float64(outdeg[u]) per node is the larger prize on
// the same line and is NOT bit-identical; this digest fails on it, so the
// decision to make that change stays a decision rather than a silent
// drift.
func TestPageRankScores_BitPin_PullPath(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("the parallel pull path needs GOMAXPROCS > 1")
	}
	c := pageRankPinGraph(t)

	// Precondition: the graph must actually be over the threshold, or the
	// digest would pin the serial push path instead of runRange.
	live := pageRankLiveCount(c)
	if live < pageRankParallelThreshold {
		t.Fatalf("pin graph has %d live nodes, below the parallel threshold %d: runRange would not run",
			live, pageRankParallelThreshold)
	}

	ranks, iters, err := PageRank(c, PageRankOptions{
		Damping:       0.85,
		MaxIterations: pageRankPinIterations,
		// Far below any delta 12 iterations can reach, so the run always
		// uses its full iteration budget and the result is independent of
		// the worker-count-dependent delta reduction.
		Tolerance: 1e-300,
	})
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}
	if iters != pageRankPinIterations {
		t.Fatalf("iterations = %d, want %d: the run converged early and the digest is not comparable",
			iters, pageRankPinIterations)
	}
	if got := pageRankBitsDigest(ranks); got != pageRankPinDigest {
		t.Fatalf("PageRank score digest = %#016x, want %#016x: the scores are not bit-identical to the pre-#2869 arm",
			got, pageRankPinDigest)
	}
}

// pageRankLiveCount counts the nodes PageRankCtx treats as live: those
// with at least one incident edge in either direction.
func pageRankLiveCount[W any](c *csr.CSR[W]) int {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := len(verts) - 1
	isLive := make([]bool, n)
	for i := 0; i < n; i++ {
		if verts[i+1] > verts[i] {
			isLive[i] = true
			for k := verts[i]; k < verts[i+1]; k++ {
				isLive[int(edges[k])] = true
			}
		}
	}
	live := 0
	for _, l := range isLive {
		if l {
			live++
		}
	}
	return live
}
