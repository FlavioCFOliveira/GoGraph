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
//
// touched is the per-transaction set of node keys whose final-state label or
// property set may bring a NOT NULL constraint into play (#1754). It is nil
// unless the engine has at least one existence constraint active, so the
// touched-node recording is skipped entirely on the common path. See
// constraint_check.go.
type mutationUndo struct {
	// wv is the graph bound to the write transaction the statement is running as
	// (rmp #2320). The inverses below MUTATE, and they run while the failing
	// statement's bracket is still open, so they must be charged to that
	// statement's transaction: an inverse that resolved its commit record through
	// the graph's ambient slot would put the rollback of one statement onto a
	// CONCURRENT writer's commit record, publishing it at that writer's instant.
	// The forward mutation and its inverse must land on ONE record — that is what
	// makes the chain net out to the original value for a reader from before the
	// statement (see graph/lpg/mvcc_txn.go, "they compose").
	//
	// Read-side helpers reach the graph through wv.Graph(); the capture paths below
	// are reads and use it.
	wv      lpg.WriteView[string, float64]
	undo    *undoLog
	touched *touchedNodes
}

// active reports whether undo recording is enabled. The helpers short-circuit
// on a nil log so a read-only adapter pays nothing.
func (m mutationUndo) active() bool { return m.undo != nil }

// touch records node key n in the per-transaction touched-node set when one is
// threaded (i.e. the engine has a NOT NULL constraint active). It is the seam
// the commit-time existence check (constraint_check.go) reads at commit. A nil
// touched set makes it a no-op, so the common no-existence-constraint path pays
// nothing.
//
// # It also STAMPS the node's constraint slot (rmp #2353)
//
// Recording the node is not enough on its own, because the commit-time check runs
// against the committing transaction's OWN view (rmp #2350, correctly) and a write
// skew is invisible from either side of it: T1 removing the property sees no
// constrained label, T2 adding the label still sees the property, and the violation
// exists only in the merged state. Ordinary conflict detection does not close it
// either — conflicts are per SUBSTORE, and those two writes are in different ones.
//
// So this seam does double duty: the set feeds the commit-time check, and
// [lpg.WriteView.NoteConstraintTouch] stamps a per-NODE slot both halves share, so
// the second half to arrive is refused. The touch sites are exactly the writes that
// can INTRODUCE a violation — a node creation, a label gain, a property removal —
// which is why the stamping needed no new bookkeeping.
//
// The conflict is deliberately NOT returned. Three of the four callers are void
// (recordAddNode, recordAddEdge, recordDelNodeProperty), and NoteConstraintTouch
// RECORDS on the transaction as well as reporting, so the doomed transaction is
// refused at commit by the backstop rmp #2354 added — the identical contract every
// other void primitive here already has. Returning it from one caller and not the
// others would be a third way for the same failure to surface.
//
// ZERO COST WHEN UNCONSTRAINED: the nil check short-circuits before the stamp, and
// the set is nil unless the registry holds a NOT NULL constraint. An unconstrained
// schema therefore reaches neither the map nor the stamp shard.
func (m mutationUndo) touch(n string) {
	if m.touched == nil {
		return
	}
	m.touched.touch(n)
	_ = m.wv.NoteConstraintTouch(n)
}

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
	if !wasNew {
		return
	}
	// A freshly created node may carry a constrained label without the required
	// property, so record it for the commit-time existence check (#1754). Touch
	// is independent of undo activity (undo is always active in a write tx, but
	// the touch set is gated on constraints being present, not on undo).
	m.touch(n)
	if !m.active() {
		return
	}
	m.undo.record(func() {
		m.wv.RemoveNode(n)
		m.wv.Graph().DecrNodesAdded()
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
	// An endpoint freshly created by AddEdge may carry a constrained label
	// without the required property, so record each new endpoint for the
	// commit-time existence check (#1754).
	if srcNew {
		m.touch(src)
	}
	if dstNew {
		m.touch(dst)
	}
	if !m.active() {
		return
	}
	selfLoop := src == dst
	m.undo.record(func() {
		m.wv.RemoveEdge(src, dst)
		m.wv.Graph().DecrEdgesAdded()
		if srcNew {
			m.wv.RemoveNode(src)
			m.wv.Graph().DecrNodesAdded()
		}
		if dstNew && !selfLoop {
			m.wv.RemoveNode(dst)
			m.wv.Graph().DecrNodesAdded()
		}
	})
}

// recordSetNodeLabel records the inverse of attaching label to n. hadLabel is
// the adapter's pre-call check: when the node ALREADY carried the label the set
// was an idempotent no-op (e.g. MERGE's ON MATCH re-tagging an existing node),
// so nothing is recorded and the undo leaves the pre-existing label intact; only
// a label the statement actually added is detached on undo.
func (m mutationUndo) recordSetNodeLabel(n, label string, hadLabel bool) {
	if hadLabel {
		return
	}
	// Adding a label can bring an existence constraint into play on a node that
	// lacks the required property, so record the node for the commit-time check
	// (#1754). Only a label the statement actually added matters (hadLabel=false).
	m.touch(n)
	if !m.active() {
		return
	}
	m.undo.record(func() { m.wv.RemoveNodeLabel(n, label) })
}

// recordRemoveNodeLabel records the inverse of detaching label from n. hadLabel
// is the adapter's pre-call check: when the label was present, the inverse
// re-attaches it; when it was absent the removal was a no-op and nothing is
// recorded.
func (m mutationUndo) recordRemoveNodeLabel(n, label string, hadLabel bool) {
	if !m.active() || !hadLabel {
		return
	}
	m.undo.record(func() { _ = m.wv.SetNodeLabel(n, label) })
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
		m.wv.Revive(n)
		m.wv.Graph().DecrNodesRemoved()
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
			_ = m.wv.SetNodeProperty(n, key, prev)
		} else {
			m.wv.DelNodeProperty(n, key)
		}
	})
}

