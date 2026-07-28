package exec

import "github.com/FlavioCFOliveira/GoGraph/graph"

// csrprobe.go — the shared O(log d) CSR membership probe (rmp #2142).
//
// Every forward-position lookup on the read path used to scan a source's whole
// neighbour range. Since rmp #2141 a CSR source's run is ordered by the total key
// (destination, handle), so the scan becomes a binary search: O(log d) instead of
// O(d), which is what makes Expand(Into) for a bound destination, the symmetric
// anchor swap, and the sorted-merge intersection primitive costable.
//
// The measured crossover is degree ≈16 and the win reaches 6.0x (hit) / 10.9x
// (miss) at degree 4096 — see docs/design-degree-adaptive-adjacency.md §2.2. The
// probe is NOT made adaptive on degree: a branch on a per-source threshold would
// cost more than it saves at the degrees where the two are within noise of each
// other, and it would reintroduce the per-source flag #2141 deliberately removed.
//
// A branchless (conditional-move) variant was measured and is NOT used: on a
// memory-bound search the branch predictor acts as a prefetcher, so removing the
// branch removes the speculation and lengthens the dependency chain. The plain
// branchy form was faster or equal at every degree in the cold regime.
//
// PARALLEL EDGES. Ordering is by destination first, so all slots sharing a
// destination form one CONTIGUOUS run, ordered by handle within it. Every probe
// below therefore locates the run with one binary search and then walks only that
// short run — which is also what preserves the two shipped behaviours that read a
// slot's ORDINAL within its run (cypher/api.go's buildEdgeTypeFilter and
// [matchFwdByOrdinal]).
//
// All functions here are allocation-free and take no interface.

// lowerBoundDst returns the smallest position in [start, end) whose destination
// is >= dst, or end when every destination in the range is smaller. The range
// must be destination-ordered, which every CSR built by graph/csr guarantees.
func lowerBoundDst(edges []graph.NodeID, start, end, dst uint64) uint64 {
	lo, hi := start, end
	for lo < hi {
		mid := lo + (hi-lo)/2
		if uint64(edges[mid]) < dst {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// firstDstPos returns the position of the FIRST slot in [start, end) whose
// destination is dst, and whether one exists. "First" matters: the pre-#2142
// linear scans returned the first matching slot, and several callers depend on
// that choice for parallel edges, so the lower bound reproduces it exactly.
func firstDstPos(edges []graph.NodeID, start, end, dst uint64) (uint64, bool) {
	pos := lowerBoundDst(edges, start, end, dst)
	if pos < end && uint64(edges[pos]) == dst {
		return pos, true
	}
	return 0, false
}

// dstRun returns the half-open position range of every slot in [start, end)
// whose destination is dst. The range is empty when dst is absent.
//
// The upper end is found by walking forward from the lower bound rather than by a
// second binary search: the run is the multiplicity of one parallel-edge group,
// which is 1 in a simple graph and small in practice, so a walk is cheaper than
// another O(log d) chain of dependent loads.
func dstRun(edges []graph.NodeID, start, end, dst uint64) (uint64, uint64) {
	lo := lowerBoundDst(edges, start, end, dst)
	hi := lo
	for hi < end && uint64(edges[hi]) == dst {
		hi++
	}
	return lo, hi
}
