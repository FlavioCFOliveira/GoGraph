package lpg

// mvcc_life.go — versioned node EXISTENCE (rmp #2290, MVCC P4c).
//
// # The defect this closes
//
// Every store a read touches is versioned, so a read gets the right CONTENT of
// every object. Nothing versions which objects exist, and P4b's own failures
// showed both halves of what that costs once the barrier is gone:
//
//   - A node CREATED after the reader started is still in the mapper, so the
//     scan hands it to the query, and the versioned stores correctly report it
//     as having no labels and no properties. The query emits a ROW OF NULLS for
//     a node that did not exist yet. Two tests caught exactly this.
//   - A node DELETED after the reader started is in the tombstone set, which is
//     read at the present, so the scan skips it. The reader loses a row it
//     should have. That direction is silent.
//
// # Why timestamps and not a delta chain
//
// The other stores hold a VALUE with a history, so they need a chain. Existence
// is a pair of instants — created at, deleted at — and a pair of instants is
// what PostgreSQL puts in every tuple header as xmin/xmax, because the
// visibility test is then two comparisons and no walk. This takes the same
// shape.
//
// # Why the records are SPARSE
//
// PostgreSQL can afford xmin/xmax per tuple because it is writing a page
// anyway. GoGraph has no per-node struct — that is the constraint the whole
// design works under — so a permanent pair of timestamps per node would be
// sixteen bytes on every node of every graph, including the graphs that never
// delete anything.
//
// So only nodes born or deleted RECENTLY carry a record, in a lazily-allocated
// per-shard map reclaimed by the same watermark as everything else. Once no
// reader can be older than a node's birth, the record is redundant — the node
// simply exists — and it goes. A graph that is not churning holds none, and the
// existence test is the lock-free counter check it was before.

