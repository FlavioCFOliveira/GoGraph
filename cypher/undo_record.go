package cypher

// undo_record.go — inverse-operation builders for the write-query undo log.
//
// Each helper here is called by BOTH mutator adapters (walMutatorAdapter and
// lpgMutatorAdapter) immediately after a mutation has been applied to the live
// in-memory graph. It captures whatever pre-image the inverse needs and records
// a closure on the undo log that, when replayed, returns the in-memory graph to
// exactly its pre-mutation state. The closures touch ONLY the *lpg.Graph: the
// WAL transaction and the secondary-index buffer roll back through their own
// mechanisms, so the undo log is concerned solely with the in-memory
// divergence that #1282 closes.
//
// Centralising the inverse logic here (rather than inlining it in each adapter)
// keeps the two adapters' undo behaviour identical by construction and gives
// the upcoming Bolt multi-statement transaction work one place to extend.
//
// mutationUndo is a thin (graph, log) pair so the helpers read as methods. It is
// embedded by value in each adapter; a nil undo makes every record* a no-op
// (read-only adapters, or the not-yet-wired in-memory path), so the helpers are
// always safe to call.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mutationUndo records the inverse of each in-memory mutation on undo. Both
// adapters embed one; g aliases the adapter's graph and undo is the per-
// statement log (nil ⇒ recording disabled).
type mutationUndo struct {
	g    *lpg.Graph[string, float64]
	undo *undoLog
}

// active reports whether undo recording is enabled. The helpers short-circuit
// on a nil log so a read-only adapter pays nothing.
func (m mutationUndo) active() bool { return m.undo != nil }

// recordAddNode records the inverse of an AddNode that freshly created (or
// revived a tombstoned) node key n. wasNew is the adapter's determination that
// the node did not previously exist as a live node. When wasNew is false the
// node already existed live, so AddNode was a no-op and there is nothing to
// undo. The inverse tombstones the node (the Mapper slot is permanent by
// contract — NodeID stability — so a logical removal is the correct and only
// available inverse) and decrements the nodes-added side-effect counter so the
// per-query +nodes delta the openCypher TCK asserts does not retain the
// rolled-back creation.
func (m mutationUndo) recordAddNode(n string, wasNew bool) {
	if !m.active() || !wasNew {
		return
	}
	m.undo.record(func() {
		m.g.RemoveNode(n)
		m.g.DecrNodesAdded()
	})
}

// recordAddEdge records the inverse of an AddEdge/AddEdgeH between src and dst.
// srcNew/dstNew report whether each endpoint was freshly created by the call
// (so its node-creation is also undone). The inverse removes the edge and
// decrements the edges-added counter; for each freshly created endpoint it also
// tombstones the node and decrements the nodes-added counter. The endpoint
// removals are recorded as part of THIS entry (not via recordAddNode) because
// AddEdge interns endpoints itself without routing through the mutator's
// AddNode.
func (m mutationUndo) recordAddEdge(src, dst string, srcNew, dstNew bool) {
	if !m.active() {
		return
	}
	selfLoop := src == dst
	m.undo.record(func() {
		m.g.RemoveEdge(src, dst)
		m.g.DecrEdgesAdded()
		if srcNew {
			m.g.RemoveNode(src)
			m.g.DecrNodesAdded()
		}
		if dstNew && !selfLoop {
			m.g.RemoveNode(dst)
			m.g.DecrNodesAdded()
		}
	})
}

// recordSetNodeLabel records the inverse of attaching label to n. hadLabel is
// the adapter's pre-call check: when the node ALREADY carried the label the set
// was an idempotent no-op (e.g. MERGE's ON MATCH re-tagging an existing node),
// so nothing is recorded and the undo leaves the pre-existing label intact; only
// a label the statement actually added is detached on undo.
func (m mutationUndo) recordSetNodeLabel(n, label string, hadLabel bool) {
	if !m.active() || hadLabel {
		return
	}
	m.undo.record(func() { m.g.RemoveNodeLabel(n, label) })
}

