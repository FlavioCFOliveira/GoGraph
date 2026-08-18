package lpg

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// Adjacency write-write conflict detection (rmp #2300, audit finding on the
// adjacency store).
//
// # Why adjacency could not use the rule every other store uses
//
// Every other versioned store here keeps a per-object delta chain, so a writer
// asks one question — is the newest version at this key visible to me? — and
// [writeCtx.conflicts] answers it. Adjacency keeps no such chain. Its only
// version signal is [Graph.topoGeneration], a single GLOBAL monotonic counter,
// and a global counter cannot distinguish "someone else changed node A" from
// "someone else changed node Z". Applying the standard rule to it would make
// every writer conflict with every other writer that touched the graph at all.
//
// # The rule adjacency uses instead, and how it was revised
//
// The design ORIGINALLY treated an adjacency APPEND as commutative — adding
// A→B and adding A→C are independent facts, so two transactions appending to
// the same source did not conflict, modelled on Memgraph's
// PrepareForNonSequentialWrite (memgraph/memgraph @ b3ac3cd,
// src/storage/v2/inmemory/storage.cpp CreateEdge → src/storage/v2/mvcc.hpp),
// which admits an edge creation over another transaction's uncommitted edge
// creations and refuses only a BLOCKING upstream delta.
//
// rmp #2445 (found by the DST multi-session mode) retired the premise for
// GoGraph: Memgraph mutates per-vertex delta CHAINS, where two pending edge
// creations stay independent records, but GoGraph's adjacency entry is an
// IMMUTABLE SNAPSHOT built from the node's current slot — so the second
// transaction's entry physically EMBEDS the first one's still-pending arc.
// When the embedder commits, every reader sees an uncommitted edge; when the
// arc's owner then aborts, the aborted arc survives inside the committed
// entry, unrecoverably (an immutable snapshot cannot be repaired). The
// commutativity was a property of the operations, not of this representation.
// The approved revision makes the NODE the unit of write-write conflict for
// appends exactly as rmp #2444 made it for removals — which is also what
// Memgraph's ordinary PrepareForWrite enforces for every other same-vertex
// write. Measured on BenchmarkCreateRelationships: no statistically
// significant change (benchstat, 6 samples per arm, interleaved).
//
// # The two stamps
//
// GoGraph has no chain to walk and no per-vertex struct to hang one on, so it
// keeps the conflict information as two stamps per node:
//
//   - appendTS — the newest COMMUTATIVE adjacency write (an arc appended).
//   - exclusiveTS — the newest NON-COMMUTATIVE adjacency write (an arc removed,
//     a pair cleared, a same-pair slot replaced), and the kind of write a
//     concurrent append must not step over.
//
// The rules, as revised by the rmp #2445 decision (the original table let
// appends commute; see [adjVersions.checkAppend] for the entry-snapshot
// embedding that retired it):
//
//	append(A→B)      conflicts iff conflicts(exclusiveTS(A)) or conflicts(appendTS(A))
//	                                                        — bumps appendTS(A)
//	exclusive on A   conflicts iff conflicts(exclusiveTS(A)) or conflicts(appendTS(A))
//	                                                        — bumps exclusiveTS(A)
//
// So append ‖ append conflicts; append ‖ remove conflicts; remove ‖ remove
// conflicts; writes on DISJOINT nodes never conflict. The UNDO replay is
// exempt from all of it (rmp #2445; see [writeCtx.undoing]).
//
// The append's stamp is what a later append or removal on the node tests:
// without it, `AddEdge(A→C)` followed by a concurrent `RemoveEdge(A→B)` (or a
// second append) would be undetectable in that order — the checker would find
// nothing recorded and proceed, losing the append. The bump is what Memgraph
// gets for free by linking a delta onto the vertex whatever its action.
//
// # What this store deliberately does NOT do
//
// It carries no pre-image and takes part in no rollback. Adjacency already has a
// physical undo log, and this store's only job is to answer the conflict
// question. It is therefore write-only bookkeeping, which is why an entry is one
// pair of stamps rather than a delta.

// adjVersionShards is the number of independently locked shards. It matches the
// 64 used by the node-property store, for the same reason: a writer touches one
// shard, so the shard count is the ceiling on concurrent adjacency conflict
// checks, and 64 is past any core count this runs on.
const adjVersionShards = 64

