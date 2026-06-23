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
func matchFwdByHandle(fwdEdges []graph.NodeID, fwdHandles []uint64, fStart, fEnd, revUID, handle uint64) uint64 {
	for fp := fStart; fp < fEnd; fp++ {
		if uint64(fwdEdges[fp]) == revUID && fwdHandles[fp] == handle {
			return fp
		}
	}
	return unresolvedFwdPos
}

// matchFwdByOrdinal returns the ordinal-th (1-based) forward position in
// [fStart,fEnd) whose destination is revUID, or [unresolvedFwdPos] when fewer
// than ordinal such positions exist.
func matchFwdByOrdinal(fwdEdges []graph.NodeID, fStart, fEnd, revUID, ordinal uint64) uint64 {
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
