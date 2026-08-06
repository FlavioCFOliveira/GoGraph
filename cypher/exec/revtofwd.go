package exec

// revtofwd.go — shared reverse-CSR → forward-CSR position mapping (rmp #1692).
//
// A reverse-edge slot encodes an edge whose stored direction is
// (fwdSrc -> revUID), where fwdSrc = revEdges[revPos] and revUID is the vertex
// owning that reverse adjacency. Its forward counterpart lives in fwdSrc's
// forward adjacency at the position whose destination is revUID. The reverse
// CSR is the transpose of the forward CSR, so the mapping is a bijection per
// logical edge.
//
// This logic was first implemented inside [VarLengthExpand] for the
// per-instance multigraph relationship-typing fix (rmp #1685/#1689). It is
// extracted here as a package-level free function so [ShortestPath] and
// [AllShortestPaths] reuse the SAME handle-disambiguated mapping when they emit
// the VLE flat-list encoding, rather than forking the operator (rmp #1692). The
// behaviour is byte-identical to the original method; [VarLengthExpand.Init]
// now delegates to [buildRevToFwd].
//
// PARALLEL edges (a multigraph fwdSrc->revUID pair with several slots) are the
// reason this is not a first-match scan: every reverse slot would otherwise map
// onto the FIRST forward slot, so the relationship hydrator would report one
// merged type and the coalesced property union for every parallel reverse hop.
// Two disambiguation strategies, preferred first:
//
//   - Handle-exact. When both CSRs carry handles, match the reverse slot to the
//     forward slot whose stable handle is identical. csr.BuildReverse keeps one
//     handle per logical edge across both directions, so this recovers the exact
//     instance and is delete-stable. Mirrors [Expand.lookupFwdEdgePosByHandle].
//
//   - Positional-ordinal (fallback when either CSR lacks handles — a simple
//     graph or a legacy snapshot). The k-th reverse (fwdSrc->revUID) slot pairs
//     with the k-th forward (fwdSrc->revUID) slot. A simple graph has exactly
//     one slot per pair, so this degenerates to the original first-match scan.
//
// Entry ^uint64(0) marks "unresolved" (an out-of-range vertex or a missing
// forward counterpart); callers fall back to the synthetic reverse position in
// that rare case.
//
// # The transpose replay, and why it exists for EXACTNESS rather than for speed
//
// Both strategies above have a case they cannot resolve — an out-of-range vertex
// or a missing forward counterpart — and report it as [unresolvedFwdPos], which
// every caller answers by falling back to the synthetic reverse position. For
// hydration that is a serviceable stand-in. For a TYPE FILTER it is not: there is
// nothing to test the edge against, so the check has to stay permissive and admit
// an edge the filter may exclude. That is precisely the hazard rmp #2220 hit and
// #2236 had to remove before a typed two-sided search could be admitted.
//
// [fwdToRevByTranspose] pairs the slots with no unresolvable case at all. Every
// reverse CSR this module builds comes from [csr.CSR.BuildReverse], a
// counting-sort transpose that scatters forward slots into destination buckets in
// (u ascending, then u's own slot order); REPLAYING that scatter reproduces the
// pairing directly, and either validates completely or declines completely.
// [revTypeAdmitSet] turns it into a reverse-position bitset — an admission answer
// that is exact for every slot, or absent, never permissive.
//
// The replay is an alternative, not an optimisation, and the measurement says so.
// It is O(V+E) against the scan's O(E·d), yet BenchmarkRevToFwd puts it only
// 1.19× ahead at n=20000 (583 µs against 711 µs) while allocating 2.1× the bytes
// and 3 allocations against 1: the scan's 2M comparisons are sequential array
// reads at roughly 0.35 ns each, whereas the replay's writes scatter across
// destination buckets. Asymptotics did not decide this one. So the scan remains
// the position mapping for every caller — [VarLengthExpand],
// [AllShortestPaths] and [ShortestPath] alike, byte-identical and with its
// allocation profile untouched — and the replay is used only where its exactness
// is load-bearing.
//
// Being a replay of another function's internals, it validates rather than
// assumes. A caller may hand an operator any [CSRAdjacency] as its reverse —
// several in-tree tests pass a placeholder, and a hand-built reverse need not use
// the canonical scatter order — so every paired slot is checked as it is
// produced: the reverse slot must record the forward slot's source, and, when
// both CSRs carry handles, the same handle. One mismatch abandons the replay, and
// the typed two-sided search then declines rather than guessing.
// [TestBuildRevToFwd_TransposeMatchesScan] pins the replay against the scan over
// every fixture shape, so the two provably agree wherever both apply.