// adjStamps is one node's pair of adjacency write stamps.
//
// Each side is held either as a raw timestamp (a published write) or as the
// *[commitInfo] of the transaction that made it (still in flight), exactly as
// every other versioned store here does — an in-flight write's effective instant
// is its transaction id until the record publishes, and reading it through the
// record is what makes the transition atomic for a concurrent checker.
type adjStamps struct {
	appendInfo    *commitInfo
	exclusiveInfo *commitInfo
	appendTS      uint64
	exclusiveTS   uint64
}

// ts resolves one side's effective instant.
func adjEffective(info *commitInfo, ts uint64) uint64 {
	if info != nil {
		return info.TS()
	}
	return ts
}

// adjVersionShard is one lock and the nodes it covers.
//
// The map is allocated on first write, so a graph nobody writes adjacency to
// through a transaction keeps 64 empty structs and no maps.
type adjVersionShard struct {
	d  map[graph.NodeID]*adjStamps
	mu sync.Mutex
}

// adjVersions is the per-node adjacency conflict index.
//
// # Concurrency contract
//
// Safe for concurrent use. Every method takes the shard lock for the node it
// addresses and holds it only for the map access, so two writers on different
// nodes serialise only when their nodes hash to the same shard.
type adjVersions struct {
	shards [adjVersionShards]adjVersionShard
}

// shard selects the lock covering id.
//
// The multiply-shift mix is there because node ids are dense and sequential: the
// low bits alone would send a contiguous batch of freshly created nodes — exactly
// what a bulk CREATE produces — to the same handful of shards.
func (av *adjVersions) shard(id graph.NodeID) *adjVersionShard {
	h := uint64(id) * 0x9E3779B97F4A7C15
	return &av.shards[(h>>58)%adjVersionShards]
}

// checkAppend reports the conflict an adjacency append to src would hit, or
// nil. It records nothing.
//
// BOTH sides can refuse an append (rmp #2445). The exclusive side always
// could. The append side used to be exempt — "a concurrent append is
// commutative with this one by construction" — and the DST multi-session mode
// disproved the construction: an adjacency ENTRY is an immutable snapshot
// built from the node's current slot, so a second transaction's entry EMBEDS
// the first one's still-pending arc. When the embedder commits, readers see an
// uncommitted edge; when the arc's owner then aborts, the aborted arc survives
// in the committed entry permanently (nothing can rewrite an immutable
// snapshot). The node is therefore the unit of write-write conflict for
// appends exactly as rmp #2444 made it for deletes, which is Memgraph's
// semantics too: PrepareForWrite refuses ANY write on a vertex whose delta
// head is not visible to the writer, edge inserts included
// (src/storage/v2/mvcc.hpp, read 2026-08-02).
//
// Check and record are SEPARATE because they happen either side of the mutation,
// and for different reasons. The check must precede the insert so a doomed
// transaction leaves the adjacency untouched; the record must follow it because
// an append may CREATE its source node, whose id does not exist until the insert
// has run. Stamping before the insert therefore silently skipped every
// edge-creates-its-endpoint write — which is most of a bulk CREATE — and left
// those nodes with no stamp for a later removal to see.
// TestConflict_AdjacencyStampsAreReclaimed caught exactly that.
//
// A nil tx — a direct Go-API mutation outside any transaction — never conflicts:
// it is committed the instant it is made, so there is no window in which another
// transaction could displace it.
func (av *adjVersions) checkAppend(src graph.NodeID, tx *writeCtx) error {
	if tx == nil || tx.undoing.Load() {
		// An UNDO-replay append is the withdrawal of this transaction's own
		// arc removal: it re-adds exactly what the transaction took out, which
		// commutes with every other transaction's writes the way the forward
		// append did. It cannot be refused — the transaction is already
		// rolling back and a skipped inverse leaves its forward write applied
		// (rmp #2445: the adjacency is the one store where another
		// transaction's COMMUTING append legitimately moves the head this
		// transaction wrote under, so the head test refuses an inverse the
		// [writeCtx.undoing] doomed-shortcut exemption was designed to admit).
		return nil
	}
	sh := av.shard(src)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e := sh.d[src]
	if e == nil {
		return nil
	}
	if head := adjEffective(e.exclusiveInfo, e.exclusiveTS); tx.conflicts(head) {
		return tx.conflictErr(mvcc.StoreAdjacency, head)
	}
	if head := adjEffective(e.appendInfo, e.appendTS); tx.conflicts(head) {
		return tx.conflictErr(mvcc.StoreAdjacency, head)
	}
	return nil
}

