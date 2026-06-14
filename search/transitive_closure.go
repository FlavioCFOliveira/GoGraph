package search

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// TC is a transitive-closure reachability oracle. The matrix is dense
// in memory (live * live bits, padded to whole uint64 words) but
// supports O(1) point-to-point reachability queries via [TC.Reachable].
//
// The matrix is allocated and indexed over the set of LIVE NodeIDs —
// those with at least one incident edge in the source CSR — not over
// the sparse [0, MaxNodeID()) range. On graphs built through the
// sharded Mapper the NodeID space is sparse (MaxNodeID() rounds up to
// multiples of the shard count), so sizing on MaxNodeID() would
// allocate O(MaxNodeID()^2) bits — up to the shard-count-squared times
// the necessary memory, an attacker-amplifiable over-allocation
// (rmp #1474). Compacting to the live set bounds the footprint at
// O(live^2). [TC.Reachable] translates arbitrary NodeIDs through the
// compaction map, so the public contract is unchanged for live nodes.
//
// Behaviour for non-live slots: a NodeID with no incident edge (a ghost
// padding slot, or a present but isolated node) is not in the live set,
// so [TC.Reachable] reports it unreachable from anything and unreachable
// to anything — including from itself. This matches [APSP.At]
// (Floyd-Warshall), which already excludes non-live slots, and fixes a
// prior over-report where the uncompacted matrix seeded a reflexive
// diagonal bit for every ghost padding slot in [0, MaxNodeID()).
//
// TC is safe for concurrent reads.
type TC struct {
	live    int   // compact matrix dimension (count of live NodeIDs)
	maxID   int   // CSR.MaxNodeID(); the NodeID-space bound for queries
	words   int   // (live + 63) / 64
	compact []int // length maxID; compact[id] is the index in [0, live) or -1
	bits    []uint64
}

// Reachable reports whether dst is reachable from src in the
// original graph. Reachable returns false when either NodeID lies
// outside the universe captured by the closure — out of NodeID range,
// or a non-live (ghost / isolated) slot (a defensive check at the API
// boundary, not a correctness contract for trusted callers).
func (t *TC) Reachable(src, dst graph.NodeID) bool {
	if int(src) >= t.maxID || int(dst) >= t.maxID {
		return false
	}
	si := t.compact[int(src)]
	di := t.compact[int(dst)]
	if si < 0 || di < 0 {
		return false
	}
	row := t.bits[si*t.words : si*t.words+t.words]
	d := uint64(di)
	return row[d>>6]&(1<<(d&63)) != 0
}

// TransitiveClosure builds the reachability oracle of c using
// Warshall's O(V^3 / 64) bitset variant, where V is the count of LIVE
// NodeIDs (not the sparse MaxNodeID): a square V*V bit-matrix is
// seeded with the direct adjacency (and the diagonal), and then for
// every k-pivot the algorithm OR's row k into every other row whose
// k-th bit is set. The bit-parallel inner step exploits the fact
// that the per-word AND/OR runs over 64 destinations at a time.
//
// For sparse graphs prefer BFS-per-source; this implementation is
// only competitive when V is small enough that the V*V/8 bytes of
// state fit in RAM (V=10k -> 12.5 MB; V=100k -> 1.25 GB).
//
// Concurrency: TransitiveClosure is safe to invoke concurrently on
// a shared CSR; the returned [TC] is safe for concurrent reads.
func TransitiveClosure[W any](c *csr.CSR[W]) *TC {
	defer metrics.Time("search.TransitiveClosure")()
	out, _ := TransitiveClosureCtx(context.Background(), c)
	return out
}

// TransitiveClosureCtx is the context-aware variant of
// [TransitiveClosure]. ctx.Err() is checked at every k-pivot; on
// cancellation returns (nil, wrapped ctx.Err()).
//
// The reachability matrix is sized on the live NodeID count, not on
// MaxNodeID(), so a sparse (shard-amplified) NodeID space cannot inflate
// the allocation beyond O(live^2) bits (rmp #1474). The Warshall triple
// loop runs entirely in the compact [0, live) index space; results are
// identical to the uncompacted computation for every live NodeID because
// reachability is invariant under vertex relabelling.
func TransitiveClosureCtx[W any](ctx context.Context, c *csr.CSR[W]) (*TC, error) {
	defer metrics.Time("search.TransitiveClosureCtx")()
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return &TC{}, nil
	}
	// Compact the live NodeID set into a dense [0, live) index space,
	// mirroring FloydWarshall. Ghost padding slots and isolated nodes
	// (no incident edge) map to -1 and are excluded from the matrix.
	mask := c.LiveMask()
	compact := make([]int, maxID)
	live := 0
	for i := 0; i < maxID; i++ {
		if mask[i] {
			compact[i] = live
			live++
		} else {
			compact[i] = -1
		}
	}
	if live == 0 {
		return &TC{maxID: maxID, compact: compact}, nil
	}
	words := (live + 63) / 64
	bits := make([]uint64, live*words)
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	for u := 0; u < maxID; u++ {
		ui := compact[u]
		if ui < 0 {
			continue
		}
		row := bits[ui*words : ui*words+words]
		// Self-reachable (reflexive diagonal for live nodes).
		row[ui>>6] |= 1 << (uint64(ui) & 63)
		for k := verts[u]; k < verts[u+1]; k++ {
			vi := compact[int(edges[k])]
			if vi < 0 {
				// Defensive: a live source's neighbour is live by the
				// LiveMask definition, so this branch is unreachable in
				// practice; it keeps the indexing total under any future
				// mask/edge skew.
				continue
			}
			row[vi>>6] |= 1 << (uint64(vi) & 63)
		}
	}
	for k := 0; k < live; k++ {
		if err := ctx.Err(); err != nil {
			metrics.IncCounter("search.TransitiveClosureCtx.errors", 1)
			return nil, err
		}
		kRow := bits[k*words : k*words+words]
		kBit := uint64(1) << (uint64(k) & 63)
		kWord := k >> 6
		for i := 0; i < live; i++ {
			if i == k {
				continue
			}
			iRow := bits[i*words : i*words+words]
			if iRow[kWord]&kBit == 0 {
				continue
			}
			for w := 0; w < words; w++ {
				iRow[w] |= kRow[w]
			}
		}
	}
	return &TC{live: live, maxID: maxID, words: words, compact: compact, bits: bits}, nil
}
