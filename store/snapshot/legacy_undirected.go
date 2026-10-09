package snapshot

import "github.com/FlavioCFOliveira/GoGraph/graph"

// Loading a legacy undirected snapshot (rmp #3072).
//
// A snapshot written by a graph configured undirected ("directed": false in
// [GraphConfig]) stores every non-loop edge as TWO arcs — one per direction,
// sharing one stable handle and one weight — and a self-loop as one. Its
// edge-keyed records (labels.bin, properties.bin, edgehandles.bin) are keyed by
// the direction each write named. [LoadSnapshotFull] returns such a readback
// with both directions intact and reports the shape in [LoadedSnapshot.Legacy]:
// recovery replays the write-ahead log above it with the old semantics on that
// two-sided image and only then folds each edge to one relationship.
//
// The one change the loader makes is to a readback without a handle column,
// written before every slot carried a handle: [assignLegacyMirrorHandles]
// gives the two arcs of each edge one shared handle, so recovery can tell
// which arcs are mirrors of each other.

// assignLegacyMirrorHandles fills the missing handle column of rb, read from a
// legacy undirected snapshot, so the two arcs of every edge share a handle: the
// k-th arc from a to b and the k-th arc from b to a are mirrors (the undirected
// engine appended both together), and a self-loop's single arc has its own.
// Handles are numbered from 1 in CSR order. It does nothing when rb carries a
// handle column or its offsets are not mutually consistent, which
// [ApplyCSRToGraph] then reports.
//
// The pairing is a reconstruction, not a record. A WAL tail written above such
// a snapshot by a handle-bearing build names the handles that build minted when
// it reloaded the snapshot, and the pairing here need not reproduce them. That
// build gave each of the two arcs of an edge its own fresh handle on reload,
// doubling every edge (11b2ddd1:store/snapshot/apply.go:422), so such a store
// was already corrupt before this migration; no store written by v0.15.0
// reaches this case, because its snapshots carry a handle column.
func assignLegacyMirrorHandles(rb *CSRReadback) {
	if rb.Handles != nil || len(rb.Edges) == 0 || len(rb.Vertices) == 0 {
		return
	}
	n := len(rb.Vertices) - 1
	for i := 0; i < n; i++ {
		if rb.Vertices[i] > rb.Vertices[i+1] {
			return
		}
	}
	if rb.Vertices[n] > uint64(len(rb.Edges)) {
		return
	}
	handles := make([]uint64, len(rb.Edges))
	waiting := make(map[[2]graph.NodeID][]uint64) // arcs (src, dst) whose mirror is not yet seen
	next := uint64(1)
	for src := 0; src < n; src++ {
		s := graph.NodeID(src)
		for k := rb.Vertices[src]; k < rb.Vertices[src+1]; k++ {
			d := rb.Edges[k]
			if q := waiting[[2]graph.NodeID{d, s}]; s != d && len(q) > 0 {
				handles[k] = q[0]
				waiting[[2]graph.NodeID{d, s}] = q[1:]
				continue
			}
			handles[k] = next
			if s != d {
				waiting[[2]graph.NodeID{s, d}] = append(waiting[[2]graph.NodeID{s, d}], next)
			}
			next++
		}
	}
	rb.Handles = handles
}