// stampAppend records that tx appended an arc from src.
//
// The stamp is what a later append or exclusive write on this node tests in
// [adjVersions.checkAppend] / [adjVersions.noteExclusive]: since rmp #2445 an
// append conflicts with a foreign in-flight (or invisible-committed) append on
// the same node, because adjacency entries are immutable snapshots that embed
// whatever the slot held when they were built. See the file comment.
//
// Charging a stamp to a nil tx would leave an instant no record will ever
// publish, so an untransacted write records nothing.
func (av *adjVersions) stampAppend(src graph.NodeID, tx *writeCtx) {
	if tx == nil {
		return
	}
	sh := av.shard(src)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e := sh.d[src]
	if e == nil {
		e = &adjStamps{}
		if sh.d == nil {
			sh.d = make(map[graph.NodeID]*adjStamps, 8)
		}
		sh.d[src] = e
	}
	e.appendInfo, e.appendTS = tx.record(), tx.txID
}

// noteExclusive records a non-commutative adjacency write to src by tx — an arc
// removed, a pair cleared, a same-pair slot replaced — and reports the conflict
// it hit, or nil.
//
// Unlike an append, this consults BOTH sides: it may not step over another
// transaction's in-flight append any more than over its removal.
func (av *adjVersions) noteExclusive(src graph.NodeID, tx *writeCtx) error {
	if tx == nil {
		return nil
	}
	sh := av.shard(src)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e := sh.d[src]
	if e != nil {
		// An UNDO-replay removal withdraws exactly the arc this transaction
		// appended, which commutes with every other transaction's appends —
		// and a refusal cannot be answered mid-rollback: the skipped inverse
		// leaves the rolled-back edge applied and committed (rmp #2445, found
		// by the DST multi-session mode as a leaked edge after a voluntary
		// rollback that overlapped a committed append on the same node). The
		// claim below is still stamped, so later writers order against the
		// rollback's publication exactly as against any other write.
		if !tx.undoing.Load() {
			if head := adjEffective(e.exclusiveInfo, e.exclusiveTS); tx.conflicts(head) {
				return tx.conflictErr(mvcc.StoreAdjacency, head)
			}
			if head := adjEffective(e.appendInfo, e.appendTS); tx.conflicts(head) {
				return tx.conflictErr(mvcc.StoreAdjacency, head)
			}
		}
	} else {
		e = &adjStamps{}
		if sh.d == nil {
			sh.d = make(map[graph.NodeID]*adjStamps, 8)
		}
		sh.d[src] = e
	}
	e.exclusiveInfo, e.exclusiveTS = tx.record(), tx.txID
	return nil
}

// truncate drops every entry whose BOTH sides are at or below watermark.
//
// Those stamps can no longer refuse anything: [mvcc.Conflicts] is false for a
// head below any live transaction's start, so keeping the entry only costs
// memory. Called by the reclaimer on the same watermark every other store uses.
//
// An entry with one side above the watermark is kept whole rather than half
// cleared: the pair is two words, and clearing one side would cost a second
// branch on the write path for no measurable memory.
func (av *adjVersions) truncate(watermark uint64) (freed int) {
	for i := range av.shards {
		sh := &av.shards[i]
		sh.mu.Lock()
		for id, e := range sh.d {
			a := adjEffective(e.appendInfo, e.appendTS)
			x := adjEffective(e.exclusiveInfo, e.exclusiveTS)
			if a <= watermark && x <= watermark {
				delete(sh.d, id)
				freed++
			}
		}
		if len(sh.d) == 0 {
			sh.d = nil
		}
		sh.mu.Unlock()
	}
	return freed
}

// len reports how many nodes currently carry an adjacency stamp, for
// observability and for the reclaim tests.
func (av *adjVersions) len() (n int) {
	for i := range av.shards {
		sh := &av.shards[i]
		sh.mu.Lock()
		n += len(sh.d)
		sh.mu.Unlock()
	}
	return n
}
