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
// appends commute; see [adjVersions.claimAppend] for the entry-snapshot
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
	// floorTS is the newest COMMIT timestamp a stamp displaced from either side,
	// or zero. Every check tests it beside the two sides; see [adjStamps.set].
	floorTS uint64
}

// set records rec (tx's commit record, effective instant txID) on one side —
// *info and *ts are that side's fields — first folding the value it displaces
// into floorTS when that value is a COMMITTED write of another transaction.
//
// # Why a displaced commit must outlive its slot (rmp #2997)
//
// Each side is ONE slot, so a write that stamps a node overwrites the stamp it
// found. A commit is only overwritten by a transaction that can see it, so the
// overwrite loses nothing while the overwriter lives: in flight it refuses every
// other writer, and once committed its own instant is later than the one it
// replaced. But when the overwriter ABORTS, [adjVersionShard.clearAbortedLocked]
// clears its side, and with nothing else recording it the displaced commit was
// gone: a transaction whose snapshot predates that commit then found the node
// unstamped and wrote over a change it never saw. Measured on the Cypher path as
// a committed DETACH DELETE of a hub beside the incoming arcs a peer committed
// after the deleter's snapshot — the deleter's snapshot lists no such arc to
// remove, and the stamp that should have refused its claim on the hub had been
// wiped by an appender refused at the hub's death claim (an append claims the
// hub, then loses the existence cross-check and aborts).
//
// The floor keeps the newest such commit for as long as the entry lives, which is
// exactly the answer the lost predecessor would have given: every check tests the
// two sides and the floor alike, so a node conflicts for tx iff some write it
// still records — the current ones or any displaced commit — is invisible to tx.
// It refuses nothing a kept slot would not have refused, because a displaced
// commit is older than the in-flight or committed stamp that displaced it. Memgraph
// reaches the same end by unlinking an aborted transaction's deltas from the
// object's chain, which leaves the older committed delta at the head for
// PrepareForWrite to test (memgraph/memgraph @ 6e9c79d,
// src/storage/v2/inmemory/storage.cpp, InMemoryAccessor::Abort, the edge pass
// setting the head to the first delta past the aborted ones; mvcc.hpp
// PrepareForWrite).
//
// A displaced write of the same transaction, an aborted one, and a still
// in-flight one of another transaction are not folded: the first is replaced by
// the same record, the second protects nothing, and the third is reachable only
// by a write that skips the test — an undo replay or [adjVersions.stampAppend] on
// a node this transaction is creating — whose displaced writer is refused by the
// existence cross-check of the node it is racing to create.
func (e *adjStamps) set(info **commitInfo, ts *uint64, rec *commitInfo, txID uint64) {
	if *info != rec {
		if h := adjEffective(*info, *ts); h != 0 && h < mvcc.TxIDBase && h > e.floorTS {
			e.floorTS = h
		}
	}
	*info, *ts = rec, txID
}

// blocking returns the instant of the first write recorded in e that tx may not
// write over — the exclusive side, the append side, then the floor of displaced
// commits ([adjStamps.set]) — or false when there is none.
func (e *adjStamps) blocking(tx *writeCtx) (uint64, bool) {
	if head := adjEffective(e.exclusiveInfo, e.exclusiveTS); tx.conflicts(head) {
		return head, true
	}
	if head := adjEffective(e.appendInfo, e.appendTS); tx.conflicts(head) {
		return head, true
	}
	if e.floorTS != 0 && tx.conflicts(e.floorTS) {
		return e.floorTS, true
	}
	return 0, false
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
	// grown records that d has held more than [adjKeepEntries] entries since it
	// was allocated; see [adjVersionShard.releaseIfEmptyLocked].
	grown bool
}

// adjKeepEntries is the largest map an emptied shard keeps for reuse.
//
// A direct write is an implicit transaction that publishes at once, so the next
// vacuum pass finds every stamp it made below the watermark and empties the
// shard. Releasing the emptied map made the next write on that shard allocate a
// new one: measured at one map allocation per 3.5 direct writes on an
// AddEdge+SetEdgeLabel build (rmp #3025). An emptied map is kept instead, but
// only while it has never held more than this many entries: Go maps do not
// shrink, so a map that once grew past one group is released as before, and
// the memory an idle graph keeps is bounded by one smallest map per shard.
const adjKeepEntries = 8