import "github.com/FlavioCFOliveira/GoGraph/graph"

// unresolvedFwdPos is the sentinel an entry of the revToFwd mapping carries when
// the reverse slot has no resolvable forward counterpart (an out-of-range vertex
// or a missing forward edge). Callers fall back to the synthetic reverse
// position in that case.
const unresolvedFwdPos = ^uint64(0)

// buildRevToFwd builds the reverse-CSR-position → forward-CSR-position mapping
// for every reverse-edge slot. The returned slice is parallel to revEdges
// (len == len(revEdges)); index it by a reverse-CSR position to obtain the
// forward-CSR position of the SAME physical edge, or [unresolvedFwdPos] when the
// slot has no resolvable forward counterpart.
//
// fwdHandles/revHandles may be nil (a simple graph or legacy snapshot); when
// either is nil the positional-ordinal fallback is used instead of the
// handle-exact pairing.
//
// This is the per-slot scan, unchanged since rmp #1692. The transpose replay the
// file comment describes is NOT wired in here: measurement put it only 1.19×
// ahead while allocating twice the bytes, so it earns its place on exactness
// alone and is used only by [revTypeAdmitSet], where exactness is what matters.
func buildRevToFwd(
	fwdVerts []uint64, fwdEdges []graph.NodeID, fwdHandles []uint64,
	revVerts []uint64, revEdges []graph.NodeID, revHandles []uint64,
) []uint64 {
	out := make([]uint64, len(revEdges))
	useHandles := fwdHandles != nil && revHandles != nil
	for revUID := uint64(0); revUID+1 < uint64(len(revVerts)); revUID++ {
		start, end := revVerts[revUID], revVerts[revUID+1]
		for revPos := start; revPos < end; revPos++ {
			fwdSrc := uint64(revEdges[revPos])
			if fwdSrc+1 >= uint64(len(fwdVerts)) {
				out[revPos] = unresolvedFwdPos
				continue
			}
			fStart, fEnd := fwdVerts[fwdSrc], fwdVerts[fwdSrc+1]
			if useHandles {
				out[revPos] = matchFwdByHandle(fwdEdges, fwdHandles, fStart, fEnd, revUID, revHandles[revPos])
				continue
			}
			// Positional-ordinal fallback: this reverse slot is the
			// ordinal-th (fwdSrc -> revUID) reverse entry; pair it with the
			// ordinal-th matching forward entry.
			ordinal := uint64(0)
			for rp := start; rp <= revPos; rp++ {
				if uint64(revEdges[rp]) == fwdSrc {
					ordinal++
				}
			}
			out[revPos] = matchFwdByOrdinal(fwdEdges, fStart, fEnd, revUID, ordinal)
		}
	}
	return out
}

// matchFwdByHandle returns the forward position in [fStart,fEnd) whose
// destination is revUID and whose stable handle equals handle, or
// [unresolvedFwdPos] when none matches.
// Since rmp #2141/#2142 the forward run is destination-ordered, so this is
// O(log d + r) — binary-search to the destination run, then walk only that run to
// match the handle — rather than O(d).
func matchFwdByHandle(fwdEdges []graph.NodeID, fwdHandles []uint64, fStart, fEnd, revUID, handle uint64) uint64 {
	lo, hi := dstRun(fwdEdges, fStart, fEnd, revUID)
	for fp := lo; fp < hi; fp++ {
		if fwdHandles[fp] == handle {
			return fp
		}
	}
	return unresolvedFwdPos
}

