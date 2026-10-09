package lpg

import (
	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// PlaceNodeID binds key n to the exact node id a durable commit marker names in
// its id annex, before the transaction's ops are replayed (WAL v2 step 3,
// docs/design-wal-v2.md §1.3). It is for RECOVERY ONLY: it runs on a graph no
// other goroutine is using, and it records no birth.
//
// It reports whether it established a new binding. A key already bound to that
// id is a no-op; any contradiction is an error wrapping [graph.ErrNodeIDMismatch].
// A newly bound node exists as soon as it is placed; replaying the ops that
// created it then finds it bound and alive. If none of the transaction's ops
// names it — its creation was withdrawn by a statement rollback — recovery
// settles it with [Graph.SettleNeverBorn].
func (g *Graph[N, W]) PlaceNodeID(n N, id graph.NodeID) (bool, error) {
	return g.adj.Mapper().PlaceUnborn(n, id)
}

// SettleNeverBorn marks a node placed by [Graph.PlaceNodeID] whose transaction
// named it in its id annex but replayed no op naming it: its creation was
// withdrawn inside the transaction by a statement rollback, which removes the
// node within the same transaction. The live graph therefore holds it as a
// committed-dead node — tombstoned and NOT unborn — and so does this. For
// RECOVERY ONLY.
func (g *Graph[N, W]) SettleNeverBorn(id graph.NodeID) {
	g.tombstoneAborted(id)
}

// UnplaceNodeID withdraws a binding [Graph.PlaceNodeID] made for a transaction
// recovery then discards, before any op of that transaction touched the key. For
// RECOVERY ONLY.
func (g *Graph[N, W]) UnplaceNodeID(n N, id graph.NodeID) bool {
	return g.adj.Mapper().Unplace(n, id)
}

// ReserveNodeID returns the id key n is bound to, interning it as a NEVER-BORN
// node when it is not interned yet: tombstoned and unborn, so no reader sees it
// and a later write that names the key creates it (WAL v2 step 3). The marks are
// set inside the mapper's interning critical section, before the id is reachable
// by any walk or lookup, so no reader ever observes the key as an existing node.
//
// It exists for a commit that writes ops to the WAL without applying them to this
// graph ([txn.Tx.CommitWALOnly] with nothing attached): the commit marker must
// still name an id for every key, and the id must be one this graph will never
// hand to another key.
//
// Safe for concurrent use.
func (g *Graph[N, W]) ReserveNodeID(n N) graph.NodeID {
	id, _ := g.adj.Mapper().InternNewHook(n, func(nid graph.NodeID) {
		g.markUnborn(nid)
		g.tombstoneMu.Lock()
		cur := g.tombstones.Load()
		next := roaring64.New()
		if cur != nil {
			next = cur.Clone()
		}
		next.Add(uint64(nid))
		// Counter first, bitmap second — see [Graph.removeNodeInfo] (rmp #2687).
		g.tombstoneActive.Add(1)
		g.tombstones.Store(next)
		g.tombstoneMu.Unlock()
		g.BumpTopoGeneration()
	})
	return id
}