// entryLocked returns id's stamps, creating an empty entry when there is none.
// The caller holds the shard lock.
func (sh *adjVersionShard) entryLocked(id graph.NodeID) *adjStamps {
	if e := sh.d[id]; e != nil {
		return e
	}
	return sh.newEntryLocked(id)
}

// newEntryLocked creates an empty entry for id, which has none, allocating the
// shard's map when it has been released. The caller holds the shard lock.
func (sh *adjVersionShard) newEntryLocked(id graph.NodeID) *adjStamps {
	e := &adjStamps{}
	if sh.d == nil {
		sh.d = make(map[graph.NodeID]*adjStamps, adjKeepEntries)
	}
	sh.d[id] = e
	if len(sh.d) > adjKeepEntries {
		sh.grown = true
	}
	return e
}

// releaseIfEmptyLocked drops an emptied map that has grown past
// [adjKeepEntries] and keeps a smaller one for the next write (rmp #3025). The
// caller holds the shard lock.
func (sh *adjVersionShard) releaseIfEmptyLocked() {
	if len(sh.d) == 0 && sh.grown {
		sh.d, sh.grown = nil, false
	}
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
	return &av.shards[av.shardIndex(id)]
}

// shardIndex is the index of the shard [adjVersions.shard] selects for id.
func (av *adjVersions) shardIndex(id graph.NodeID) int {
	h := uint64(id) * 0x9E3779B97F4A7C15
	return int((h >> 58) % adjVersionShards)
}

// claimAppend tests an adjacency append to src, which already exists, and stamps
// it in one step under src's shard lock, returning the conflict it hit, or nil.
// A refused claim records nothing.
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
// Test and stamp are ONE observation (rmp #2947): of two appenders to the same
// node exactly one passes and the other sees its stamp. They used to be two —
// the test before the insert, the stamp after it — and a second appender that
// passed the test in between went on to create any endpoint it was adding
// before the entry itself refused it, and an aborted creation leaves that key
// tombstoned. The stamp still FOLLOWS the insert for a node the append creates,
// whose id does not exist before it: [adjVersions.stampAppend]. Stamping only
// before the insert silently skipped every edge-creates-its-endpoint write —
// which is most of a bulk CREATE — and left those nodes with no stamp for a
// later removal to see. TestConflict_AdjacencyStampsAreReclaimed caught exactly
// that.
//
// An UNDO-replay append is the withdrawal of this transaction's own arc removal:
// it re-adds exactly what the transaction took out, which commutes with every
// other transaction's writes the way the forward append did. It is never refused
// — the transaction is already rolling back and a skipped inverse leaves its
// forward write applied (rmp #2445: the adjacency is the one store where another
// transaction's COMMUTING append legitimately moves the head this transaction
// wrote under, so the head test refuses an inverse the [writeCtx.undoing]
// doomed-shortcut exemption was designed to admit) — and it is still stamped,
// so later writers order against the rollback's publication.
//
// A nil tx — a write on a disarmed graph — records nothing and never conflicts.
func (av *adjVersions) claimAppend(src graph.NodeID, tx *writeCtx) error {
	if tx == nil {
		return nil
	}
	sh := av.shard(src)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e := sh.d[src]
	if e != nil && !tx.undoing.Load() {
		if head, ok := e.blocking(tx); ok {
			return tx.conflictErr(mvcc.StoreAdjacency, head)
		}
	}
	if e == nil {
		e = sh.newEntryLocked(src)
	}
	e.set(&e.appendInfo, &e.appendTS, tx.record(), tx.txID)
	tx.tx.Touch(touchedAdjClaims)
	tx.noteSide(sideAdjClaim, uint64(src), 0, 0)
	return nil
}