// recordRemoveNodeLabel records the inverse of detaching label from n. hadLabel
// is the adapter's pre-call check: when the label was present, the inverse
// re-attaches it; when it was absent the removal was a no-op and nothing is
// recorded.
func (m mutationUndo) recordRemoveNodeLabel(n, label string, hadLabel bool) {
	if !m.active() || !hadLabel {
		return
	}
	m.undo.record(func() { _ = m.g.SetNodeLabel(n, label) })
}

// recordRemoveNode records the inverse of tombstoning the live node n: revive
// it and re-increment the nodes-removed counter. wasLive is the adapter's
// pre-call check that n existed and was not already tombstoned; a no-op
// RemoveNode records nothing. Reviving restores visibility; the node's labels,
// properties, and incident edges were stripped by separate mutations the
// executor issued before RemoveNode, each of which recorded its own inverse, so
// the full pre-delete state is reconstructed by the LIFO replay.
func (m mutationUndo) recordRemoveNode(n string, wasLive bool) {
	if !m.active() || !wasLive {
		return
	}
	m.undo.record(func() {
		m.g.Revive(n)
		m.g.DecrNodesRemoved()
	})
}

// recordSetNodeProperty records the inverse of SetNodeProperty(n, key, …). It
// captures the prior value (prev, had) the adapter read BEFORE the write: when
// the property existed, the inverse restores the old value; otherwise it
// deletes the key the statement added.
func (m mutationUndo) recordSetNodeProperty(n, key string, prev lpg.PropertyValue, had bool) {
	if !m.active() {
		return
	}
	m.undo.record(func() {
		if had {
			_ = m.g.SetNodeProperty(n, key, prev)
		} else {
			m.g.DelNodeProperty(n, key)
		}
	})
}

// recordDelNodeProperty records the inverse of deleting node property key. prev
// (had) is the value captured before deletion; when it existed the inverse
// re-sets it, otherwise the delete was a no-op and nothing is recorded.
func (m mutationUndo) recordDelNodeProperty(n, key string, prev lpg.PropertyValue, had bool) {
	if !m.active() || !had {
		return
	}
	m.undo.record(func() { _ = m.g.SetNodeProperty(n, key, prev) })
}

// recordSetEdgeLabel records the inverse of attaching label to edge (src, dst).
// hadLabel is the adapter's pre-call check: an idempotent re-tag of an edge that
// already carried the label (e.g. MERGE's match branch re-asserting the type)
// records nothing, so the undo never strips a label that pre-dated the
// statement; only a freshly added label is detached.
func (m mutationUndo) recordSetEdgeLabel(src, dst, label string, hadLabel bool) {
	if !m.active() || hadLabel {
		return
	}
	m.undo.record(func() { m.g.RemoveEdgeLabel(src, dst, label) })
}

// recordSetEdgeProperty records the inverse of SetEdgeProperty(src, dst, key, …)
// using the prior value captured before the write.
func (m mutationUndo) recordSetEdgeProperty(src, dst, key string, prev lpg.PropertyValue, had bool) {
	if !m.active() {
		return
	}
	m.undo.record(func() {
		if had {
			_ = m.g.SetEdgeProperty(src, dst, key, prev)
		} else {
			m.g.DelEdgeProperty(src, dst, key)
		}
	})
}

// recordDelEdgeProperty records the inverse of deleting edge property key using
// the prior value captured before deletion.
func (m mutationUndo) recordDelEdgeProperty(src, dst, key string, prev lpg.PropertyValue, had bool) {
	if !m.active() || !had {
		return
	}
	m.undo.record(func() { _ = m.g.SetEdgeProperty(src, dst, key, prev) })
}

// recordIncEdgeCreateCount records the inverse of bumping the CREATE-multiplicity
// counter for edge (src, dst): decrement it. The counter is metadata only and
// DecEdgeCreateCount floors at zero, so the inverse is exact for the increment
// this entry pairs with.
func (m mutationUndo) recordIncEdgeCreateCount(src, dst string) {
	if !m.active() {
		return
	}
	m.undo.record(func() { m.g.DecEdgeCreateCount(src, dst) })
}