import (
	"slices"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// lifeStamp is when something happened to a node: the transaction's shared
// commit record, or an inline commit timestamp for an untransacted mutation —
// the same union every other store here uses, for the same reason.
type lifeStamp struct {
	info *commitInfo
	ts   uint64
	// seq orders two events that a timestamp cannot separate. A node removed
	// and re-created inside ONE transaction — which is exactly what a
	// rolled-back DELETE is, the undo log reviving what the statement
	// tombstoned — stamps both events with the same shared commit record, so
	// they carry the same instant and "which happened last" has no answer in
	// the timestamps. This is assigned in write order and does. Without it the
	// node vanished for every reader afterwards; TestRunInTx_DeleteReturn
	// caught it.
	seq uint64
	// primordial records whether the node was ALIVE before this record chain's
	// EARLIEST event. It is what answers a reader that can see NEITHER of the
	// two recorded events: such a reader observes the state before the chain
	// began, and the chain itself cannot say what that state was — the records
	// are only two deep, so a birth+death pair looks identical whether the
	// node was fresh (create then delete in one uncommitted transaction: it
	// never existed for that reader) or ancient (delete then revive: it very
	// much existed). Set on the FIRST record for the id — false when that
	// first event is a birth, true when it is a death — and propagated to
	// every later record under the same shard lock, so both records always
	// carry the chain's answer. Without it an outside reader saw a bare
	// phantom node while a create+delete transaction was still open (rmp
	// #2443, found by the DST multi-session mode).
	primordial bool
}

// at returns the instant, resolving the shared record when there is one.
func (s lifeStamp) at() uint64 {
	if s.info != nil {
		return s.info.TS()
	}
	return s.ts
}

// visibleTo reports whether an event stamped this way had already happened, as
// far as a reader at (startTS, txID) is concerned.
func (s lifeStamp) visibleTo(startTS, txID uint64) bool {
	return mvcc.Visible(s.at(), startTS, txID)
}

// nodeLifeShard holds the birth and death records of the nodes in one shard.
//
// Both maps are nil until something is recorded, so a graph that never deletes
// and is not being read against an older snapshot allocates neither.
type nodeLifeShard struct {
	born map[graph.NodeID]lifeStamp
	died map[graph.NodeID]lifeStamp
	mu   sync.RWMutex
}

// nodeLifeShardFor selects the shard responsible for id.
func (g *Graph[N, W]) nodeLifeShardFor(id graph.NodeID) *nodeLifeShard {
	return &g.nodeLifeShards[uint64(id)&(propMapShards-1)]
}

// noteNodeBorn records that id came into existence now.
//
// Called from the node-creation path with no lock held. It is the ONLY place a
// birth is recorded, so a node with no record is one that has existed for
// longer than any reader can remember — which is what makes the absence of a
// record mean "exists".
func (g *Graph[N, W]) noteNodeBorn(id graph.NodeID, tx *writeCtx) bool {
	return g.noteNodeLife(id, tx, true)
}

// noteNodeBornAutocommit is [Graph.noteNodeBorn] outside any transaction, in the
// shape [graph.Mapper.InternNewHook] takes.
//
// It exists so the autocommit AddNode path passes a bare method value, as it did
// before the transaction was threaded through, rather than a closure over a nil
// pointer.
func (g *Graph[N, W]) noteNodeBornAutocommit(id graph.NodeID) {
	g.noteNodeLife(id, nil, true)
}

// noteNodeDied records that id was removed now.
func (g *Graph[N, W]) noteNodeDied(id graph.NodeID, tx *writeCtx) bool {
	return g.noteNodeLife(id, tx, false)
}

// noteNodeRevived records that a tombstoned id is live again.
//
// A revival is a BIRTH as far as a reader is concerned: from this instant the
// node exists, and before it the death record still applies.
func (g *Graph[N, W]) noteNodeRevived(id graph.NodeID, tx *writeCtx) bool {
	return g.noteNodeLife(id, tx, true)
}

// noteNodeLife records a birth (alive) or a death, and reports whether the
// change may proceed.
//
// The three entry points became one function when write-write conflict
// detection arrived (rmp #2300), because the check has to run under the SAME
// shard lock as the write it guards. Held separately — read the head, release,
// then take the lock to write — two writers can both pass the check and both
// write, which is the lost update the check exists to prevent. The node-label
// and node-property paths already check inside their shard lock for the same
// reason.
//
// The death record is KEPT across a birth: a reader older than the birth but
// newer than the death must still see the node as gone, and only the record can
// tell it that. [Graph.NodeExistsAsOf] decides between the two by taking the
// later of the events that reader can see.
func (g *Graph[N, W]) noteNodeLife(id graph.NodeID, tx *writeCtx, alive bool) bool {
	if !g.mvccArmed {
		return true
	}
	sh := g.nodeLifeShardFor(id)
	sh.mu.Lock()
	// A BIRTH on a slot with NO life record displaces nobody's version, so it
	// is exempt from the doomed-transaction refusal: the mapper has ALREADY
	// interned the slot by the time this hook runs, and refusing to record the
	// birth left the slot as a permanently visible bare node once the
	// transaction aborted — no record means "exists" to every reader, and the
	// abort reclaim had nothing to withdraw. Recording it instead stamps the
	// slot with the doomed transaction, and [Graph.reclaimAbortedLife]
	// tombstones it when the abort is processed (rmp #2444, found by the DST
	// multi-session mode: a CREATE on an already-doomed transaction leaked its
	// slot). A genuine collision (head != 0) is still refused.
	if head := sh.headStamp(id); tx.conflicts(head) && (!alive || head != 0) {
		sh.mu.Unlock()
		_ = tx.conflictErr(mvcc.StoreNodeExistence, head)
		return false
	}
	// Inside the lock, so the record this write lands on is the one the check
	// just cleared. deltaStamp allocates the transaction's commit record on
	// first use; it takes no lock of its own and cannot reach back here.
	info, ts := g.deltaStamp(tx.record())
	seq := g.lifeSeq.Add(1)
	// Propagate the chain's alive-before-epoch answer from whichever record
	// already exists; on the chain's FIRST record, derive it from the event
	// itself (a first birth means the node was NOT alive before; a first death
	// means it was). See [lifeStamp.primordial].
	var prim bool
	if b, ok := sh.born[id]; ok {
		prim = b.primordial
	} else if d, ok := sh.died[id]; ok {
		prim = d.primordial
	} else {
		prim = !alive
	}
	st := lifeStamp{info: info, ts: ts, seq: seq, primordial: prim}
	if alive {
		if sh.born == nil {
			sh.born = make(map[graph.NodeID]lifeStamp, 8)
		}
		sh.born[id] = st
	} else {
		if sh.died == nil {
			sh.died = make(map[graph.NodeID]lifeStamp, 8)
		}
		sh.died[id] = st
	}
	sh.mu.Unlock()
	g.nodeLifeActive.Add(1)
	return true
}

// headStamp returns the effective timestamp of the LATEST life event recorded
// for id — birth or death, whichever happened last — or zero when the node has
// no record at all.
//
// The caller must hold the shard lock. The two maps are one logical version
// chain two entries deep, ordered by seq rather than by timestamp: both events
// of one transaction share a commit record and therefore an identical
// timestamp, and seq is what [Graph.NodeExistsAsOf] already uses to order them.
func (sh *nodeLifeShard) headStamp(id graph.NodeID) uint64 {
	born, hasBorn := sh.born[id]
	died, hasDied := sh.died[id]
	switch {
	case hasBorn && hasDied:
		if died.seq > born.seq {
			return died.at()
		}
		return born.at()
	case hasBorn:
		return born.at()
	case hasDied:
		return died.at()
	}
	return 0
}

// NodeExistsAsOf reports whether id was a live node at s.
//
// A nil snapshot asks about the present, which is the tombstone check the read
// path has always made.
//
// # The rule
//
// A node exists for a reader when its birth is visible to that reader and its
// death is not. "No record" resolves the same way it does everywhere else here:
// no birth record means it was born before anything this reader can remember,
// and no death record means it is not dead.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodeExistsAsOf(id graph.NodeID, s *Snapshot) bool {
	if s == nil {
		return !g.IsTombstoned(id)
	}
	// The gate is this shard's OWN maps, read under its lock — not a
	// graph-level counter read before it. A counter sampled first can report
	// zero while the very record this call needs is being written, which is the
	// class of bug that tore example 27's bank-transfer invariant; see
	// [Graph.propBagAsOf]. The lock is uncontended and the two nil checks are in
	// the same cache line as the maps.
	sh := g.nodeLifeShardFor(id)
	sh.mu.RLock()
	if sh.born == nil && sh.died == nil {
		sh.mu.RUnlock()
		return !g.IsTombstoned(id)
	}
	born, hasBorn := sh.born[id]
	died, hasDied := sh.died[id]
	sh.mu.RUnlock()

	bornVisible := hasBorn && born.visibleTo(s.startTS, s.txID)
	diedVisible := hasDied && died.visibleTo(s.startTS, s.txID)

	if hasBorn && !bornVisible {
		// The recorded birth is in this reader's future (or belongs to a
		// transaction it cannot see), so the birth contributes nothing.
		if !hasDied || diedVisible {
			// Never removed — it does not exist yet for this reader; or the
			// death IS visible while the (re)birth is not — it is gone.
			return false
		}
		// NEITHER event is visible: the reader observes the state before the
		// chain's earliest event, which only the primordial flag can answer. A
		// fresh create+delete inside one still-invisible transaction reads
		// false — without this an outside reader saw the node as a bare
		// phantom, all its label and property versions invisible (rmp #2443,
		// caught by the DST multi-session mode). A delete+revive pair on an
		// ancient node reads true — the removal is in the reader's future.
		return died.primordial
	}
	switch {
	case bornVisible && diedVisible:
		// Both events are in this reader's past, so the LATER one decides. A
		// node can be removed and re-created — a rolled-back DELETE does
		// exactly that, the undo log reviving what the statement tombstoned —
		// and taking the death unconditionally made the node vanish for every
		// reader afterwards. TestRunInTx_DeleteReturn caught it.
		//
		// The comparison is by SEQUENCE, not by timestamp: inside one
		// transaction both events carry the same shared commit record and
		// therefore the same instant, so only the write order separates them.
		return born.seq > died.seq
	case diedVisible:
		// A death record beats the tombstone bitmap in BOTH directions: visible
		// means gone even if the bitmap has not been updated, and not-visible
		// means still here even though the bitmap says otherwise. The bitmap is
		// the present; this reader is not.
		return false
	case bornVisible:
		return true
	case hasDied:
		// The removal is in this reader's future, so the node is still here —
		// which is the direction the tombstone bitmap alone cannot express.
		return true
	}
	return !g.IsTombstoned(id)
}

