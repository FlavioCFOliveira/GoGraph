package exec

// revtofwd_test.go — the differential gate for the transpose replay rmp #2236
// added alongside the O(E·d) reverse→forward position scan.
//
// The replay is not a new answer, it is the SAME answer computed differently: it
// recovers the pairing csr.CSR.BuildReverse produced, by replaying that
// function's counting-sort scatter. What it adds is that it has no unresolvable
// case — it validates every slot or declines outright — which is the property a
// type check needs and the scan cannot offer. So the test is a differential
// against the scan, over every fixture shape the two-sided search is exercised
// on, plus the cases where the replay must decline.
//
// Why the agreement matters: a wrong pairing does not produce an obviously broken
// answer. It hydrates a hop to the WRONG relationship instance, reporting a real
// edge's type and properties for a different edge between the same two nodes.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// revToFwdByReplay inverts [fwdToRevByTranspose] into the reverse→forward form
// [buildRevToFwd] returns, so the two can be compared entry for entry. It lives
// in the test rather than in the package because production reads the replay only
// through [revTypeAdmitSet]; the position mapping stays with the scan, whose
// allocation profile measurement did not justify changing.
//
// The inversion needs no [unresolvedFwdPos] entries: a validated replay is a
// bijection — distinct (destination, cursor) pairs give distinct positions within
// disjoint buckets, and the edge counts are equal — so every reverse slot is
// written exactly once.
func revToFwdByReplay(fwd, rev *biCSR) ([]uint64, bool) {
	fwdToRev := make([]uint64, len(fwd.edges))
	if !fwdToRevByTranspose(
		fwd.vertices, fwd.edges, fwd.handles,
		rev.vertices, rev.edges, rev.handles, fwdToRev,
	) {
		return nil, false
	}
	out := make([]uint64, len(rev.edges))
	for k, revPos := range fwdToRev {
		out[revPos] = uint64(k)
	}
	return out, true
}

// TestBuildRevToFwd_TransposeMatchesScan requires the replay and the scan to
// agree entry for entry on a canonical transpose — the reverse CSR every
// production query has, since cypher's csrPairFromGraph always derives it via
// BuildReverse.
func TestBuildRevToFwd_TransposeMatchesScan(t *testing.T) {
	for _, gc := range biGraphCases(t) {
		t.Run(gc.name, func(t *testing.T) {
			fwd, rev := gc.g.csrPair()

			replay, ok := revToFwdByReplay(fwd, rev)
			if !ok {
				t.Fatalf("the replay declined a canonical transpose (V=%d E=%d) — that is the "+
					"reverse CSR every production query has, so declining one means no typed "+
					"two-sided search can ever run", gc.g.n, len(gc.g.edges))
			}
			scan := buildRevToFwd(
				fwd.vertices, fwd.edges, fwd.handles,
				rev.vertices, rev.edges, rev.handles,
			)
			if len(replay) != len(scan) {
				t.Fatalf("replay has %d entries, scan has %d", len(replay), len(scan))
			}
			for revPos := range scan {
				if replay[revPos] != scan[revPos] {
					t.Errorf("reverse slot %d: replay maps to forward %d, scan maps to %d",
						revPos, replay[revPos], scan[revPos])
				}
			}
			// An independent absolute check, so agreement between the two is not the
			// only evidence: every mapping must name a forward slot that really is the
			// same physical edge, identified by its handle.
			for revPos := range rev.edges {
				fwdPos := replay[revPos]
				if fwdPos >= uint64(len(fwd.edges)) {
					t.Fatalf("reverse slot %d maps to forward position %d, out of range", revPos, fwdPos)
				}
				if rev.handles[revPos] != fwd.handles[fwdPos] {
					t.Errorf("reverse slot %d (handle %d) maps to forward slot %d (handle %d) — "+
						"a different physical edge", revPos, rev.handles[revPos], fwdPos, fwd.handles[fwdPos])
				}
			}
		})
	}
}

// TestBuildRevToFwd_DeclinesWhatIsNotATranspose pins the validation. Each case is
// a reverse CSR that is not the canonical transpose of its forward CSR, and the
// replay must decline rather than return a plausible-looking wrong pairing —
// buildRevToFwd then falls back to the scan, which is why declining is safe.
func TestBuildRevToFwd_DeclinesWhatIsNotATranspose(t *testing.T) {
	// Two arcs into node 2, from sources 1 and 0 in that edge-list order. The
	// canonical transpose fills 2's reverse bucket source-ascending, [0,1]; the
	// edge-list order gives [1,0].
	g := biTestGraph{3, [][2]int{{1, 2}, {0, 2}}}
	fwd, canonicalRev := g.csrPair()
	_, listOrderRev := g.csrPairNonCanonical()

	cases := []struct {
		name string
		rev  *biCSR
		want bool
	}{
		{"canonical transpose", canonicalRev, true},
		{"buckets in edge-list order", listOrderRev, false},
		{"empty placeholder", buildCSRWithHandles(3, nil), false},
		{"wrong arc contents", &biCSR{
			vertices: canonicalRev.vertices,
			edges:    []graph.NodeID{0, 0}, // node 1's arc rewritten as node 0's
			handles:  canonicalRev.handles,
		}, false},
		{"handles swapped between two arcs of one bucket", &biCSR{
			vertices: canonicalRev.vertices,
			edges:    canonicalRev.edges,
			handles:  []uint64{canonicalRev.handles[1], canonicalRev.handles[0]},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := revToFwdByReplay(fwd, tc.rev)
			if ok != tc.want {
				t.Fatalf("replay accepted = %v, want %v", ok, tc.want)
			}
			// Whatever the replay decides, buildRevToFwd must still answer: it falls
			// back to the scan. A nil result would break every reverse hop.
			got := buildRevToFwd(
				fwd.vertices, fwd.edges, fwd.handles,
				tc.rev.vertices, tc.rev.edges, tc.rev.handles,
			)
			if len(got) != len(tc.rev.edges) {
				t.Fatalf("buildRevToFwd returned %d entries for %d reverse slots",
					len(got), len(tc.rev.edges))
			}
		})
	}
}

