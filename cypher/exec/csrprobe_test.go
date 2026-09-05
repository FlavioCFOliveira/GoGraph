package exec

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// csrprobe_test.go — differential for the O(log d) CSR probes (rmp #2142).
//
// The acceptance criterion is not "the binary search finds the destination" but
// "the by-handle probe returns the SAME slot as before for every input", because
// a forward position is an edge identity: it feeds relationship hydration,
// per-instance typing and cyphermorphism. So the pre-#2142 linear scans are
// reproduced here verbatim as the ORACLE and the new probes are compared against
// them over randomised ordered CSRs that are deliberately dense in parallel edges.

// --- oracles: the exact pre-#2142 bodies ---

func refFirstDstPos(edges []graph.NodeID, start, end, dst uint64) (uint64, bool) {
	for pos := start; pos < end; pos++ {
		if uint64(edges[pos]) == dst {
			return pos, true
		}
	}
	return 0, false
}

func refMatchFwdByHandle(fwdEdges []graph.NodeID, fwdHandles []uint64, fStart, fEnd, revUID, handle uint64) uint64 {
	for fp := fStart; fp < fEnd; fp++ {
		if uint64(fwdEdges[fp]) == revUID && fwdHandles[fp] == handle {
			return fp
		}
	}
	return unresolvedFwdPos
}

func refMatchFwdByOrdinal(fwdEdges []graph.NodeID, fStart, fEnd, revUID, ordinal uint64) uint64 {
	seen := uint64(0)
	for fp := fStart; fp < fEnd; fp++ {
		if uint64(fwdEdges[fp]) == revUID {
			seen++
			if seen == ordinal {
				return fp
			}
		}
	}
	return unresolvedFwdPos
}

// probeFixture builds an ORDERED CSR (via the production csr.OrderRuns, so the
// fixture cannot drift from the invariant the probes rely on) whose destination
// space is small relative to the degree, which forces parallel edges.
func probeFixture(r *rand.Rand, nSrc, maxDeg, dstSpace int) (verts []uint64, edges []graph.NodeID, handles []uint64) {
	verts = make([]uint64, nSrc+1)
	var total uint64
	degs := make([]int, nSrc)
	for i := range degs {
		degs[i] = r.IntN(maxDeg + 1)
		verts[i] = total
		total += uint64(degs[i])
	}
	verts[nSrc] = total
	edges = make([]graph.NodeID, total)
	handles = make([]uint64, total)
	for i := range edges {
		edges[i] = graph.NodeID(r.IntN(dstSpace))
		// Handle 0 appears on purpose: it is the "no handle" sentinel a
		// MERGE-created slot carries, and it is the case the total key cannot
		// separate, so the probes must still agree with the linear oracle there.
		handles[i] = uint64(r.IntN(4))
	}
	csr.OrderRuns[struct{}](verts, edges, nil, handles)
	return verts, edges, handles
}

func TestCSRProbe_DifferentialAgainstLinearScan(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(20, 26)) //nolint:gosec // G404: math/rand/v2 PCG seeded from a fixed constant — this test asserts a reproducible sequence, which a CSPRNG would destroy.
	for trial := 0; trial < 200; trial++ {
		// Span both sides of the insertion-sort cutoff and the probe crossover.
		nSrc, maxDeg, dstSpace := 1+r.IntN(5), 1+r.IntN(70), 1+r.IntN(10)
		verts, edges, handles := probeFixture(r, nSrc, maxDeg, dstSpace)

		for s := 0; s < nSrc; s++ {
			start, end := verts[s], verts[s+1]
			// Probe every destination in the space, present or not, plus one
			// beyond it so the "greater than every element" path is covered.
			for dst := uint64(0); dst <= uint64(dstSpace); dst++ {
				wantPos, wantOK := refFirstDstPos(edges, start, end, dst)
				gotPos, gotOK := firstDstPos(edges, start, end, dst)
				if gotOK != wantOK || (wantOK && gotPos != wantPos) {
					t.Fatalf("trial %d src %d dst %d: firstDstPos = (%d,%v), want (%d,%v)",
						trial, s, dst, gotPos, gotOK, wantPos, wantOK)
				}

				// dstRun must cover EXACTLY the slots the oracle would match.
				lo, hi := dstRun(edges, start, end, dst)
				var wantCount uint64
				for pos := start; pos < end; pos++ {
					if uint64(edges[pos]) == dst {
						wantCount++
					}
				}
				if hi-lo != wantCount {
					t.Fatalf("trial %d src %d dst %d: dstRun covers %d slots, want %d",
						trial, s, dst, hi-lo, wantCount)
				}
				for pos := lo; pos < hi; pos++ {
					if uint64(edges[pos]) != dst {
						t.Fatalf("trial %d src %d dst %d: dstRun slot %d holds %d",
							trial, s, dst, pos, edges[pos])
					}
				}

				// By-handle: every handle value, including absent ones.
				for h := uint64(0); h < 5; h++ {
					want := refMatchFwdByHandle(edges, handles, start, end, dst, h)
					got := matchFwdByHandle(edges, handles, start, end, dst, h)
					if got != want {
						t.Fatalf("trial %d src %d dst %d handle %d: matchFwdByHandle = %d, want %d",
							trial, s, dst, h, got, want)
					}
				}

				// By-ordinal: 0 (invalid), every valid ordinal, and one past the end.
				for ord := uint64(0); ord <= wantCount+1; ord++ {
					want := refMatchFwdByOrdinal(edges, start, end, dst, ord)
					got := matchFwdByOrdinal(edges, start, end, dst, ord)
					if got != want {
						t.Fatalf("trial %d src %d dst %d ordinal %d: matchFwdByOrdinal = %d, want %d",
							trial, s, dst, ord, got, want)
					}
				}
			}
		}
	}
}

