package cypher

// count_maintenance.go — relationship count-store delta production (task #2082,
// design docs/count-store-design.md §3). These helpers are called from the two
// mutator adapters (lpgMutatorAdapter, walMutatorAdapter) at the exact edge- and
// label-lifecycle hooks, enqueueing deltas and dirty markings into the
// transaction-scoped exec.CountBuffer that commitUnderBarrier flushes after the
// WAL fsync. Every helper is a no-op when the count store is absent (cs == nil),
// so an engine without a count store pays nothing.
//
// # Which hook produces which delta
//
//   - Edge typed (+): SetEdgeLabelByHandle is the single authoritative
//     once-per-edge typing call (create_relationship.go fires SetEdgeLabel +
//     SetEdgeLabelAt + SetEdgeLabelByHandle for one edge; only the by-handle form
//     is hooked, so the +delta fires exactly once). AddEdge/AddEdgeH cannot carry
//     the delta because the relationship type is assigned afterwards.
//   - Edge removed (-): RemoveEdgeByHandle (instance-precise), RemoveEdge (first
//     slot), and RemoveAllEdgesFrom (every out-edge) capture the removed edge(s)'
//     authoritative per-handle type and the current endpoint labels BEFORE the
//     removal, enqueueing the symmetric negative deltas.
//   - Node relabel (SET/REMOVE n:X): countRelabel — OUT-exact, IN-dirty (§3.3.1).
//
// # Edge-type authority
//
// The per-instance relationship type is read from the by-handle store
// (EdgeLabelsByHandleID), NOT the per-slot inline label column: the slot column
// coalesces across parallel edges and is unreliable for parallel edges of
// differing type (verified empirically). The slot column is used only as the
// handle-less fallback (simple-graph storage, where at most one edge per pair
// exists).

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// countMutator is implemented by the write mutator adapters that maintain the
// relationship count-store. execUnderBarrier extracts the transaction's count
// buffer and store from the mutator so the autocommit Result can flush them
// under the commit barrier (after the WAL fsync).
type countMutator interface {
	countState() (*exec.CountBuffer, *count.Store)
}

// countLabelScratch is the small on-stack capacity for a node's label ids. Nodes
// carry one or a few labels in the overwhelming common case, so an 8-slot array
// avoids a heap allocation while reading endpoint labels; more labels spill to
// the heap via append.
const countLabelScratch = 8

// appendNodeLabelIDs appends the interned label ids of the node identified by id
// to dst (which should be dst[:0]) and returns the extended slice. It reads via
// the allocation-free ForEachNodeLabelByID; the name→id Lookup is lock-free and
// does not re-intern.
func appendNodeLabelIDs(g *lpg.Graph[string, float64], id graph.NodeID, dst []uint32) []uint32 {
	reg := g.Registry()
	g.ForEachNodeLabelByID(id, func(name string) {
		if lid, ok := reg.Lookup(name); ok {
			dst = append(dst, uint32(lid))
		}
	})
	return dst
}

// enqueueEdgeDeltas enqueues the E/D/T deltas for one edge (src)-[rt]->(dst) with
// the given endpoint label-id sets and sign (+1 create, -1 remove).
func enqueueEdgeDeltas(cbuf *exec.CountBuffer, rt uint32, srcLabels, dstLabels []uint32, sign int64) {
	cbuf.EnqueueDelta(count.EDelta(rt, sign))
	for _, la := range srcLabels {
		cbuf.EnqueueDelta(count.DDelta(la, rt, count.Out, sign))
	}
	for _, lb := range dstLabels {
		cbuf.EnqueueDelta(count.DDelta(lb, rt, count.In, sign))
	}
	for _, la := range srcLabels {
		for _, lb := range dstLabels {
			cbuf.EnqueueDelta(count.TDelta(la, rt, lb, sign))
		}
	}
}

