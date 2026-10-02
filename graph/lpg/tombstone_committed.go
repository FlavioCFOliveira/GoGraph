package lpg

// tombstone_committed.go — the node-existence family at the newest COMMITTED
// state (rmp #2965, round 6).
//
// The tombstone bitmap and its counter are maintained eagerly by every node
// removal and revival, committed or not: a durable commit that deleted a node and
// was still waiting for its fsync showed the node as tombstoned, and a failed
// fsync then brought it back. The public readers below resolve existence through
// the node-life records instead ([Graph.NodeExistsAsOf]) at the committed-only
// read position every other present-state accessor uses ([Graph.latestCommitted]).
//
// The bitmap readers keep their old behaviour under the Stored names
// ([Graph.IsTombstonedStored] and the rest): the versioned readers fall back to
// them for a node with no life record, and the engine and the writers, which must
// see their own uncommitted removals, call them directly.
//
// # Cost
//
// A graph with no life record — nothing created or removed since the last
// reclamation — answers from the bitmap after one atomic load, exactly as before.
// Otherwise IsTombstoned takes the node's life-shard read lock, and the two
// counts visit every retained life record once, which reclamation keeps
// proportional to recent node churn.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// IsTombstoned reports whether the node id does not exist at the newest
// committed state: it was removed by a committed transaction, or it is interned
// only by a creation that has not committed. A removal no transaction has
// published is stepped back over, and so is a revival.
//
// The stored bitmap, which reflects every uncommitted removal and revival, is
// [Graph.IsTombstonedStored]. A transaction asks about its own writes through
// [ReadView.IsTombstoned] on its own view ([Graph.WriterViewOf]).
//
// Safe for concurrent use.
func (g *Graph[N, W]) IsTombstoned(id graph.NodeID) bool {
	if !g.mvccArmed || g.nodeLifeActive.Load() == 0 {
		return g.IsTombstonedStored(id)
	}
	var cs Snapshot // the read position: newest committed
	return !g.NodeExistsAsOf(id, g.latestCommitted(&cs))
}

// TombstonedIDs returns, in ascending order, every interned node id that does
// not exist at the newest committed state, as [Graph.TombstonedIDsAsOf] does
// for a snapshot. The stored set is [Graph.TombstonedIDsStored].
//
// Safe for concurrent use.
func (g *Graph[N, W]) TombstonedIDs() []graph.NodeID {
	if !g.mvccArmed || g.nodeLifeActive.Load() == 0 {
		return g.TombstonedIDsStored()
	}
	var cs Snapshot // the read position: newest committed
	return g.TombstonedIDsAsOf(g.latestCommitted(&cs))
}

// TombstoneCount returns how many interned node ids do not exist at the newest
// committed state: len([Graph.TombstonedIDs]) without materialising the set.
// An id interned only by an uncommitted creation is counted by neither this nor
// [Graph.LiveOrder], as [Graph.TombstonedIDsAsOf] does not list it. The stored
// counter is [Graph.TombstoneCountStored].
//
// Safe for concurrent use.
func (g *Graph[N, W]) TombstoneCount() int {
	dead, _ := g.committedLifeCounts()
	return dead
}

// LiveOrder returns how many interned nodes exist at the newest committed
// state. The stored count is [Graph.LiveOrderStored].
//
// Safe for concurrent use.
func (g *Graph[N, W]) LiveOrder() uint64 {
	_, live := g.committedLifeCounts()
	return live
}

// LiveNodeFilter returns the liveness predicate at the newest committed state,
// or nil when every interned node is live — so a caller can skip the per-node
// call. The stored predicate is [Graph.LiveNodeFilterStored].
//
// Safe for concurrent use.
func (g *Graph[N, W]) LiveNodeFilter() func(graph.NodeID) bool {
	if !g.mvccArmed || g.nodeLifeActive.Load() == 0 {
		return g.LiveNodeFilterStored()
	}
	return func(id graph.NodeID) bool { return !g.IsTombstoned(id) }
}

// committedLifeCounts returns the dead and the live interned-node counts at the
// newest committed state.
//
// Only a node with a life record can differ between the stored state and the
// committed one, so the stored counts are corrected over the retained records
// alone rather than over every interned node.
func (g *Graph[N, W]) committedLifeCounts() (dead int, live uint64) {
	dead, live = g.TombstoneCountStored(), g.LiveOrderStored()
	if !g.mvccArmed || g.nodeLifeActive.Load() == 0 {
		return dead, live
	}
	var cs Snapshot // the read position: newest committed
	s := g.latestCommitted(&cs)
	var ids []graph.NodeID
	for i := range g.nodeLifeShards {
		sh := &g.nodeLifeShards[i]
		sh.mu.RLock()
		for id := range sh.born {
			ids = append(ids, id)
		}
		for id := range sh.died {
			if _, both := sh.born[id]; !both {
				ids = append(ids, id)
			}
		}
		sh.mu.RUnlock()
	}
	dDead, dLive := 0, int64(0)
	for _, id := range ids {
		storedDead := g.IsTombstonedStored(id)
		interned := g.NodeInternedAsOf(id, s)
		exists := interned && g.NodeExistsAsOf(id, s)
		if storedDead {
			dDead--
		} else {
			dLive--
		}
		switch {
		case exists:
			dLive++
		case interned:
			dDead++
		}
	}
	dead += dDead
	if dLive < 0 && uint64(-dLive) > live {
		return dead, 0
	}
	return dead, uint64(int64(live) + dLive)
}

// nodeExistsAt reports whether id exists at snap, or in the stored state for a
// nil snap: the per-neighbour liveness test of the degree walkers (rmp #2969).
func (g *Graph[N, W]) nodeExistsAt(id graph.NodeID, snap *Snapshot) bool {
	if snap == nil {
		return !g.IsTombstonedStored(id)
	}
	return g.NodeExistsAsOf(id, snap)
}
