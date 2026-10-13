package lpg

// mvcc_abort_release.go — an aborted transaction gives back the nodes it created
// under synthetic keys (rmp #3119).
//
// # The defect
//
// A node a transaction creates is interned in the graph's [graph.Mapper] the
// moment the write is made: the id has to exist for the transaction's own edges,
// labels, properties and versions. When the transaction aborted, the withdrawal
// ([Graph.withdrawAbortedNow]) left the id tombstoned and marked unborn, and the
// key stayed bound for the life of the graph. Measured at v0.17.0: a
// `MATCH (a), (b), (c) CREATE ()` cut by a one-second deadline left 1.57 million
// keys interned and 161 MB live after its rollback, against 2.3 MB before it.
//
// # What is released, and why only that
//
// When a write transaction ABORTS as a whole ([Graph.endWrite]'s abort branch:
// an autocommit statement that failed or was cut, an explicit transaction rolled
// back or doomed), each node it created whose key is a synthetic key
// ([synthkey.Is]) and that never came into existence is released. The Cypher
// engine mints those keys for every node a CREATE or MERGE makes; such a key is
// unique by construction and known only to the transaction that minted it, so no
// other transaction can hold it. Keys of any other form keep the state the abort
// has always left: bound, tombstoned and unborn, so a later write of the same
// key revives the same id.
//
// A transaction that COMMITS releases nothing, including the creations a failed
// statement inside it withdrew: its commit marker names every id it created in
// its id annex, and recovery rebuilds those ids as committed-dead nodes
// ([Graph.SettleNeverBorn]), so the running graph must hold them too.
//
// # The checks, and the per-node lock they run under
//
// An id is released only if the transaction created it ([writeCtx.created]),
// its key is still bound to it, its label and property bags are empty — the
// withdrawal restored them to their state before the creation, which for a
// first creation is empty — and, under its node-life shard's lock, it carries no
// life record and no existence claim from any transaction and is tombstoned and
// in the unborn set: the state the withdrawal leaves an aborted first creation
// in.
//
// Every node write names the node by its KEY ([WriteView] has no id-addressed
// node write), so a writer reaches the id only by interning the key, and only
// the aborted transaction knew it. The life-shard check is the second line:
// every write that brings a node back records under that lock before it takes
// effect — a revival a birth, a removal a death, an existence claim a claim — so
// an id any transaction has recorded on is never released. The check is made
// twice, before the key is unbound and again after, and an id that fails the
// second check has its binding put back.
//
// # The order of the steps, and why
//
//  1. The key is unbound and the id becomes a hole ([graph.Mapper.Unplace]),
//     under the mapper shard's lock. From here no lookup, intern or walk reaches
//     the id: a capture walking the mapper sees either the bound, unborn id — a
//     hole in its image — or no id at all.
//  2. The unborn mark is dropped under the life-shard lock.
//  3. The tombstone is cleared, for a batch of ids at a time with one copy of
//     the bitmap per batch rather than one per id.
//
// Dropping the unborn mark BEFORE unbinding the key would leave a moment in
// which the id is bound, tombstoned and not unborn — a committed-dead node to a
// capture, which would then write it into a snapshot image as one.
//
// A released id is then exactly what recovery makes of an aborted transaction's
// ids: a hole below the shard's high-water mark, which is never lowered, so no
// id is issued twice and every WAL id reservation stays valid. Unplace frees the
// reverse-table page a release empties and shrinks the forward map once enough
// keys are gone ([graph.Mapper.Unplace]).
//
// The node counts read the mapper and the tombstone bitmap at different moments
// ([Graph.LiveOrderStored]), so a count taken while a release runs can be off by
// up to one batch of ids. That is the contract those counts already state for
// any concurrent creation or removal.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/internal/synthkey"
)

// releaseBatchMin is the smallest batch of released ids whose tombstones are
// cleared with one copy of the bitmap; see [Graph.releaseNeverBorn].
const releaseBatchMin = 1024