// recordDecEdgeCreateCount records the inverse of decrementing the CREATE-
// multiplicity counter: increment it. had reports that the counter was above
// zero before the decrement (so a floored no-op records nothing).
func (m mutationUndo) recordDecEdgeCreateCount(src, dst string, had bool) {
	if !m.active() || !had {
		return
	}
	m.undo.record(func() { m.g.IncEdgeCreateCount(src, dst) })
}

// removedEdgePreimage captures the per-pair state of an edge the statement is
// about to remove, so RemoveEdge can be inverted: the edge is re-added with its
// original weight and its per-pair labels, properties, and CREATE-multiplicity
// counter are restored. This covers the realistic DELETE-then-fail interleaving
// (e.g. `MATCH (n) SET n.x=1 DELETE n` failing on a later row, or a standalone
// `DELETE r` followed by a failing clause).
//
// NOTE (#1282 deferred follow-up): the per-HANDLE and per-CREATE-INSTANCE edge
// metadata (SetEdgeLabelByHandle / SetEdgeLabelAt and their property analogues)
// of a removed edge are NOT captured here. Restoring those exotic multigraph
// pre-images on a removal-then-fail interleaving requires snapshotting the
// handle/instance-keyed shards, which the orchestrator is tracking as a
// separate task. recordRemoveEdge below restores the per-pair union (weight,
// labels, properties, create-count), which is what every TCK-covered
// DELETE-then-fail path observes; the design leaves room to add the per-handle
// pre-image capture without reworking the call sites.
type removedEdgePreimage struct {
	src, dst    string
	weight      float64
	hadEdge     bool
	labels      []string
	props       map[string]lpg.PropertyValue
	createCount int64
}

// captureRemovedEdge snapshots the per-pair state of edge (src, dst) before a
// RemoveEdge. It is called only when undo is active, on the cold DELETE path, so
// its O(out-degree) weight scan and metadata copies never touch the read hot
// path.
func (m mutationUndo) captureRemovedEdge(src, dst string) removedEdgePreimage {
	pre := removedEdgePreimage{src: src, dst: dst}
	if !m.g.AdjList().HasEdge(src, dst) {
		return pre
	}
	pre.hadEdge = true
	if w, ok := m.g.EdgeWeight(src, dst); ok {
		pre.weight = w
	}
	pre.labels = m.g.EdgeLabels(src, dst)
	pre.props = m.g.EdgeProperties(src, dst)
	pre.createCount = m.g.EdgeCreateCount(src, dst)
	return pre
}

// recordRemoveEdge records the inverse of removing edge (src, dst) from the
// captured pre-image. wasPresent reports that the adapter observed the edge and
// incremented the edges-removed counter; a no-op removal records nothing. The
// inverse re-adds the edge with its original weight, decrements the edges-
// removed counter, then restores the per-pair labels, properties, and CREATE-
// multiplicity counter (each via the same setters the forward path used, so the
// restored state is byte-for-byte the pre-removal state for everything the
// per-pair union covers).
func (m mutationUndo) recordRemoveEdge(pre *removedEdgePreimage, wasPresent bool) {
	if !m.active() || !wasPresent || !pre.hadEdge {
		return
	}
	m.undo.record(func() {
		// Re-add the edge first so SetEdgeLabel/SetEdgeProperty (which require
		// the edge to exist) reattach successfully.
		_ = m.g.AddEdge(pre.src, pre.dst, pre.weight)
		m.g.DecrEdgesRemoved()
		for _, lbl := range pre.labels {
			m.g.SetEdgeLabel(pre.src, pre.dst, lbl)
		}
		for k, v := range pre.props {
			_ = m.g.SetEdgeProperty(pre.src, pre.dst, k, v)
		}
		// Restore the CREATE-multiplicity counter to its captured value. The
		// re-add above does not touch the counter (only IncEdgeCreateCount does),
		// so set it explicitly by replaying the delta from its current value.
		for c := m.g.EdgeCreateCount(pre.src, pre.dst); c < pre.createCount; c++ {
			m.g.IncEdgeCreateCount(pre.src, pre.dst)
		}
	})
}

// _ pins graph.NodeID into this file's imports so a future inverse that needs a
// NodeID-keyed restore has the type in scope; the helpers above operate on node
// keys, matching the adapter surface.
var _ = graph.NodeID(0)
