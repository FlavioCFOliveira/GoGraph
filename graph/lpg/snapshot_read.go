package lpg

// snapshot_read.go — the versioned read surface (rmp #2289, MVCC P4b).
//
// Every accessor here takes a *Snapshot, and nil means "the current stored
// value with no version walk". The plain accessors elsewhere in the package
// delegate here with nil, so there is ONE implementation per read rather than
// two that can drift apart.
//
// # Why nil is the writer's snapshot
//
// A writer inside the visibility barrier applies its changes EAGERLY and must
// see them — read-your-own-writes within a statement is what MERGE, DELETE and
// SET all depend on. Its own versions are stamped with an in-flight transaction
// id, so a versioned read would step back over them and hand the writer the
// state it started from. Reading the current value is not a shortcut for a
// writer; it is the only correct answer.
//
// # What is still NOT snapshot-consistent, and where that is resolved
//
// These accessors answer what an OBJECT contains. They do not answer which
// objects a scan should consider: the label bitmap index, the property indexes,
// the tombstone set, the node mapper and the count store are not versioned, so
// a scan driven by one of them can see a candidate set from a later instant.
// That is the candidate-set discipline of P4c (rmp #2290), and until it lands
// the barrier is still what closes the gap.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// ── node labels ──────────────────────────────────────────────────────────────

// NodeLabelsByIDAsOf returns the names of every label id carried at s, in
// unspecified order, or nil when it carried none.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodeLabelsByIDAsOf(id graph.NodeID, s *Snapshot) []string {
	var out []string
	g.withLabelBag(id, s, func(bag labelBag) {
		if bag.len() == 0 {
			return
		}
		out = make([]string, 0, bag.len())
		bag.forEach(func(lid LabelID) {
			if name, ok := g.reg.Resolve(lid); ok {
				out = append(out, name)
			}
		})
	})
	return out
}

// NodeLabelsAsOf is [Graph.NodeLabelsByIDAsOf] keyed by the external node key.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodeLabelsAsOf(n N, s *Snapshot) []string {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return nil
	}
	return g.NodeLabelsByIDAsOf(id, s)
}

// HasNodeLabelByIDAsOf reports whether id carried the named label at s.
//
// Safe for concurrent use.
func (g *Graph[N, W]) HasNodeLabelByIDAsOf(id graph.NodeID, name string, s *Snapshot) bool {
	lid, ok := g.reg.Lookup(name)
	if !ok {
		return false
	}
	// Written out for the same reason as [Graph.NodePropertyByIDAsOf]: this is a
	// per-ROW predicate on every labelled scan. The membership test runs INSIDE
	// the lock, because the bag aliases the stored backing array; see
	// [Graph.withLabelBag].
	// The gate is the shard's own side map, read UNDER the lock. A graph-level
	// counter sampled BEFORE the lock is fast and wrong: a writer can create
	// the first version in between, and the reader then takes the raw current
	// value here and the pre-write value at the next node. See
	// [Graph.propBagAsOf].
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	bag := sh.m[id]
	if s != nil && sh.d != nil {
		// Through the SNAPSHOT (rmp #2420/#2378). This predicate is what
		// ReadView.HasNodeLabel resolves to, and it used to pass only the
		// timestamps — so it read each record's MUTABLE commit stamp live and
		// never consulted [Snapshot.visible]. The verdict memo was wired into
		// [Graph.withLabelBag] and [Graph.EntryViewAsOf] but not here, which is
		// why TestIsolation_CrossSubstructure_EdgeImpliesLabels kept tearing after
		// #2378 was closed: its two label reads go through THIS function, so they
		// never saw the pinned verdict at all.
		bag = g.labelBagAsOfLockedSnap(sh, id, s, s.startTS, s.txID)
	}
	present := bag.has(lid)
	sh.mu.RUnlock()
	return present
}

// HasNodeLabelAsOf is [Graph.HasNodeLabelByIDAsOf] keyed by the external node
// key.
//
// Safe for concurrent use.
func (g *Graph[N, W]) HasNodeLabelAsOf(n N, name string, s *Snapshot) bool {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return false
	}
	return g.HasNodeLabelByIDAsOf(id, name, s)
}

// withLabelBag resolves a node's label bag as of s and hands it to fn WHILE THE
// SHARD READ LOCK IS HELD.
//
// The lock is not an implementation detail here. The bag is returned by value
// but its small tier is a slice header over the STORED backing array, so
// consuming it after the lock is released races a concurrent writer's in-place
// mutation — `-race` reported exactly that, a write in labelBag.del against a
// read in labelBag.has. Every accessor that CONSUMES a bag rather than copying
// a scalar out of it goes through this.
//
// fn must not touch this shard again: sync.RWMutex is not re-entrant, and a
// writer arriving in between deadlocks the pair. Reading a DIFFERENT structure
// (the label registry, the existence records) is fine and is what the callers
// do.
func (g *Graph[N, W]) withLabelBag(id graph.NodeID, s *Snapshot, fn func(labelBag)) {
	sh := g.nodeLabelShardFor(id)
	startTS, txID, walk := snapshotTimes(s)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if !walk || sh.d == nil {
		fn(sh.m[id])
		return
	}
	fn(g.labelBagAsOfLockedSnap(sh, id, s, startTS, txID))
}

