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
	bag := g.labelBagFor(id, s)
	if bag.len() == 0 {
		return nil
	}
	out := make([]string, 0, bag.len())
	bag.forEach(func(lid LabelID) {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
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
	bag := g.labelBagFor(id, s)
	return bag.has(lid)
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

// labelBagFor resolves a node's label bag either as of s or as it currently
// stands, taking the shard read lock in both cases.
//
// The returned bag may alias the stored one, so callers must only read it —
// which every caller here does, and which is the same contract the plain
// accessors already work under.
func (g *Graph[N, W]) labelBagFor(id graph.NodeID, s *Snapshot) labelBag {
	startTS, txID, walk := snapshotTimes(s)
	if !walk {
		return g.labelBagPlain(id)
	}
	return g.labelBagAsOf(id, startTS, txID)
}

// ── node properties ──────────────────────────────────────────────────────────

// NodePropertiesByIDAsOf returns a map of every property id carried at s, or
// nil when it carried none.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodePropertiesByIDAsOf(id graph.NodeID, s *Snapshot) map[string]PropertyValue {
	bag := g.propBagFor(id, s)
	if bag.len() == 0 {
		return nil
	}
	out := make(map[string]PropertyValue, bag.len())
	bag.forEach(func(pid PropertyKeyID, v PropertyValue) {
		if name, ok := g.pkeys.Resolve(pid); ok {
			out[name] = v
		}
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
	bag := g.propBagFor(id, s)
	return bag.get(pid)
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

// propBagFor resolves a node's property bag either as of s or as it currently
// stands. See [Graph.labelBagFor] for the aliasing contract.
func (g *Graph[N, W]) propBagFor(id graph.NodeID, s *Snapshot) propBag {
	startTS, txID, walk := snapshotTimes(s)
	if !walk {
		return g.propBagPlain(id)
	}
	return g.propBagAsOf(id, startTS, txID)
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
	return g.adj.EntryViewAsOf(id, startTS, txID)
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