// recordDelNodeProperty records the inverse of deleting node property key. prev
// (had) is the value captured before deletion; when it existed the inverse
// re-sets it, otherwise the delete was a no-op and nothing is recorded.
func (m mutationUndo) recordDelNodeProperty(n, key string, prev lpg.PropertyValue, had bool) {
	if !had {
		return
	}
	// Removing a property (including SET n.prop = null, a removal in the Cypher
	// data model) can violate an existence constraint if the node still carries
	// the constrained label in its final state, so record it for the commit-time
	// check (#1754). Only a real removal (had=true) matters.
	m.touch(n)
	if !m.active() {
		return
	}
	m.undo.record(func() { _ = m.wv.SetNodeProperty(n, key, prev) })
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
	m.undo.record(func() { m.wv.RemoveEdgeLabel(src, dst, label) })
}

// recordSetEdgeProperty records the inverse of SetEdgeProperty(src, dst, key, …)
// using the prior value captured before the write.
func (m mutationUndo) recordSetEdgeProperty(src, dst, key string, prev lpg.PropertyValue, had bool) {
	if !m.active() {
		return
	}
	m.undo.record(func() {
		if had {
			_ = m.wv.SetEdgeProperty(src, dst, key, prev)
		} else {
			m.wv.DelEdgeProperty(src, dst, key)
		}
	})
}

// recordDelEdgeProperty records the inverse of deleting edge property key using
// the prior value captured before deletion.
func (m mutationUndo) recordDelEdgeProperty(src, dst, key string, prev lpg.PropertyValue, had bool) {
	if !m.active() || !had {
		return
	}
	m.undo.record(func() { _ = m.wv.SetEdgeProperty(src, dst, key, prev) })
}

