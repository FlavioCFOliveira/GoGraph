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
	if head := sh.headStamp(id); tx.conflicts(head) {
		sh.mu.Unlock()
		_ = tx.conflictErr(mvcc.StoreNodeExistence, head)
		return false
	}
	// Inside the lock, so the record this write lands on is the one the check
	// just cleared. deltaStamp allocates the transaction's commit record on
	// first use; it takes no lock of its own and cannot reach back here.
	info, ts := g.deltaStamp(tx.record())
	seq := g.lifeSeq.Add(1)
	st := lifeStamp{info: info, ts: ts, seq: seq}
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

	if hasBorn && !bornVisible && !hasDied {
		// Created after this reader started, or by a transaction that has not
		// committed, and never removed. It does not exist yet.
		return false
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