// forEachSlotRelType invokes visit for every relationship-type id carried by
// out-slot i of srcID's adjacency (dst = dstID). It prefers the authoritative
// per-handle type(s); a handle-less slot falls back to the decoded inline slot
// label (id+1 encoding, 0 == none).
func forEachSlotRelType(g *lpg.Graph[string, float64], srcID, dstID graph.NodeID, handles []uint64, labs []uint32, i int, visit func(rt uint32)) {
	reg := g.Registry()
	if i < len(handles) && handles[i] != 0 {
		names := g.EdgeLabelsByHandleID(srcID, dstID, handles[i])
		if len(names) > 0 {
			for _, name := range names {
				if rt, ok := reg.Lookup(name); ok {
					visit(uint32(rt))
				}
			}
			return
		}
	}
	if i < len(labs) && labs[i] > 0 {
		visit(labs[i] - 1)
	}
}

// countEdgeTyped enqueues the +1 create-deltas for the edge (src)-[relType]->(dst)
// whose type was just assigned. It reads both endpoints' current (eager) labels.
func countEdgeTyped(g *lpg.Graph[string, float64], cs *count.Store, cbuf *exec.CountBuffer, src, dst, relType string) {
	if cs == nil || relType == "" {
		return
	}
	srcID, ok1 := g.AdjList().Mapper().Lookup(src)
	dstID, ok2 := g.AdjList().Mapper().Lookup(dst)
	if !ok1 || !ok2 {
		return
	}
	// The type was just interned by SetEdgeLabelByHandle; Intern returns the
	// existing id via its lock-free fast path.
	rt := uint32(g.Registry().Intern(relType))
	var sb, db [countLabelScratch]uint32
	sl := appendNodeLabelIDs(g, srcID, sb[:0])
	dl := appendNodeLabelIDs(g, dstID, db[:0])
	enqueueEdgeDeltas(cbuf, rt, sl, dl, +1)
}

// countEdgeRemovedByHandle enqueues the -1 remove-deltas for the single edge
// instance (src)-[handle]->(dst), read via its authoritative per-handle type(s)
// and the current endpoint labels. Must be called while the edge still exists
// (before the underlying removal), and only when the removal will actually
// happen. A zero handle or missing endpoint is a no-op.
func countEdgeRemovedByHandle(g *lpg.Graph[string, float64], cs *count.Store, cbuf *exec.CountBuffer, src, dst string, handle uint64) {
	if cs == nil || handle == 0 {
		return
	}
	srcID, ok1 := g.AdjList().Mapper().Lookup(src)
	dstID, ok2 := g.AdjList().Mapper().Lookup(dst)
	if !ok1 || !ok2 {
		return
	}
	names := g.EdgeLabelsByHandleID(srcID, dstID, handle)
	if len(names) == 0 {
		return
	}
	reg := g.Registry()
	var sb, db [countLabelScratch]uint32
	sl := appendNodeLabelIDs(g, srcID, sb[:0])
	dl := appendNodeLabelIDs(g, dstID, db[:0])
	for _, name := range names {
		if rt, ok := reg.Lookup(name); ok {
			enqueueEdgeDeltas(cbuf, uint32(rt), sl, dl, -1)
		}
	}
}

// countEdgeRemovedFirstSlot enqueues the -1 remove-deltas for the FIRST
// src→dst adjacency slot — the instance a handle-less [Graph.RemoveEdge]
// removes. It resolves the removed slot's type via its stable handle when one
// exists (the multigraph case), falling back to the per-pair label union for
// handle-less simple-graph storage (at most one edge per pair). Must be called
// before the removal, and only when the edge is present.
func countEdgeRemovedFirstSlot(g *lpg.Graph[string, float64], cs *count.Store, cbuf *exec.CountBuffer, src, dst string) {
	if cs == nil {
		return
	}
	if h, ok := g.FirstEdgeHandle(src, dst); ok && h != 0 {
		countEdgeRemovedByHandle(g, cs, cbuf, src, dst, h)
		return
	}
	srcID, ok1 := g.AdjList().Mapper().Lookup(src)
	dstID, ok2 := g.AdjList().Mapper().Lookup(dst)
	if !ok1 || !ok2 {
		return
	}
	names := g.EdgeLabels(src, dst)
	if len(names) == 0 {
		return
	}
	reg := g.Registry()
	var sb, db [countLabelScratch]uint32
	sl := appendNodeLabelIDs(g, srcID, sb[:0])
	dl := appendNodeLabelIDs(g, dstID, db[:0])
	for _, name := range names {
		if rt, ok := reg.Lookup(name); ok {
			enqueueEdgeDeltas(cbuf, uint32(rt), sl, dl, -1)
		}
	}
}