// claimAppendPair is [adjVersions.claimAppend] for the existing endpoints of one
// append, in ONE test-then-stamp step: it takes both endpoints' shard locks in
// ascending shard order, tests both, and stamps both only when neither refuses.
// ids[:n] are the endpoints (n is 0, 1 or 2; a pair in one shard is locked once).
//
// Testing both before stamping either is what lets a refused append allocate
// nothing (rmp #2965, audit finding 4): a stamp needs the transaction's commit
// record, so claiming the source and then being refused on the destination
// used to allocate a record, and a direct write's implicit transaction then had
// to abort with one, which wakes the abort withdrawal and the vacuum. Refused
// here, the transaction has written nothing and simply ends.
//
// Lock order: two shards of this index are only ever held together here, in
// ascending order, and no other lock is taken while they are held.
func (av *adjVersions) claimAppendPair(ids [2]graph.NodeID, n int, tx *writeCtx) error {
	if tx == nil || n == 0 {
		return nil
	}
	if n == 1 {
		return av.claimAppend(ids[0], tx)
	}
	i0, i1 := av.shardIndex(ids[0]), av.shardIndex(ids[1])
	lo, hi := i0, i1
	if lo > hi {
		lo, hi = hi, lo
	}
	av.shards[lo].mu.Lock()
	defer av.shards[lo].mu.Unlock()
	if hi != lo {
		av.shards[hi].mu.Lock()
		defer av.shards[hi].mu.Unlock()
	}
	if !tx.undoing.Load() {
		for _, id := range ids {
			e := av.shards[av.shardIndex(id)].d[id]
			if e == nil {
				continue
			}
			if head, ok := e.blocking(tx); ok {
				return tx.conflictErr(mvcc.StoreAdjacency, head)
			}
		}
	}
	rec := tx.record()
	for _, id := range ids {
		e := av.shards[av.shardIndex(id)].entryLocked(id)
		e.set(&e.appendInfo, &e.appendTS, rec, tx.txID)
		tx.noteSide(sideAdjClaim, uint64(id), 0, 0)
	}
	tx.tx.Touch(touchedAdjClaims)
	return nil
}

// admits tests, WITHOUT stamping, whether tx may write the adjacency of each
// of the nodes ids[:n], and returns the conflict it hit, or nil. It is the test
// [adjVersions.claimAppendPair] and [adjVersions.noteExclusive] make before they
// stamp, for a write that has found, in the STORED entry, nothing to do: such a
// verdict must not rest on another transaction's uncommitted entry (ACID audit
// round 6, finding C1), and recording a stamp for a write that changes nothing
// would turn it into an effect. The undo replay is exempt, as it is from the
// claims. A nil tx never conflicts.
func (av *adjVersions) admits(ids [2]graph.NodeID, n int, tx *writeCtx) error {
	if tx == nil || tx.undoing.Load() {
		return nil
	}
	for _, id := range ids[:n] {
		sh := av.shard(id)
		sh.mu.Lock()
		e := sh.d[id]
		if e != nil {
			if head, ok := e.blocking(tx); ok {
				sh.mu.Unlock()
				return tx.conflictErr(mvcc.StoreAdjacency, head)
			}
		}
		sh.mu.Unlock()
	}
	return nil
}

// stampAppend records that tx appended an arc from src.
//
// The stamp is what a later append or exclusive write on this node tests in
// [adjVersions.claimAppend] / [adjVersions.noteExclusive]: since rmp #2445 an
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
	e := sh.entryLocked(src)
	e.set(&e.appendInfo, &e.appendTS, tx.record(), tx.txID)
	tx.tx.Touch(touchedAdjClaims)
	tx.noteSide(sideAdjClaim, uint64(src), 0, 0)
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
			if head, ok := e.blocking(tx); ok {
				return tx.conflictErr(mvcc.StoreAdjacency, head)
			}
		}
	} else {
		e = sh.newEntryLocked(src)
	}
	e.set(&e.exclusiveInfo, &e.exclusiveTS, tx.record(), tx.txID)
	tx.tx.Touch(touchedAdjClaims)
	tx.noteSide(sideAdjClaim, uint64(src), 0, 0)
	return nil
}

// truncate drops every entry whose BOTH sides and floor are at or below
// watermark.
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
			if a <= watermark && x <= watermark && e.floorTS <= watermark {
				delete(sh.d, id)
				freed++
			}
		}
		sh.releaseIfEmptyLocked()
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
