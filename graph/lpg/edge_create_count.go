package lpg

// edge_create_count.go — per-(src,dst) Cypher CREATE multiplicity counter
// used by the openCypher TCK's multi-edge semantics. The underlying
// adjacency list in its simple-graph configuration collapses parallel
// CREATEs of the same (src,dst) into a single edge entry, but the TCK
// asks `MERGE ... RETURN count(r)` to return the *number of CREATE
// calls* the user issued, not the number of distinct storage entries.
// The counter is incremented every time `CreateRelationship` adds an
// edge between the same endpoints, decremented every time a
// `DeleteRelationship` removes one, and read by `MergeRelationship` to
// emit one output row per recorded CREATE call when an existing edge
// satisfies the merge pattern (Merge5 [3]).
//
// The counter is metadata only — it does NOT participate in property
// listings (`NodeProperties`/`EdgeProperties`), label sets, or side-
// effect counters surfaced via [Graph.SideEffectCounters]. Adding it
// as a real edge property would inflate the `+properties` counter the
// TCK harness asserts against.

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// edgeCreateCountShards holds the per-(src,dst) CREATE-call counter.
// Sharded the same way the rest of the LPG's edge metadata is sharded
// (by src NodeID, modulo propMapShards) so concurrent writers from
// different source nodes contend on separate locks.
type edgeCreateCountShard struct {
	mu sync.Mutex
	m  map[edgeKey]int64
}

// IncEdgeCreateCount bumps the CREATE multiplicity counter for the
// directed edge (src, dst) by one. Returns the new count.
//
// Idempotent across "edge already exists" calls: simple-graph
// upsertEdge no-ops the underlying storage write, but the counter
// still moves so a subsequent MERGE sees the correct multiplicity.
//
// IncEdgeCreateCount is safe for concurrent use.
func (g *Graph[N, W]) IncEdgeCreateCount(src, dst N) int64 {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return 0
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return 0
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeCreateCountShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]int64)
	}
	sh.m[k]++
	return sh.m[k]
}

// EdgeCreateCount returns the CREATE multiplicity counter for the
// directed edge (src, dst), or 0 when no CREATE was recorded.
//
// This counter is guarded by its own per-shard mutex and is only
// per-operation atomic: it is NOT cross-store consistent with the
// adjacency layer, [Graph.EdgeLabelsAt], or [Graph.EdgePropertiesAt]
// outside a transaction barrier. A reader that correlates this count
// with the populated per-instance indices (or any other substructure)
// while a multi-CREATE multigraph transaction is committing can observe
// a partial cross-store state — e.g. the count already at 2 while only
// one instance has been populated. To read a consistent cross-store
// view, bracket the correlated reads in [Graph.View] (writers commit
// under [Graph.ApplyAtomically]); see docs/isolation-design.md.
//
// EdgeCreateCount is safe for concurrent use.
func (g *Graph[N, W]) EdgeCreateCount(src, dst N) int64 {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return 0
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return 0
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeCreateCountShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		return 0
	}
	return sh.m[k]
}

// DecEdgeCreateCount decrements the counter by one (floor 0). Used by
// [Graph.RemoveEdge] callers (DELETE) so subsequent MERGEs see the
// updated multiplicity.
//
// DecEdgeCreateCount is safe for concurrent use.
func (g *Graph[N, W]) DecEdgeCreateCount(src, dst N) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeCreateCountShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		return
	}
	if sh.m[k] > 0 {
		sh.m[k]--
	}
	if sh.m[k] == 0 {
		delete(sh.m, k)
	}
}

// edgeCreateCountShardFor returns the shard responsible for key k. The
// shard count matches propMapShards so the same sharding constants
// apply across all edge metadata maps. The shards live on the Graph
// struct directly; see [Graph.edgeCreateCountShards].
func (g *Graph[N, W]) edgeCreateCountShardFor(k edgeKey) *edgeCreateCountShard {
	return &g.edgeCreateCountShards[uint64(k.src)&(propMapShards-1)]
}

// _ ensures the package compiler enforces graph.NodeID is the source/
// destination type used internally; the cast happens automatically via
// the Mapper.Lookup return but pinning the variable here documents the
// intent for future readers.
var _ graph.NodeID