// fwdToRevByTranspose fills out[fwdPos] with the reverse-CSR position of the
// same physical edge, by replaying the counting-sort scatter
// [csr.CSR.BuildReverse] performs: walking the forward CSR in (u ascending, slot
// order), the k-th arc into destination v lands at reverse position
// revVerts[v]+k. It reports whether the reverse CSR really is that transpose.
//
// out must be len(fwdEdges). Cost is O(V+E) time and one []uint64 of len(revVerts)
// — no adjacency scanning, which is the whole point (see the file comment).
//
// # What is validated, and why the validation is sufficient
//
// The replay computes a candidate pairing from the FORWARD CSR alone, so it must
// prove the reverse CSR agrees rather than assume it:
//
//   - the slice shapes must be those of a transpose (equal edge counts, equal
//     vertex-offset lengths) — a placeholder reverse CSR fails here;
//   - the reverse bucket for v must have room for the arc (revPos < revVerts[v+1]),
//     so a reverse CSR with a different in-degree distribution cannot be misread;
//   - the reverse slot must record this arc's source u, which pins the pairing to
//     the right (u,v) pair;
//   - when both CSRs carry handles, the reverse slot's handle must equal the
//     forward slot's, which pins WHICH of several parallel u→v arcs it is —
//     BuildReverse copies the handle to exactly the slot this replay computes.
//
// With handles present the pairing is therefore edge-exact. Without them the
// parallel arcs of one (u,v) pair are indistinguishable in both CSRs, so any
// pairing among them is equally correct — and the replay's is the k-th-to-k-th
// pairing, which is precisely what [matchFwdByOrdinal] defines.
func fwdToRevByTranspose(
	fwdVerts []uint64, fwdEdges []graph.NodeID, fwdHandles []uint64,
	revVerts []uint64, revEdges []graph.NodeID, revHandles []uint64,
	out []uint64,
) bool {
	if len(out) != len(fwdEdges) ||
		len(revEdges) != len(fwdEdges) ||
		len(revVerts) != len(fwdVerts) ||
		len(fwdVerts) == 0 {
		return false
	}
	useHandles := fwdHandles != nil && revHandles != nil &&
		len(fwdHandles) >= len(fwdEdges) && len(revHandles) >= len(revEdges)

	cursor := make([]uint64, len(revVerts))
	for u := uint64(0); u+1 < uint64(len(fwdVerts)); u++ {
		for k := fwdVerts[u]; k < fwdVerts[u+1]; k++ {
			v := uint64(fwdEdges[k])
			if v+1 >= uint64(len(revVerts)) {
				return false
			}
			revPos := revVerts[v] + cursor[v]
			if revPos >= revVerts[v+1] {
				return false
			}
			cursor[v]++
			if uint64(revEdges[revPos]) != u {
				return false
			}
			if useHandles && revHandles[revPos] != fwdHandles[k] {
				return false
			}
			out[k] = revPos
		}
	}
	return true
}

// revTypeAdmitSet returns a bitset over REVERSE-CSR positions marking every slot
// whose edge the forward-position-keyed type filter admits, or (nil, false) when
// the transpose replay does not apply.
//
// This is the exact, O(1)-per-slot reverse type check the typed two-sided
// shortestPath needed (rmp #2236). The filter a query builds is keyed by forward
// position ([buildEdgeTypeFilter]), so a search that scans reverse slots must map
// each one before it can test it — and every mapping strategy in this file has an
// unresolvable case a type check can only answer permissively, which is how a
// typed search came to route over an excluded edge. The replay has no such case:
// it validates every slot or declines outright. The result is a bitset of
// len(revEdges)/64 words, so the per-slot test is a single word read and shift —
// no map lookup, no position resolution, and nothing to be permissive about.
//
// Bits are set by iterating the FILTER, not the CSR, so the cost beyond the
// replay is O(number of admitted edges) rather than a second full pass.
func revTypeAdmitSet(
	fwdVerts []uint64, fwdEdges []graph.NodeID, fwdHandles []uint64,
	revVerts []uint64, revEdges []graph.NodeID, revHandles []uint64,
	filter map[uint64]string,
) ([]uint64, bool) {
	fwdToRev := make([]uint64, len(fwdEdges))
	if !fwdToRevByTranspose(fwdVerts, fwdEdges, fwdHandles, revVerts, revEdges, revHandles, fwdToRev) {
		return nil, false
	}
	admit := make([]uint64, (len(revEdges)+63)/64)
	for fwdPos := range filter {
		if fwdPos >= uint64(len(fwdToRev)) {
			// A filter position outside this CSR cannot be tested against it. The
			// filter and the CSR must be the same snapshot (buildEdgeTypeFilter's
			// documented contract), so this is a caller error rather than a
			// tolerable gap: refuse the bitset instead of silently under-admitting.
			return nil, false
		}
		revPos := fwdToRev[fwdPos]
		admit[revPos/64] |= 1 << (revPos % 64)
	}
	return admit, true
}

// matchFwdByOrdinal returns the ordinal-th (1-based) forward position in
// [fStart,fEnd) whose destination is revUID, or [unresolvedFwdPos] when fewer
// than ordinal such positions exist.
// Since rmp #2141/#2142 every slot sharing a destination forms one CONTIGUOUS
// run, so the ordinal-th match is simply the ordinal-th element of that run:
// O(log d) with no scan at all. ordinal is 1-based, as the caller counts it.
func matchFwdByOrdinal(fwdEdges []graph.NodeID, fStart, fEnd, revUID, ordinal uint64) uint64 {
	if ordinal == 0 {
		return unresolvedFwdPos
	}
	lo, hi := dstRun(fwdEdges, fStart, fEnd, revUID)
	if fp := lo + ordinal - 1; fp < hi {
		return fp
	}
	return unresolvedFwdPos
}