// reclaimNodeLife drops the birth and death records the watermark has made
// redundant, and returns how many it released.
//
// A BIRTH at or before the watermark is redundant: every reader began at or
// after it, so all of them see the node as existing, which is what no record
// already means.
//
// A DEATH is NOT redundant when it is old — an old death is the reason the node
// is absent, and dropping it would fall back to the tombstone bitmap. That
// happens to give the same answer, because the bitmap records exactly the
// deaths that have happened; so an old death record IS redundant, for the same
// reason as a birth. The two are symmetrical only because the bitmap is kept in
// lockstep with the death records, which [Graph.RemoveNode] does.
//
// The caller must exclude concurrent writers and other sweeps, exactly as the
// other reclaimers require.
func (g *Graph[N, W]) reclaimNodeLife(watermark uint64) int {
	if g.nodeLifeActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.nodeLifeShards {
		sh := &g.nodeLifeShards[i]
		sh.mu.Lock()
		for id, st := range sh.born {
			if st.at() <= watermark {
				delete(sh.born, id)
				freed++
			}
		}
		for id, st := range sh.died {
			if st.at() <= watermark {
				delete(sh.died, id)
				freed++
			}
		}
		if len(sh.born) == 0 {
			sh.born = nil
		}
		if len(sh.died) == 0 {
			sh.died = nil
		}
		sh.mu.Unlock()
	}
	if freed > 0 {
		g.nodeLifeActive.Add(-int64(freed))
	}
	return freed
}