// TestCSRProbe_ZeroAllocation pins the allocation budget: these probes sit on the
// per-row read path, so any allocation would show up per emitted row.
func TestCSRProbe_ZeroAllocation(t *testing.T) {
	// No t.Parallel: testing.AllocsPerRun panics in a parallel test.
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: math/rand/v2 PCG seeded from a fixed constant — this test asserts a reproducible sequence, which a CSPRNG would destroy.
	verts, edges, handles := probeFixture(r, 4, 64, 8)
	start, end := verts[0], verts[1]

	if allocs := testing.AllocsPerRun(20, func() {
		for dst := uint64(0); dst < 8; dst++ {
			firstDstPos(edges, start, end, dst)
			dstRun(edges, start, end, dst)
			matchFwdByHandle(edges, handles, start, end, dst, 2)
			matchFwdByOrdinal(edges, start, end, dst, 1)
		}
	}); allocs != 0 {
		t.Errorf("CSR probes allocated %.1f times per run; want 0", allocs)
	}
}

// TestCSRProbe_EmptyAndSingletonRanges covers the degenerate ranges the binary
// search must handle without indexing out of bounds: an empty run (a source with
// no out-edges) and a single-slot run.
func TestCSRProbe_EmptyAndSingletonRanges(t *testing.T) {
	t.Parallel()
	edges := []graph.NodeID{7}
	handles := []uint64{9}

	// Empty range [0,0).
	if _, ok := firstDstPos(edges, 0, 0, 7); ok {
		t.Error("firstDstPos found a destination in an empty range")
	}
	if lo, hi := dstRun(edges, 0, 0, 7); lo != 0 || hi != 0 {
		t.Errorf("dstRun on an empty range = (%d,%d), want (0,0)", lo, hi)
	}
	if got := matchFwdByHandle(edges, handles, 0, 0, 7, 9); got != unresolvedFwdPos {
		t.Errorf("matchFwdByHandle on an empty range = %d, want unresolved", got)
	}
	if got := matchFwdByOrdinal(edges, 0, 0, 7, 1); got != unresolvedFwdPos {
		t.Errorf("matchFwdByOrdinal on an empty range = %d, want unresolved", got)
	}

	// Singleton range [0,1): hit, and misses on both sides of the element.
	if pos, ok := firstDstPos(edges, 0, 1, 7); !ok || pos != 0 {
		t.Errorf("firstDstPos singleton hit = (%d,%v), want (0,true)", pos, ok)
	}
	for _, miss := range []uint64{6, 8} {
		if _, ok := firstDstPos(edges, 0, 1, miss); ok {
			t.Errorf("firstDstPos singleton found absent destination %d", miss)
		}
	}
	if got := matchFwdByOrdinal(edges, 0, 1, 7, 2); got != unresolvedFwdPos {
		t.Errorf("matchFwdByOrdinal past the run end = %d, want unresolved", got)
	}
}
