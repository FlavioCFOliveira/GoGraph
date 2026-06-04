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

// removedEdgePreimage captures the state of an edge the statement is about to
// remove, so RemoveEdge can be inverted: the edge is re-added with its original
// weight and stable handle and its per-pair labels, properties, and
// CREATE-multiplicity counter — plus the removed instance's per-HANDLE labels
// and properties — are restored. This covers both the realistic DELETE-then-fail
// interleaving (e.g. `MATCH (n) SET n.x=1 DELETE n` failing on a later row, or a
// standalone `DELETE r` followed by a failing clause) and the exotic multigraph
// removal-then-fail interleaving (#1327): removing ONE of several parallel edges
// between the same endpoints and then failing a later row.
//
// Per-HANDLE vs per-pair. [Graph.RemoveEdge] removes only the FIRST adjacency
// slot for the pair; while a parallel sibling survives it leaves the per-pair
// union, the per-handle store, and the per-CREATE-index store untouched. The
// only thing it loses for the removed instance is the stable handle stamped on
// its adjacency slot. The undo's re-add therefore restores that handle (so the
// handle-keyed read path resolves the re-added instance to its OWN type and
// properties rather than mis-mapping to a surviving sibling) and re-asserts the
// per-handle labels/properties so the inverse is self-sufficient — it does not
// rely on the handle store having survived the removal.
//
// The per-CREATE-INDEX store ([Graph.SetEdgeLabelAt] et al.) is the simple-graph
// fallback and is keyed by CREATE order, not by adjacency slot; no removal path
// (DELETE or this undo's re-add) ever mutates it, so it survives a
// removal-then-fail rollback unchanged and needs no capture. In multigraph mode
// — where this exotic interleaving lives — the per-handle store is the
// authoritative per-instance surface (see graph/lpg/edge_handle.go).
type removedEdgePreimage struct {
	src, dst    string
	weight      float64
	hadEdge     bool
	labels      []string
	props       map[string]lpg.PropertyValue
	createCount int64
	// handle is the stable handle of the FIRST src→dst adjacency slot — the
	// one RemoveEdge will remove — or 0 when the edge carries no handle
	// (simple-graph or pre-Stage-2 storage). On undo the edge is re-added with
	// this handle so a removed parallel instance keeps its identity.
	handle uint64
	// handleLabels / handleProps are the removed instance's per-handle label
	// and property pre-images, captured under handle. Restored on undo so the
	// re-added instance resolves to its own metadata. Empty when handle is 0.
	handleLabels []string
	handleProps  map[string]lpg.PropertyValue
}

// captureRemovedEdge snapshots the state of edge (src, dst) before a
// RemoveEdge. It is called only when undo is active, on the cold DELETE path, so
// its O(out-degree) weight/handle scan and metadata copies never touch the read
// hot path. In addition to the per-pair union it records the FIRST src→dst
// slot's stable handle and that handle's per-instance label/property pre-images,
// so a removed parallel edge can be re-added with its original identity (#1327).
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
	// Capture the per-handle identity of the exact slot RemoveEdge will drop
	// (the first src→dst slot). When the edge carries a handle, snapshot that
	// handle's per-instance labels and properties too, so the inverse re-adds
	// the instance with its own metadata even if a future removal path clears
	// the handle store.
	if h, ok := m.g.FirstEdgeHandle(src, dst); ok {
		pre.handle = h
		pre.handleLabels = m.g.EdgeLabelsByHandle(src, dst, h)
		pre.handleProps = m.g.EdgePropertiesByHandle(src, dst, h)
	}
	return pre
}

// recordRemoveEdge records the inverse of removing edge (src, dst) from the
// captured pre-image. wasPresent reports that the adapter observed the edge and
// incremented the edges-removed counter; a no-op removal records nothing. The
// inverse re-adds the edge with its original weight AND stable handle,
// decrements the edges-removed counter, then restores the per-pair labels,
// properties, and CREATE-multiplicity counter, and finally the removed
// instance's per-handle labels and properties (each via the same setters the
// forward path used, so the restored state is byte-for-byte the pre-removal
// state for both the per-pair union and the per-handle instance surface).
func (m mutationUndo) recordRemoveEdge(pre *removedEdgePreimage, wasPresent bool) {
	if !m.active() || !wasPresent || !pre.hadEdge {
		return
	}
	m.undo.record(func() {
		// Re-add the edge first so SetEdgeLabel/SetEdgeProperty (which require
		// the edge to exist) reattach successfully. Re-add WITH the captured
		// handle ([Graph.AddEdgeHIfAbsent]) so a removed parallel instance keeps
		// its stable identity — the adjacency slot would otherwise come back
		// with the 0 sentinel and the handle-keyed read path would mis-map it
		// to a surviving sibling. A 0 handle falls back to a plain AddEdge.
		_, _ = m.g.AddEdgeHIfAbsent(pre.src, pre.dst, pre.weight, pre.handle)
		m.g.DecrEdgesRemoved()
		for _, lbl := range pre.labels {
			m.g.SetEdgeLabel(pre.src, pre.dst, lbl)
		}
		for k, v := range pre.props {
			_ = m.g.SetEdgeProperty(pre.src, pre.dst, k, v)
		}
		// Re-assert the removed instance's per-handle labels and properties.
		// Idempotent when the handle store survived the removal (the common
		// case: RemoveEdge keeps it while a sibling survives); authoritative
		// when it did not. No-op when handle is 0.
		for _, lbl := range pre.handleLabels {
			m.g.SetEdgeLabelByHandle(pre.src, pre.dst, pre.handle, lbl)
		}
		for k, v := range pre.handleProps {
			m.g.SetEdgePropertyByHandle(pre.src, pre.dst, pre.handle, k, v)
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