// LiveCountExactAsOf reports whether the CURRENT live node count is also the
// count this reader should see.
//
// It is, when no node was born or removed after the reader started: the present
// and the reader's instant then contain exactly the same nodes, however many
// records happen to be retained. That is a very different question from "is any
// record live at all", which is what the first version asked — and asking the
// weaker one made `MATCH (n) RETURN count(*)` decline its O(1) answer and walk
// every node under a per-node lock the moment ANY write had happened, which
// measured 2.5x on BenchmarkEngReadUnderWriter against a saturating writer.
//
// The scan is over the life shards' own maps, which hold only the churn the
// reclaimer has not caught up with — sixteen uncontended read locks once per
// QUERY, against one per node.
//
// Safe for concurrent use.
func (g *Graph[N, W]) LiveCountExactAsOf(s *Snapshot) bool {
	if s == nil || g.nodeLifeActive.Load() == 0 {
		return true
	}
	for i := range g.nodeLifeShards {
		sh := &g.nodeLifeShards[i]
		sh.mu.RLock()
		for _, st := range sh.born {
			if !st.visibleTo(s.startTS, s.txID) {
				sh.mu.RUnlock()
				return false
			}
		}
		for _, st := range sh.died {
			if !st.visibleTo(s.startTS, s.txID) {
				sh.mu.RUnlock()
				return false
			}
		}
		sh.mu.RUnlock()
	}
	return true
}

// NodeLifeVersionCount returns the number of live birth and death records.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodeLifeVersionCount() int64 { return g.nodeLifeActive.Load() }