// releaseNeverBorn releases the never-born nodes with synthetic keys that the
// aborted transaction w created; see the file comment. It runs from
// [Graph.endWrite] after the abort withdrawal and before w is recycled, and
// consumes w.created.
func (g *Graph[N, W]) releaseNeverBorn(w *writeCtx) {
	w.createdMu.Lock()
	ids := w.created
	w.created = w.created[:0]
	w.createdMu.Unlock()
	if len(ids) == 0 {
		return
	}
	var zero N
	if _, ok := any(zero).(string); !ok {
		return // synthetic keys are strings; no other key type has any
	}
	m := g.adj.Mapper()
	// One bitmap copy per batch: at least releaseBatchMin ids, and never more
	// than about 256 copies however large the transaction was.
	batch := max(releaseBatchMin, len(ids)/256)
	done := ids[:0] // released ids awaiting their tombstone clear, in ids' prefix
	unborn := 0
	for _, id := range ids {
		k, ok := m.Resolve(id)
		if !ok {
			continue
		}
		if s, _ := any(k).(string); !synthkey.Is(s) {
			continue
		}
		if !g.nodeBagsEmpty(id) || !g.neverBornHeld(id, false) {
			continue
		}
		if !m.Unplace(k, id) {
			continue
		}
		if !g.neverBornHeld(id, true) {
			// A record appeared between the two checks, which needs the key
			// that only the aborted transaction had; put the binding back.
			_, _ = m.PlaceUnborn(k, id)
			continue
		}
		unborn++
		done = append(done, id) // done never overtakes the read position
		if len(done) >= batch {
			g.untombstoneReleased(done)
			done = done[:0]
		}
	}
	if len(done) > 0 {
		g.untombstoneReleased(done)
	}
	if unborn > 0 {
		g.shrinkUnborn(unborn)
		g.BumpTopoGeneration()
	}
}

// nodeBagsEmpty reports whether id carries no label and no property, stored or
// versioned.
func (g *Graph[N, W]) nodeBagsEmpty(id graph.NodeID) bool {
	ls := g.nodeLabelShardFor(id)
	ls.mu.RLock()
	bag, hasBag := ls.m[id]
	_, hasDelta := ls.d[id]
	ls.mu.RUnlock()
	if hasDelta || (hasBag && bag.len() != 0) {
		return false
	}
	ps := g.nodePropShardFor(id)
	ps.mu.RLock()
	pbag, hasPBag := ps.m[id]
	_, hasPDelta := ps.d[id]
	ps.mu.RUnlock()
	return !hasPDelta && (!hasPBag || pbag.len() == 0)
}

// neverBornHeld reports, under id's node-life shard lock, whether id is a
// never-born node no transaction holds: no birth, death or claim record, and
// tombstoned and unborn. With drop it also removes the unborn mark when it
// reports true. The lock order — life shard, then unbornMu — is the one
// [lifeWithdrawal.withdrawLocked] takes.
func (g *Graph[N, W]) neverBornHeld(id graph.NodeID, drop bool) bool {
	sh := g.nodeLifeShardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, ok := sh.born[id]; ok {
		return false
	}
	if _, ok := sh.died[id]; ok {
		return false
	}
	if _, ok := sh.claim[id]; ok {
		return false
	}
	if !g.IsTombstonedStored(id) {
		return false
	}
	g.unbornMu.Lock()
	defer g.unbornMu.Unlock()
	if _, ok := g.unborn[id]; !ok {
		return false
	}
	if drop {
		delete(g.unborn, id)
		g.unbornN.Add(-1)
	}
	return true
}

// untombstoneReleased clears the tombstone of every id in ids with one copy of
// the bitmap. No label bitmap is touched: a released node carries no label
// ([Graph.nodeBagsEmpty]), so there is nothing for the churn gate to pin
// either.
func (g *Graph[N, W]) untombstoneReleased(ids []graph.NodeID) {
	g.tombstoneMu.Lock()
	defer g.tombstoneMu.Unlock()
	cur := g.tombstones.Load()
	if cur == nil {
		return
	}
	next := cur.Clone()
	removed := 0
	for _, id := range ids {
		if next.CheckedRemove(uint64(id)) {
			removed++
		}
	}
	if removed == 0 {
		return
	}
	// Bitmap first, counter second on the way DOWN — see [Graph.removeNodeInfo]
	// (rmp #2687).
	g.tombstones.Store(next)
	g.tombstoneActive.Add(-int64(removed))
}

// shrinkUnborn gives back the unborn set's memory after a release dropped
// dropped of its entries: a Go map never shrinks on delete, so it is rebuilt
// when at least half of its entries went, and dropped when it is empty
// (rmp #3120).
func (g *Graph[N, W]) shrinkUnborn(dropped int) {
	g.unbornMu.Lock()
	defer g.unbornMu.Unlock()
	switch n := len(g.unborn); {
	case n == 0:
		g.unborn = nil
	case dropped >= n:
		rebuilt := make(map[graph.NodeID]struct{}, n)
		for id := range g.unborn {
			rebuilt[id] = struct{}{}
		}
		g.unborn = rebuilt
	}
}