// labelBagTest applies pred to a node's label bag as of s, under the shard
// lock. pred must be pure; see [Graph.withLabelBag].
func (g *Graph[N, W]) labelBagTest(id graph.NodeID, s *Snapshot, pred func(labelBag) bool) bool {
	out := false
	g.withLabelBag(id, s, func(b labelBag) { out = pred(b) })
	return out
}

// ── node properties ──────────────────────────────────────────────────────────

// NodePropertiesByIDAsOf returns a map of every property id carried at s, or
// nil when it carried none.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodePropertiesByIDAsOf(id graph.NodeID, s *Snapshot) map[string]PropertyValue {
	var out map[string]PropertyValue
	g.withPropBag(id, s, func(bag propBag) {
		if bag.len() == 0 {
			return
		}
		out = make(map[string]PropertyValue, bag.len())
		bag.forEach(func(pid PropertyKeyID, v PropertyValue) {
			if name, ok := g.pkeys.Resolve(pid); ok {
				out[name] = v
			}
		})
	})
	return out
}

// NodePropertiesAsOf is [Graph.NodePropertiesByIDAsOf] keyed by the external
// node key.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodePropertiesAsOf(n N, s *Snapshot) map[string]PropertyValue {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return nil
	}
	return g.NodePropertiesByIDAsOf(id, s)
}

// NodePropertyByIDAsOf returns the value id carried under key at s.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodePropertyByIDAsOf(id graph.NodeID, key string, s *Snapshot) (PropertyValue, bool) {
	pid, ok := g.pkeys.Lookup(key)
	if !ok {
		return PropertyValue{}, false
	}
	// The fast path is written out HERE rather than delegated through
	// propBagAsOf, and that is measured: this is the per-ROW accessor of a
	// scalar projection, so two extra call frames returning a 32-byte bag cost
	// 21 ns per row — 23 % of BenchmarkEngReadProjectLargeSerial over 60 000
	// rows. Both the GATE and the lookup run inside the lock: the gate because
	// sampling it before would let a write land in between and tear the read
	// (see [Graph.propBagAsOf]), the lookup because the bag aliases the stored
	// backing array (see [Graph.withLabelBag]).
	sh := g.nodePropShardFor(id)
	sh.mu.RLock()
	bag := sh.m[id]
	if s != nil && sh.d != nil {
		// Through the SNAPSHOT (rmp #2420), not the bare timestamps: the record's
		// TS is mutable and flips at commit, so two reads of one snapshot that
		// straddle that flip classify the same transaction differently and observe
		// a partial one. See [Graph.propBagAsOfLockedSnap].
		bag = g.propBagAsOfLockedSnap(sh, id, s, s.startTS, s.txID)
	}
	v, ok2 := bag.get(pid)
	sh.mu.RUnlock()
	return v, ok2
}

// GetNodePropertyAsOf is [Graph.NodePropertyByIDAsOf] keyed by the external
// node key.
//
// Safe for concurrent use.
func (g *Graph[N, W]) GetNodePropertyAsOf(n N, key string, s *Snapshot) (PropertyValue, bool) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return PropertyValue{}, false
	}
	return g.NodePropertyByIDAsOf(id, key, s)
}

// withPropBag is [Graph.withLabelBag] for the property bag, under the same
// aliasing contract and the same re-entrancy restriction on fn.
func (g *Graph[N, W]) withPropBag(id graph.NodeID, s *Snapshot, fn func(propBag)) {
	sh := g.nodePropShardFor(id)
	startTS, txID, walk := snapshotTimes(s)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if !walk || sh.d == nil {
		fn(sh.m[id])
		return
	}
	// Through the SNAPSHOT (rmp #2420); see [Graph.propBagAsOfLockedSnap]. This is
	// the accessor behind NodeProperties/NodePropertiesByIDAsOf, so a projection
	// reading a whole bag pins its verdicts exactly as a single-key read does.
	fn(g.propBagAsOfLockedSnap(sh, id, s, startTS, txID))
}

// ── adjacency ────────────────────────────────────────────────────────────────

// EntryViewAsOf returns every column of id's adjacency entry as it stood at s,
// resolved from ONE entry so the columns are mutually consistent.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EntryViewAsOf(id graph.NodeID, s *Snapshot) adjlist.EntryView[W] {
	startTS, txID, walk := snapshotTimes(s)
	if !walk {
		return g.adj.LoadEntryView(id)
	}
	return g.adj.EntryViewAsOfVisible(id, func(info *commitInfo, ts uint64) bool {
		return s.visible(info, ts, startTS, txID)
	})
}

// HasEdgeByIDAsOf reports whether a directed edge srcID→dstID existed at s.
//
// Safe for concurrent use.
func (g *Graph[N, W]) HasEdgeByIDAsOf(srcID, dstID graph.NodeID, s *Snapshot) bool {
	startTS, txID, walk := snapshotTimes(s)
	if !walk {
		// The current value, read from the same one-load view the versioned
		// path uses, so the two answer the same question.
		for _, n := range g.adj.LoadEntryView(srcID).Neighbours {
			if n == dstID {
				return true
			}
		}
		return false
	}
	return g.adj.HasEdgeAsOf(srcID, dstID, startTS, txID)
}

// HasEdgeAsOf is [Graph.HasEdgeByIDAsOf] keyed by the external node keys.
//
// Safe for concurrent use.
func (g *Graph[N, W]) HasEdgeAsOf(src, dst N, s *Snapshot) bool {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return false
	}
	return g.HasEdgeByIDAsOf(srcID, dstID, s)
}