// TombstonedIDsAsOf returns, in ascending order, every interned node id that did NOT
// exist as of s — the set a snapshot must record so a recovered graph has the same
// live nodes the image was taken from (rmp #2310).
//
// A nil snapshot returns [Graph.TombstonedIDs], the present-time answer.
//
// # Why it does not read the tombstone bitmap
//
// The bitmap is a COW accelerator maintained beside the versioned truth, and it
// answers "is this node removed NOW". A capture that no longer excludes writers needs
// "was this node removed as of s", and those differ by exactly the transactions that
// committed during the capture — which is the whole class of partial-transaction image
// this task exists to make impossible. So the authoritative store is consulted:
// [Graph.NodeExistsAsOf], which resolves the node's birth and death records against s.
//
// The cost is one existence test per interned id, O(V). A capture already walks every
// node to serialise its labels, properties and adjacency, so this adds a constant
// factor to a walk that must happen anyway rather than a new traversal — and the
// present-time form's bitmap read is not available at an arbitrary instant at any
// price, because the bitmap keeps no history.
//
// Safe for concurrent use.
func (g *Graph[N, W]) TombstonedIDsAsOf(s *Snapshot) []graph.NodeID {
	if s == nil {
		return g.TombstonedIDs()
	}
	// SORTED EXPLICITLY. This said "ascending by construction: the mapper walks ids in
	// increasing order", and that is false for any graph with more than one node in
	// some shard. A NodeID packs as (intra << shardBits) | shard and Walk is
	// shard-major, so a 2000-node graph walks 0, 256, 512, 768, 1, 257, … — ascending
	// only while every shard holds a single node, which is why it survived being
	// written down. The present-time form returns the roaring bitmap's ToArray, which
	// genuinely is ascending, so without this sort the two forms of the same function
	// would disagree on the order of their result and the snapshot writer's documented
	// input contract would hold on one path and not the other.
	//
	// The cost is O(D log D) in the number of TOMBSTONES, against the O(V) existence
	// walk it follows.
	out := make([]graph.NodeID, 0, g.TombstoneCount())
	g.adj.Mapper().Walk(func(id graph.NodeID, _ N) bool {
		// INTERNED as of s AND not alive as of s. Both halves are load-bearing.
		//
		// An id interned AFTER s is also "not alive as of s", but it is not a tombstone —
		// it is a node the image does not carry at all, because the mapper is filtered by
		// the same instant. Listing it here names an id the image has no entry for, and
		// the manifest then disagrees with what a recovery reconstructs: measured at
		// manifest Order=1552 against a reconstructed 1542.
		//
		// A tombstone is only meaningful for a node the image HOLDS: interned by the
		// instant, and removed by it.
		if g.NodeInternedAsOf(id, s) && !g.NodeExistsAsOf(id, s) {
			out = append(out, id)
		}
		return true
	})
	slices.Sort(out)
	return out
}

// NodeInternedAsOf reports whether id had been INTERNED at or before s — that is,
// whether the node existed at any point up to that instant, whether or not it had
// already been removed by then.
//
// A nil snapshot reports whether the id is interned at all.
//
// # Why this is a different question from NodeExistsAsOf, and who needs it
//
// [Graph.NodeExistsAsOf] answers "is this node ALIVE as of s". A snapshot image needs
// a weaker question for its MAPPER: the id→key table must carry every id the image can
// reference, and that includes ids whose node was removed before s — those are in the
// mapper AND in the tombstone set, which is how a removal survives a restart.
//
// What the mapper must NOT carry is an id interned AFTER s. Before rmp #2310 that
// could not happen, because the capture excluded writers and the mapper could not
// grow during it. It can now, and it was measured the moment the exclusion was
// removed: TestCheckpoint_CaptureIsAtomic_SnapshotOnlyArtefact recovered Order=322
// against Size=157, eight nodes above the 2*Size the fixture guarantees — exactly the
// endpoints of four transactions that committed during the capture and whose edges
// were correctly excluded.
//
// A node with no birth record is treated as interned: it predates the versioned life
// store, or its record has been reclaimed, and in both cases its birth is in the past
// of every live reader.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodeInternedAsOf(id graph.NodeID, s *Snapshot) bool {
	if s == nil {
		return true
	}
	sh := g.nodeLifeShardFor(id)
	sh.mu.RLock()
	if sh.born == nil {
		sh.mu.RUnlock()
		return true
	}
	born, hasBorn := sh.born[id]
	sh.mu.RUnlock()
	if !hasBorn {
		// No birth record: reclaimed or pre-versioning, so it is in every reader's
		// past. A node born after s ALWAYS has one, because reclamation cannot free a
		// record newer than the oldest live reader — and s is a live reader.
		return true
	}
	return born.visibleTo(s.startTS, s.txID)
}
