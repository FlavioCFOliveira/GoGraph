package adjlist

import (
	"slices"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// revIndex is the live in-edge index: for every node it records which nodes
// hold an edge INTO it, one entry per edge slot, so parallel edges are
// represented with their multiplicity exactly as the forward adjacency is.
//
// # Why it exists
//
// The forward adjacency answers "what does src point at" in O(1) and "what
// points at dst" not at all. Before rmp #2400 the only answer to the second
// question was [graph.Mapper.Walk] — a scan of every interned node, loading
// every adjacency entry, per question asked. The Cypher delete path asks it
// once per node deleted (the DELETE guard "does this node still have
// relationships", and DETACH DELETE's removal of the incoming edges), so
// deleting k nodes from a graph of n cost O(k·n) and the measured wipe time
// grew linearly with every node the graph had ever interned. That is the
// defect the 2026-08-11 concurrency assessment recorded as F1, and this is its
// real mechanism: in a CPU profile of the reproduction, Mapper.Walk is 78.77%
// of samples, while the tombstone-bitmap clone the assessment named as the
// root cause is 0.99% and the whole of removeNodeInfo is 1.72%.
//
// Neo4j and Memgraph both store the incoming direction beside the outgoing one
// for the same reason. This index is the same decision, kept deliberately
// smaller: it carries NodeIDs only, with no weight, handle, label or property
// column, because the questions it exists to answer never need them.
//
// # Concurrency
//
// Sharded on the destination's own shard bits, exactly like the forward
// adjacency, so two writes to different destinations never contend. It is a
// LEAF in the lock order: a caller may hold any number of adjacency shard
// locks while calling into this index, and this index never acquires an
// adjacency lock, so the order adjacency→reverse can never invert.
//
// # It is a derived index, and it is maintained where membership changes
//
// Every edge-slot insertion funnels through [AdjList.upsertEdgeLocked] and
// every edge-slot removal through one of the four removal paths, all of which
// know (src, dst) at the point they publish. Nothing else changes membership:
// the label, property and aux mutators republish an entry carrying
// `current.neighbours` unchanged. Rollback needs no special handling because
// the undo log replays the ordinary add and remove operations rather than
// restoring entry snapshots.
// The gate that lets a graph with no edges pay nothing for this index is
// [AdjList.Size], the edge counter the forward path already maintains — NOT a
// counter of its own. An earlier draft kept its own atomic.Int64 here and
// incremented it on every insertion, which put a second globally shared cache
// line on the write path for a number the AdjList was already tracking. That is
// precisely the shape graph/mvcc/horizon.go exists to avoid, and measuring it
// against a counter that had to exist anyway is not a trade worth making.
type revIndex struct {
	shards [shardCount]revShard
}

// revShard holds the in-neighbour lists of every node whose NodeID selects this
// shard, indexed by the intra-shard component of the DESTINATION's NodeID.
type revShard struct {
	mu sync.RWMutex
	// srcs[intra] holds one entry per edge slot pointing INTO the node whose
	// intra-shard index is intra. Nil until this shard records its first
	// in-edge, so a shard no edge ever reaches costs one mutex and one nil
	// slice header.
	srcs [][]graph.NodeID
}

// add records one edge slot from src into dst.
func (r *revIndex) add(dst, src graph.NodeID) {
	sh := &r.shards[dst&shardMask]
	intra := uint64(dst) >> shardBits
	sh.mu.Lock()
	if intra >= uint64(len(sh.srcs)) {
		// GEOMETRIC, and the first version of this was not — it grew to exactly
		// intra+1, which reallocates and copies the whole array every time a new
		// destination appears. On BenchmarkHub_AddEdge_100k, where every edge
		// lands on a fresh destination, that made the index O(n²): +296% time and
		// +1057% allocation (45.48MiB to 526.10MiB) against the same benchmark
		// without the index at all. Doubling amortises the copy to O(1) per node.
		grown := make([][]graph.NodeID, max(intra+1, 2*uint64(len(sh.srcs))))
		copy(grown, sh.srcs)
		sh.srcs = grown
	}
	sh.srcs[intra] = append(sh.srcs[intra], src)
	sh.mu.Unlock()
}

// remove drops ONE recorded edge slot from src into dst, mirroring the forward
// removal paths, which each excise a single slot. It is a no-op when no such
// slot is recorded, so a double removal cannot drive the count negative.
func (r *revIndex) remove(dst, src graph.NodeID) {
	sh := &r.shards[dst&shardMask]
	intra := uint64(dst) >> shardBits
	sh.mu.Lock()
	if intra >= uint64(len(sh.srcs)) {
		sh.mu.Unlock()
		return
	}
	list := sh.srcs[intra]
	idx := slices.Index(list, src)
	if idx < 0 {
		sh.mu.Unlock()
		return
	}
	// Order within a destination's list carries no meaning — sources reorders
	// what it returns — so excise by swapping the tail element down, which
	// keeps the removal O(1) instead of O(degree).
	last := len(list) - 1
	list[idx] = list[last]
	list[last] = 0
	if last == 0 {
		sh.srcs[intra] = nil
	} else {
		sh.srcs[intra] = list[:last]
	}
	sh.mu.Unlock()
}

// sources returns the DISTINCT nodes holding an edge into dst, excluding dst
// itself, ordered as [graph.Mapper.Walk] would have yielded them.
//
// Both the deduplication and the exclusion of dst reproduce the contract of the
// full scan this index replaced: that scan appended each source key at most
// once however many parallel edges it held, and skipped the destination itself,
// so a self-loop never made a node its own in-neighbour. The Walk ordering is
// reproduced rather than replaced because it is observable — a caller iterating
// in-neighbours to match a pattern sees them in this order — and changing it
// silently would be a behaviour change smuggled in with a performance fix.
func (r *revIndex) sources(dst graph.NodeID) []graph.NodeID {
	sh := &r.shards[dst&shardMask]
	intra := uint64(dst) >> shardBits

	sh.mu.RLock()
	var out []graph.NodeID
	if intra < uint64(len(sh.srcs)) {
		if list := sh.srcs[intra]; len(list) > 0 {
			out = make([]graph.NodeID, len(list))
			copy(out, list)
		}
	}
	sh.mu.RUnlock()

	if len(out) == 0 {
		return nil
	}
	// Mapper.Walk yields shard 0 first and, within a shard, ascending
	// intra-shard index — which is NOT ascending NodeID, because the shard
	// occupies the low bits. Sort on the same pair Walk iterates.
	slices.SortFunc(out, func(x, y graph.NodeID) int {
		if sx, sy := x&shardMask, y&shardMask; sx != sy {
			return int(sx) - int(sy)
		}
		switch ix, iy := uint64(x)>>shardBits, uint64(y)>>shardBits; {
		case ix < iy:
			return -1
		case ix > iy:
			return 1
		default:
			return 0
		}
	})
	out = slices.Compact(out)
	if i := slices.Index(out, dst); i >= 0 {
		out = slices.Delete(out, i, i+1)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// InNeighbourIDs returns the distinct NodeIDs holding an edge into dst,
// excluding dst itself, in [graph.Mapper.Walk] order. The result is a fresh
// slice the caller owns; a node with no incoming edge returns nil.
//
// For an undirected graph every edge is stored in both directions, so the
// answer is the node's neighbour set.
//
// InNeighbourIDs is safe for concurrent use, and takes only the destination's
// own reverse shard lock: it neither blocks nor is blocked by adjacency
// operations on other nodes.
func (a *AdjList[N, W]) InNeighbourIDs(dst graph.NodeID) []graph.NodeID {
	// A graph with no edge has no in-neighbour, so the edge counter the forward
	// path already maintains answers without touching a shard. This is the case
	// a bulk delete of unconnected nodes takes — the shape that exposed #2400.
	if a.size.Load() == 0 {
		return nil
	}
	return a.rev.sources(dst)
}

// InNeighbours returns the keys of the distinct nodes holding an edge into dst,
// excluding dst itself. Keys that the Mapper can no longer resolve are skipped.
//
// InNeighbours is safe for concurrent use.
func (a *AdjList[N, W]) InNeighbours(dst N) []N {
	if a.size.Load() == 0 {
		return nil
	}
	dstID, ok := a.mapper.Lookup(dst)
	if !ok {
		return nil
	}
	ids := a.rev.sources(dstID)
	if len(ids) == 0 {
		return nil
	}
	out := make([]N, 0, len(ids))
	for _, id := range ids {
		if key, ok := a.mapper.Resolve(id); ok {
			out = append(out, key)
		}
	}
	return out
}

// RecordedInEdges reports how many in-edge slots the reverse index currently
// holds, counted on demand across every shard. On a consistent graph it equals
// [AdjList.Size] for a directed graph, and twice it for an undirected one,
// since an undirected edge is stored in both directions.
//
// It exists so tests can assert the index has not drifted from the forward
// adjacency — the failure mode that would matter, because an index missing an
// edge would let DETACH DELETE leave that edge behind. It is O(nodes) and takes
// every shard lock in turn, so it belongs in a test or a diagnostic, never on a
// hot path; that is also why it is not maintained as a counter, which would put
// a shared cache line on the write path to serve an assertion.
func (a *AdjList[N, W]) RecordedInEdges() int64 {
	var total int64
	for i := range a.rev.shards {
		sh := &a.rev.shards[i]
		sh.mu.RLock()
		for _, list := range sh.srcs {
			total += int64(len(list))
		}
		sh.mu.RUnlock()
	}
	return total
}