// countAllOutEdgesRemoved enqueues the -1 remove-deltas for EVERY out-edge of n,
// read per-slot (authoritative per-handle type, current endpoint labels), before
// a [Graph.RemoveAllEdgesFrom] strips them. Used by DETACH DELETE; the node's
// in-edges are decremented separately by the caller's RemoveEdge loop.
func countAllOutEdgesRemoved(g *lpg.Graph[string, float64], cs *count.Store, cbuf *exec.CountBuffer, n string) {
	if cs == nil {
		return
	}
	nID, ok := g.AdjList().Mapper().Lookup(n)
	if !ok {
		return
	}
	nbs, _, handles := g.AdjList().LoadEntryH(nID)
	if len(nbs) == 0 {
		return
	}
	labs := g.AdjList().LoadEntryLabels(nID)
	var nb [countLabelScratch]uint32
	nl := appendNodeLabelIDs(g, nID, nb[:0])
	for i, dstID := range nbs {
		var db [countLabelScratch]uint32
		dl := appendNodeLabelIDs(g, dstID, db[:0])
		forEachSlotRelType(g, nID, dstID, handles, labs, i, func(rt uint32) {
			enqueueEdgeDeltas(cbuf, rt, nl, dl, -1)
		})
	}
}

// countRelabel handles a node gaining (sign +1) or losing (sign -1) label on
// node n, per design §3.3.1 (OUT-exact, IN-dirty, X-scoped). The label must
// already be interned. The caller guarantees the count store is present and the
// graph is non-edgeless (AdjList.Size() > 0) — the cheap CREATE (:N) fast path
// (Size()==0) is applied at the adapter hook, before the buffer is even
// allocated, so a bare labelled-node create pays nothing and never dirties.
func countRelabel(g *lpg.Graph[string, float64], cs *count.Store, cbuf *exec.CountBuffer, n, label string, sign int64) {
	x, ok := g.Registry().Lookup(label)
	if !ok {
		// The label was never interned, so no D/T cell references it. (SET interns
		// before this runs; a REMOVE of a never-seen label changes nothing.)
		return
	}
	xID := uint32(x)
	nID, ok := g.AdjList().Mapper().Lookup(n)
	if !ok {
		return
	}

	// IN side: n's in-edges are not enumerable in O(delta) (no reverse index),
	// so mark the minimal X-scoped IN cells dirty — never a wrong exact.
	cbuf.MarkDirty(count.DirtyMark{Label: xID, Scope: count.DirtyDIn})
	cbuf.MarkDirty(count.DirtyMark{Label: xID, Scope: count.DirtyTB})
	// Observability (#2087): a relabel that reaches here always marks the IN
	// X-scoped cells dirty, so count it as one relabel-that-dirtied. An
	// additional OUT-side dirtying below (over-budget) is part of the same
	// relabel and is not double-counted.
	cmetrics.IncCounter(countMetricRelabelDirtied, 1)

	// OUT side: enumerate n's out-edges. Over the out-degree budget, dirty the
	// OUT X-scoped cells instead of recounting — bounded per-relabel work.
	nbs, _, handles := g.AdjList().LoadEntryH(nID)
	if len(nbs) == 0 {
		return
	}
	if budget := cs.MaxRecountEdges(); budget > 0 && len(nbs) > budget {
		cbuf.MarkDirty(count.DirtyMark{Label: xID, Scope: count.DirtyDOut})
		cbuf.MarkDirty(count.DirtyMark{Label: xID, Scope: count.DirtyTA})
		return
	}
	labs := g.AdjList().LoadEntryLabels(nID)
	for i, dstID := range nbs {
		var db [countLabelScratch]uint32
		dl := appendNodeLabelIDs(g, dstID, db[:0])
		forEachSlotRelType(g, nID, dstID, handles, labs, i, func(rt uint32) {
			// D(X, rt, OUT) and T(X, rt, Lb) change exactly by sign.
			cbuf.EnqueueDelta(count.DDelta(xID, rt, count.Out, sign))
			for _, lb := range dl {
				cbuf.EnqueueDelta(count.TDelta(xID, rt, lb, sign))
			}
		})
	}
}

