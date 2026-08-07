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
// # The rule adjacency uses instead, and the source that settled it
//
// An adjacency APPEND is commutative. Adding A→B and adding A→C are independent
// facts; either order yields the same adjacency, and adding the same arc twice is
// idempotent. Two transactions appending to the SAME source node therefore do not
// conflict, and must not be made to, because on a power-law graph most arcs share
// few source nodes — conflicting on every append would serialise exactly the hot
// path that MVCC exists to open.
//
// This is not a liberty GoGraph is taking. It is what Memgraph does, and the
// distinction is visible in the name of the function it calls: CreateEdge invokes
// PrepareForNonSequentialWrite, not PrepareForWrite, on both endpoint vertices
// (memgraph/memgraph @ b3ac3cd, src/storage/v2/inmemory/storage.cpp CreateEdge →
// src/storage/v2/mvcc.hpp). That function returns NON_SEQUENTIAL rather than
// SERIALIZATION_ERROR when the head delta is itself an edge creation, and says so
// in its own comment: "Check if this is a situation where the entire uncommitted
// delta chain is of edge creations: if so, we can safely add a non-sequential
// delta." A serialization error arises only when a BLOCKING delta — a property,
// a label, an edge removal — sits upstream in the other transaction's chain.
//
// # The two stamps, and why the append must still record one
//
// Memgraph reaches that answer by walking a delta chain and asking
// IsDeltaNonSequential of each link. GoGraph has no chain to walk and no
// per-vertex struct to hang one on, so it keeps the same information as two
// stamps per node:
//
//   - appendTS — the newest COMMUTATIVE adjacency write (an arc appended).
//   - exclusiveTS — the newest NON-COMMUTATIVE adjacency write (an arc removed,
//     a pair cleared, a same-pair slot replaced), and the kind of write a
//     concurrent append must not step over.
//
// The rules, which are the table the user approved:
//
//	append(A→B)      conflicts iff conflicts(exclusiveTS(A)) — bumps appendTS(A)
//	exclusive on A   conflicts iff conflicts(exclusiveTS(A)) or conflicts(appendTS(A))
//	                                                        — bumps exclusiveTS(A)
//
// So append ‖ append commits; append ‖ remove conflicts; remove ‖ remove
// conflicts.
//
// AN APPEND STILL BUMPS ITS STAMP EVEN THOUGH IT NEVER CONFLICTS WITH ANOTHER
// APPEND, and that is the subtle half. Without the bump, `AddEdge(A→C)` followed
// by a concurrent `RemoveEdge(A→B)` would be undetectable in that order: the
// removal would find nothing recorded and proceed, losing the append. The bump is
// what Memgraph gets for free by linking a delta onto the vertex whatever its
// action, and what its has_uncommitted_non_sequential_deltas flag then
// distinguishes.
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

// checkAppend reports the conflict a commutative adjacency write to src would
// hit, or nil. It records nothing.
//
// Only the EXCLUSIVE side can refuse an append: a concurrent append is
// commutative with this one by construction.
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
	if tx == nil {
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
	return nil
}

// stampAppend records that tx appended an arc from src.
//
// It is recorded even though an append cannot conflict with another append: a
// LATER exclusive write on this node must be able to see that an append is in
// flight, or it would silently displace it. See the file comment.
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
		if head := adjEffective(e.exclusiveInfo, e.exclusiveTS); tx.conflicts(head) {
			return tx.conflictErr(mvcc.StoreAdjacency, head)
		}
		if head := adjEffective(e.appendInfo, e.appendTS); tx.conflicts(head) {
			return tx.conflictErr(mvcc.StoreAdjacency, head)
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