// TestRevTypeAdmitSet_MarksExactlyTheAdmittedSlots is the absolute check on the
// bitset the typed two-sided search rests on. It is built from forward positions
// and read at reverse positions, so the property to verify is that a reverse slot
// is marked if and only if the SAME physical edge — identified by handle — is
// admitted in the forward filter.
func TestRevTypeAdmitSet_MarksExactlyTheAdmittedSlots(t *testing.T) {
	for _, gc := range biGraphCases(t) {
		for _, fc := range biFilterCases {
			if fc.admits == nil {
				continue // no filter, no bitset
			}
			t.Run(gc.name+"/"+fc.name, func(t *testing.T) {
				fwd, rev := gc.g.csrPair()
				filter := filterFor(fwd, fc)

				admit, ok := revTypeAdmitSet(
					fwd.vertices, fwd.edges, fwd.handles,
					rev.vertices, rev.edges, rev.handles, filter,
				)
				if !ok {
					t.Fatal("the bitset was refused for a canonical transpose, so no typed " +
						"two-sided search can run on this shape")
				}
				for revPos := range rev.edges {
					// The oracle: this slot's edge, by handle, and whether the FIXTURE
					// admits it — read from the filter case, not from the filter map the
					// bitset was built from.
					wantAdmitted := fc.admits(int(rev.handles[revPos]))
					if got := bitsetContains(admit, uint64(revPos)); got != wantAdmitted {
						t.Errorf("reverse slot %d (edge %d): bitset says admitted=%v, filter case says %v",
							revPos, rev.handles[revPos], got, wantAdmitted)
					}
				}
			})
		}
	}
}

// benchRevToFwdGraph builds the shape bench/r4audit/shortestpath_test.go measures
// on — n nodes of average out-degree 10, wired deterministically — as a raw CSR
// pair, so the two mapping algorithms can be timed without a query around them.
func benchRevToFwdGraph(n, degree int) (fwd, rev *biCSR) {
	edges := make([][2]int, 0, n*degree)
	for i := 0; i < n; i++ {
		for j := 1; j <= degree; j++ {
			edges = append(edges, [2]int{i, (i*7 + j*13) % n})
		}
	}
	g := biTestGraph{n: n, edges: edges}
	return g.csrPair()
}

// BenchmarkRevToFwd is why the transpose replay is NOT wired into buildRevToFwd.
//
// The end-to-end figures in
// docs/benchmarks/shortest-path-bidir-widened-2026-07-28.md fold together two
// independent effects — hoisting the build out of the per-outer-row Init path, and
// replacing an O(E·d) build with an O(V+E) one — and a combined measurement cannot
// attribute the win to either. Measured apart, the replay is worth almost nothing:
// 1.19× at n=20000, for 2.1× the bytes and 3 allocations against 1. The scan's
// comparisons are sequential array reads; the replay's writes scatter across
// destination buckets, and on this hardware that all but cancels a factor-of-d
// advantage in operation count.
//
// So the hoist is the entire win, the replay is kept only for the exactness
// revTypeAdmitSet needs, and the position mapping stays with the scan rather than
// taking an allocation regression for a 19% time gain nothing measured needs.
// Re-run this before changing that decision.
//
//	go test -run '^$' -bench BenchmarkRevToFwd -benchmem ./cypher/exec/
func BenchmarkRevToFwd(b *testing.B) {
	for _, n := range []int{5000, 20000} {
		fwd, rev := benchRevToFwdGraph(n, 10)

		b.Run(fmt.Sprintf("n%d/transpose", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				out, ok := revToFwdByReplay(fwd, rev)
				if !ok || len(out) == 0 {
					b.Fatal("the replay declined the canonical transpose it is meant to serve")
				}
			}
		})
		b.Run(fmt.Sprintf("n%d/scan", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if out := buildRevToFwd(
					fwd.vertices, fwd.edges, fwd.handles,
					rev.vertices, rev.edges, rev.handles,
				); len(out) == 0 {
					b.Fatal("empty mapping")
				}
			}
		})
	}
}