// recomputeCountStore rebuilds the derived, non-durable relationship count-store
// from scratch by an O(V+E) pass over the recovered graph's live nodes and edges
// (task #2084, design docs/count-store-design.md §6). The store starts EMPTY on
// every open, so without this a database reopened over pre-existing relationships
// would report an exact 0 for them.
//
// It first resets the store — clearing every cell AND every X-scoped dirty flag —
// then replays the +1 create-deltas of every live edge, exactly as the
// maintenance path enqueues them on a live CREATE ([enqueueEdgeDeltas]). The
// reopen is therefore the natural self-heal point for any in-session dirty
// degradation (design §4.3, §6.2): the recomputed store equals a ground-truth
// recount of the recovered graph on every cell — E, D and T exact, zero dirty.
//
// Invoked once at construction from [NewEngineWithOptions], after
// registerRecoveredConstraints / registerRecoveredIndexes, from the same fully-
// materialised graph — mirroring registerRecoveredIndexes' backfill-from-
// recovered-graph discipline ([Engine.backfillNodeHashIndex]). Because the counts
// are a pure function of the persisted graph, no WAL op, no checkpoint component
// and no fsync participate, and the store provably cannot diverge from the graph
// regardless of where a crash fell (the crashed process's in-memory store is
// simply discarded and recomputed from the crash-consistent graph on restart).
//
// The walk runs at construction with no concurrent reader or writer — matching
// the recovered-index backfill, which likewise walks the mapper without a
// visibility barrier — so it needs no visMu round; the store's own shard locks
// keep it -race clean. A nil count store or an empty graph makes it a no-op, so
// the store-less write-path benchmark pays nothing.
func (e *Engine) recomputeCountStore() {
	cs := e.countStore
	if cs == nil {
		return
	}
	// Observability (#2087): time the O(V+E) recompute. The histogram's sample
	// count is the number of reopen recomputes; its sum, their total duration.
	defer cmetrics.Time(countMetricRecompute).Stop()
	g := e.g
	// Reset first: a reopen restores full exactness by recomputing from the
	// crash-consistent graph, so any cell the prior session left dirty heals.
	cs.RecomputeReset()
	adj := g.AdjList()
	var buf exec.CountBuffer
	var sb [countLabelScratch]uint32
	adj.Mapper().Walk(func(srcID graph.NodeID, _ string) bool {
		if g.IsTombstoned(srcID) {
			return true
		}
		nbs, _, handles := adj.LoadEntryH(srcID)
		if len(nbs) == 0 {
			return true
		}
		labs := adj.LoadEntryLabels(srcID)
		sl := appendNodeLabelIDs(g, srcID, sb[:0])
		for i, dstID := range nbs {
			// Skip edges to a tombstoned destination: they are not live edges, so
			// the ground-truth recount excludes them and the store must too.
			if g.IsTombstoned(dstID) {
				continue
			}
			var db [countLabelScratch]uint32
			dl := appendNodeLabelIDs(g, dstID, db[:0])
			forEachSlotRelType(g, srcID, dstID, handles, labs, i, func(rt uint32) {
				enqueueEdgeDeltas(&buf, rt, sl, dl, +1)
			})
		}
		// Apply this source node's edge deltas and reset the buffer, bounding the
		// transient buffer to one node's out-fan-out rather than to the whole graph.
		buf.Commit(cs)
		return true
	})
}