// recordSetEdgePropertyByHandle records the inverse of
// SetEdgePropertyByHandle(src, dst, handle, key, …) using the per-handle prior
// value (prev, had) the adapter captured BEFORE the write: when the handle
// already carried key the inverse restores the old value, otherwise it deletes
// the key the statement added on that handle.
//
// This inverse is mandatory for SET on a PRE-EXISTING parallel edge: unlike the
// CREATE path — where the enclosing AddEdge inverse removes the whole edge (and
// with it the handle's metadata) — a SET/REMOVE on an edge a prior committed
// statement created has no enclosing edge-removal to lean on, so the per-handle
// property change must be inverted explicitly. On a CREATE rollback this entry
// is harmless: the LIFO replay runs it first (deleting the just-added handle
// property) and then the AddEdge inverse drops the pair, so the two are
// order-independent and idempotent (mirrors recordRemoveEdge's self-sufficient
// per-handle restore).
func (m mutationUndo) recordSetEdgePropertyByHandle(src, dst string, handle uint64, key string, prev lpg.PropertyValue, had bool) {
	if !m.active() || handle == 0 {
		return
	}
	m.undo.record(func() {
		if had {
			_ = m.wv.SetEdgePropertyByHandle(src, dst, handle, key, prev)
		} else {
			m.wv.DelEdgePropertyByHandle(src, dst, handle, key)
		}
	})
}

// recordDelEdgePropertyByHandle records the inverse of
// DelEdgePropertyByHandle(src, dst, handle, key) using the per-handle prior
// value captured before deletion: when the handle carried key the inverse
// re-sets it, otherwise the delete was a no-op on that handle and nothing is
// recorded.
func (m mutationUndo) recordDelEdgePropertyByHandle(src, dst string, handle uint64, key string, prev lpg.PropertyValue, had bool) {
	if !m.active() || handle == 0 || !had {
		return
	}
	m.undo.record(func() { _ = m.wv.SetEdgePropertyByHandle(src, dst, handle, key, prev) })
}

// recordIncEdgeCreateCount records the inverse of bumping the CREATE-multiplicity
// counter for edge (src, dst): decrement it. The counter is metadata only and
// DecEdgeCreateCount floors at zero, so the inverse is exact for the increment
// this entry pairs with.
func (m mutationUndo) recordIncEdgeCreateCount(src, dst string) {
	if !m.active() {
		return
	}
	m.undo.record(func() { m.wv.Graph().DecEdgeCreateCount(src, dst) })
}

// recordDecEdgeCreateCount records the inverse of decrementing the CREATE-
// multiplicity counter: increment it. had reports that the counter was above
// zero before the decrement (so a floored no-op records nothing).
func (m mutationUndo) recordDecEdgeCreateCount(src, dst string, had bool) {
	if !m.active() || !had {
		return
	}
	m.undo.record(func() { m.wv.Graph().IncEdgeCreateCount(src, dst) })
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
	props    map[string]lpg.PropertyValue
	src, dst string
	// handleLabels / handleProps are the removed instance's per-handle label
	// and property pre-images, captured under handle. Restored on undo so the
	// re-added instance resolves to its own metadata. Empty when handle is 0.
	handleLabels []string
	handleProps  map[string]lpg.PropertyValue
	labels       []string
	weight       float64
	createCount  int64
	// handle is the stable handle of the FIRST src→dst adjacency slot — the
	// one RemoveEdge will remove — or 0 when the edge carries no handle
	// (simple-graph or pre-Stage-2 storage). On undo the edge is re-added with
	// this handle so a removed parallel instance keeps its identity.
	handle  uint64
	hadEdge bool
}

// captureRemovedEdge snapshots the state of edge (src, dst) before a
// RemoveEdge. It is called only when undo is active, on the cold DELETE path, so
// its O(out-degree) weight/handle scan and metadata copies never touch the read
// hot path. In addition to the per-pair union it records the FIRST src→dst
// slot's stable handle and that handle's per-instance label/property pre-images,
// so a removed parallel edge can be re-added with its original identity (#1327).
func (m mutationUndo) captureRemovedEdge(src, dst string) removedEdgePreimage {
	// The FIRST src→dst slot is the one [Graph.RemoveEdge] drops; capture its
	// handle so the by-handle capture path below records the exact instance.
	var handle uint64
	if h, ok := m.wv.Graph().FirstEdgeHandle(src, dst); ok {
		handle = h
	}
	return m.captureRemovedEdgeH(src, dst, handle)
}

