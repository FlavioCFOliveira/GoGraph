package search

import (
	"context"

	"gograph/graph"
	"gograph/graph/csr"
)

// TC is a transitive-closure reachability oracle: a bitset matrix
// over the live NodeID space of the source CSR. The matrix is dense
// in memory (V * V bits, padded to whole uint64 words) but supports
// O(1) point-to-point reachability queries via [TC.Reachable].
//
// TC is safe for concurrent reads.
type TC struct {
	n     int
	words int // (n + 63) / 64
	bits  []uint64
}

// Reachable reports whether dst is reachable from src in the
// original graph. Reachable returns false when either NodeID lies
// outside the universe captured by the closure (a defensive check
// at the API boundary, not a correctness contract for trusted
// callers).
func (t *TC) Reachable(src, dst graph.NodeID) bool {
	if int(src) >= t.n || int(dst) >= t.n {
		return false
	}
	row := t.bits[int(src)*t.words : int(src)*t.words+t.words]
	d := uint64(dst)
	return row[d>>6]&(1<<(d&63)) != 0
}

// TransitiveClosure builds the reachability oracle of c using
// Warshall's O(V^3 / 64) bitset variant: a square V*V bit-matrix is
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
	out, _ := TransitiveClosureCtx(context.Background(), c)
	return out
}

// TransitiveClosureCtx is the context-aware variant of
// [TransitiveClosure]. ctx.Err() is checked at every k-pivot; on
// cancellation returns (nil, wrapped ctx.Err()).
func TransitiveClosureCtx[W any](ctx context.Context, c *csr.CSR[W]) (*TC, error) {
	n := int(c.MaxNodeID())
	if n == 0 {
		return &TC{}, nil
	}
	words := (n + 63) / 64
	bits := make([]uint64, n*words)
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	for u := 0; u < n; u++ {
		row := bits[u*words : u*words+words]
		// Self-reachable.
		row[u>>6] |= 1 << (uint64(u) & 63)
		for k := verts[u]; k < verts[u+1]; k++ {
			v := uint64(edges[k])
			row[v>>6] |= 1 << (v & 63)
		}
	}
	for k := 0; k < n; k++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kRow := bits[k*words : k*words+words]
		kBit := uint64(1) << (uint64(k) & 63)
		kWord := k >> 6
		for i := 0; i < n; i++ {
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
	return &TC{n: n, words: words, bits: bits}, nil
}