// captureRemovedEdgeByHandle snapshots the state of the parallel edge instance
// identified by handle on (src, dst) before a [Graph.RemoveEdgeByHandle], so
// the instance-precise removal is invertible. It is the by-handle analogue of
// [mutationUndo.captureRemovedEdge]: where captureRemovedEdge resolves the
// handle of the first slot RemoveEdge would drop, this captures the SPECIFIC
// bound instance's handle so the inverse re-adds exactly that instance — its
// weight, its stable handle, and its own per-handle labels and properties —
// not the first slot's (rmp #2018). The shared inverse [recordRemoveEdge]
// re-adds via [Graph.AddEdgeHIfAbsent] and restores both the per-pair union
// and the per-handle instance surfaces, so a rolled-back instance-precise
// DELETE restores the pair byte-for-byte.
func (m mutationUndo) captureRemovedEdgeByHandle(src, dst string, handle uint64) removedEdgePreimage {
	return m.captureRemovedEdgeH(src, dst, handle)
}

// captureRemovedEdgeH is the shared body of [mutationUndo.captureRemovedEdge]
// and [mutationUndo.captureRemovedEdgeByHandle]: it snapshots the per-pair
// union (weight, labels, properties, CREATE-multiplicity counter) plus, when
// handle is non-zero, that handle's per-instance labels and properties, so the
// inverse re-adds the edge with its original identity. It is called only when
// undo is active, on the cold DELETE path, so its O(out-degree) weight scan and
// metadata copies never touch the read hot path.
//
// The per-pair weight is read from [Graph.EdgeWeight] (the first slot's
// weight); every Cypher relationship is created with the zero weight, so the
// re-added instance's weight is exact for the engine's only caller.
func (m mutationUndo) captureRemovedEdgeH(src, dst string, handle uint64) removedEdgePreimage {
	pre := removedEdgePreimage{src: src, dst: dst}
	if !m.wv.Graph().AdjList().HasEdge(src, dst) {
		return pre
	}
	pre.hadEdge = true
	if w, ok := m.wv.Graph().EdgeWeight(src, dst); ok {
		pre.weight = w
	}
	pre.labels = m.wv.Graph().EdgeLabels(src, dst)
	pre.props = m.wv.Graph().EdgeProperties(src, dst)
	pre.createCount = m.wv.Graph().EdgeCreateCount(src, dst)
	// When the removed instance carries a stable handle, snapshot that handle's
	// per-instance labels and properties so the inverse re-adds the instance
	// with its own metadata even if the removal cleared the handle store.
	if handle != 0 {
		pre.handle = handle
		pre.handleLabels = m.wv.Graph().EdgeLabelsByHandle(src, dst, handle)
		pre.handleProps = m.wv.Graph().EdgePropertiesByHandle(src, dst, handle)
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
		_, _ = m.wv.AddEdgeHIfAbsent(pre.src, pre.dst, pre.weight, pre.handle)
		m.wv.Graph().DecrEdgesRemoved()
		for _, lbl := range pre.labels {
			m.wv.SetEdgeLabel(pre.src, pre.dst, lbl)
		}
		for k, v := range pre.props {
			_ = m.wv.SetEdgeProperty(pre.src, pre.dst, k, v)
		}
		// Re-assert the removed instance's per-handle labels and properties.
		// Idempotent when the handle store survived the removal (the common
		// case: RemoveEdge keeps it while a sibling survives); authoritative
		// when it did not. No-op when handle is 0.
		for _, lbl := range pre.handleLabels {
			m.wv.SetEdgeLabelByHandle(pre.src, pre.dst, pre.handle, lbl)
		}
		for k, v := range pre.handleProps {
			// Restoring a value that passed validation at the original write; ignore the error.
			_ = m.wv.SetEdgePropertyByHandle(pre.src, pre.dst, pre.handle, k, v)
		}
		// Restore the CREATE-multiplicity counter to its captured value. The
		// re-add above does not touch the counter (only IncEdgeCreateCount does),
		// so set it explicitly by replaying the delta from its current value.
		for c := m.wv.Graph().EdgeCreateCount(pre.src, pre.dst); c < pre.createCount; c++ {
			m.wv.Graph().IncEdgeCreateCount(pre.src, pre.dst)
		}
	})
}

// _ pins graph.NodeID into this file's imports so a future inverse that needs a
// NodeID-keyed restore has the type in scope; the helpers above operate on node
// keys, matching the adapter surface.
var _ = graph.NodeID(0)
