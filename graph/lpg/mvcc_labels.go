package lpg

// mvcc_labels.go — P0 SPIKE (rmp #2275): per-object delta chains for node
// labels, to settle empirically what a delta-chain MVCC costs in GoGraph.
//
// # THIS IS A COST PROBE, NOT PHASE ONE
//
// It is off unless [Graph.EnableLabelDeltas] is called, nothing in the module
// turns it on, and it deliberately implements only enough to be MEASURED: a
// delta per label mutation, a chain per node, and a visibility walk. There is
// no transaction integration, no garbage collection, and no read-path
// retirement of the visibility barrier. Those are phases P1 to P7 of
// docs/design-mvcc-delta-chains.md, and none of them is authorised until the
// measurement this file exists to produce is on the record.
//
// # The question it answers
//
// rmp #2051 prototyped a WHOLE-GRAPH per-shard copy-on-write snapshot and
// measured 5.4× time and 43× memory on the write path, then reverted it. Its
// root cause is sound — Go maps have no O(1) immutable snapshot, so eager COW
// deep-clones a whole shard map, giving O(shard size) per write. Its
// CONCLUSION, that MVCC therefore needs the LPG core maps replaced with
// persistent structures, is a property of the whole-graph-snapshot model, and
// neither reference implementation uses that model. The one question worth a
// sprint is therefore: **is the per-write cost of a delta chain independent of
// graph size?** Everything here is shaped to answer exactly that.
//
// # Reference: Memgraph, read at master on 2026-07-31
//
// `src/storage/v2/delta.hpp` — a Delta is a tagged union recording ONE
// modification (ADD_LABEL, REMOVE_LABEL, SET_PROPERTY, ADD_IN_EDGE, …) carrying
// only what changed, with a commit-info pointer, a command id and prev/next
// links, bounded at 56 bytes. `src/storage/v2/vertex.hpp` — each Vertex owns an
// RWSpinLock and a PointerPack<Delta,2>. `src/storage/v2/mvcc.hpp` — a reader
// walks the chain backwards applying undo records until it reaches the version
// visible at its start timestamp.
//
// The timestamp encoding is taken from Memgraph directly because it is the part
// that makes the visibility test a single comparison: a delta's timestamp holds
// the WRITER'S TRANSACTION ID while uncommitted and its COMMIT TIMESTAMP once
// committed, with the two drawn from disjoint ranges either side of
// [mvcc.TxIDBase]. One uint64 then distinguishes "mine, uncommitted",
// "committed, compare against my start timestamp" and "someone else's,
// uncommitted". The primitives moved to graph/mvcc in P3 so the adjacency,
// which lives in a package lpg imports, shares one commit record with them.
//
// # Where this DIVERGES from Memgraph, and why
//
// Memgraph hangs the delta pointer off the Vertex struct, which is free because
// a Vertex is already a struct. GoGraph has no per-node struct: node labels
// live in `map[graph.NodeID]labelBag` across 64 shards, so a pointer field on
// labelBag would grow the map's value for EVERY labelled node — a permanent
// memory cost paid by graphs that never write.
//
// Deltas are transient: a node has one only between a write and the moment the
// last transaction that could need the old version finishes. So the head lives
// in a SPARSE side map per shard, allocated lazily and holding only the nodes
// with a live chain, and a graph-level atomic counter mirrors the total. A
// reader that finds the counter at zero — the whole of a read-only workload,
// and the overwhelming majority of a mixed one — does one atomic load and takes
// the current value, touching neither the side map nor a chain. That is the
// same lock-free-gate idiom [Graph.tombstoneActive] and
// [Graph.edgeLabelOverflowActive] already use for exactly this reason.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// Label-delta actions. A delta records the UNDO of the change that created it,
// so replaying the chain backwards reconstructs the older version.
const (
	// undoRemoveLabel undoes an ADD: the label was not present before.
	undoRemoveLabel uint8 = iota
	// undoAddLabel undoes a REMOVE: the label was present before.
	undoAddLabel
)

// nodeLabelDelta is one undo record in a node's label-version chain.
//
// The layout is deliberately flat and small — 24 bytes on a 64-bit target
// (pointer, uint64, uint32, uint8 plus padding) against Memgraph's 56-byte
// bound, which it can be because a label delta needs no property value and no
// edge reference. A delta per modification IS the cost model, so anything
// carried here is paid on every write.
type nodeLabelDelta struct {
	// next is the older version's delta, or nil when this is the oldest
	// recorded change. Loaded with acquire semantics by a reader.
	next *nodeLabelDelta
	// info is the commit record SHARED by every delta the same transaction
	// wrote, so publishing a transaction is one atomic store however many
	// deltas it created and no reader can observe half of one (rmp #2278).
	//
	// It is NIL for an autocommit write, in which case [nodeLabelDelta.ts]
	// below carries the commit timestamp directly. That case is worth a branch
	// and eight bytes: an autocommit write is already committed the instant it
	// is made, so it needs no shared mutable record, and allocating one cost
	// +16 B and +1 allocation per write — measured, on the commonest path the
	// Go API has.
	info *commitInfo
	// ts is the commit timestamp of an autocommit write, read only when info is
	// nil. An autocommit delta is immutable once created.
	ts uint64
	// lid is the label this record adds back or removes.
	lid LabelID
	// action is [undoAddLabel] or [undoRemoveLabel].
	action uint8
}

// visibleTo reports whether the version BELOW this delta is the one a reader
// with the given start timestamp and transaction id should see — that is,
// whether this delta must be applied as an undo.
//
// The three cases are Memgraph's, in the same order:
//
//   - the delta is the reader's OWN uncommitted change, so the reader must see
//     it and must NOT undo it;
//   - the delta is committed, so it is undone only when it committed at or
//     after the reader started;
//   - the delta belongs to another transaction that has not committed, so it is
//     always undone.
func (d *nodeLabelDelta) mustUndo(startTS, txID uint64) bool {
	ts := d.ts
	if d.info != nil {
		ts = d.info.TS()
	}
	return !mvcc.Visible(ts, startTS, txID)
}

// labelDeltasEnabled reports whether the spike is armed. It is a plain field
// read: the flag is set once at construction and never mutated afterwards.
func (g *Graph[N, W]) labelDeltasEnabled() bool { return g.labelDeltas }

// EnableLabelDeltas arms the P0 MVCC spike for node labels (rmp #2275).
//
// It exists so both arms can be measured in ONE process toggled by an option
// rather than by two builds compared back to back, which on this machine has
// manufactured phantom regressions from a byte-identical control.
//
// It must be called before any label is written and never concurrently with
// another operation on g.
//
// # It is a NO-OP on a graph from [New] (rmp #2623)
//
// The substrate moved underneath this seam. [Graph.armMVCC] sets labelDeltas and
// runs by default, so on any graph this package hands out the flag is ALREADY
// set and this call changes nothing. It survives because the two tests that call
// it still read correctly, not because it arms anything.
//
// A benchmark that used it to build an "armed but no live delta" fixture got a
// timing-dependent count instead — 13, 153, 4965 across bench times — because
// the seeding writes it believed were delta-free were not. The seam that
// actually toggles the substrate is [Graph.disarmMVCCForTest]; use that, disarm
// before seeding, and re-arm after.
//
// Not safe for concurrent use.
func (g *Graph[N, W]) EnableLabelDeltas() { g.labelDeltas = true }

// LabelDeltaCount returns the number of live node-label delta records.
//
// It is the lock-free gate a reader consults before considering a chain walk:
// zero means no node in the graph has an unreclaimed older version, which is
// the whole of a read-only workload. It is also the memory bound the spike
// reports, since nothing reclaims deltas yet.
//
// Safe for concurrent use.
func (g *Graph[N, W]) LabelDeltaCount() int64 { return g.labelDeltaActive.Load() }

// pushLabelDelta records the undo of a label change on id and links it at the
// head of that node's chain. The caller must hold the shard's write lock.
//
// This is the entire write-side cost of the design: one allocation, one map
// assignment into a sparse side map, and one atomic increment — none of which
// depends on the number of nodes in the graph or in the shard. That
// independence is the property the spike measures.
func (sh *nodeLabelShard) pushLabelDelta(id graph.NodeID, action uint8, lid LabelID, info *commitInfo, ts uint64, active *atomic.Int64) {
	if sh.d == nil {
		// Lazily allocated: a shard that is never written keeps no side map, so
		// a read-only graph pays nothing for the mechanism existing.
		sh.d = make(map[graph.NodeID]*nodeLabelDelta, 8)
	}
	sh.d[id] = &nodeLabelDelta{
		next:   sh.d[id],
		info:   info,
		ts:     ts,
		lid:    lid,
		action: action,
	}
	active.Add(1)
}

// labelBagAsOf reconstructs the label set of id as it was at startTS.
//
// The fast path is the point of the design: when the graph holds no live delta
// at all the function does one atomic load and returns the current bag, which
// is exactly what a non-MVCC read does plus a single uncontended load. The
// chain walk runs only for a node that a concurrent writer has actually
// touched.
//
// The returned bag is a private copy whenever any undo was applied, so the
// caller may read it without holding the shard lock; when no undo applied it
// aliases the stored bag and the caller must already hold the read lock, which
// every caller here does.
func (g *Graph[N, W]) labelBagAsOf(id graph.NodeID, startTS, txID uint64) labelBag {
	sh := g.nodeLabelShardFor(id)
	// FAST PATH. The gate is the shard's own side map, read UNDER the lock and
	// therefore atomically with the value — see [Graph.propBagAsOf] for the torn
	// read a graph-level counter sampled before the lock produced. The body
	// still holds no defer: a first version deferred the RUnlock and put the
	// chain walk inline, which made the function too large to inline and cost
	// 9.06 ns against a 5.03 ns baseline.
	sh.mu.RLock()
	cur := sh.m[id]
	if sh.d == nil {
		sh.mu.RUnlock()
		return cur
	}
	out := g.labelBagAsOfLocked(sh, id, startTS, txID)
	sh.mu.RUnlock()
	return out
}

// labelBagAsOfLocked is [Graph.labelBagAsOfSlow] with the shard read lock
// already held by the caller.
//
// It exists because the returned bag ALIASES the stored one whenever no undo
// applied, so consuming it after the lock is released is a data race on the
// bag's backing array — which `-race` caught as a write in labelBag.del racing
// a read in labelBag.has. Callers that consume the bag rather than copying it
// out must therefore hold the lock across the consumption, and this is what
// they resolve through.
func (g *Graph[N, W]) labelBagAsOfLockedSnap(sh *nodeLabelShard, id graph.NodeID, snap *Snapshot, startTS, txID uint64) labelBag {
	cur := sh.m[id]
	if sh.d == nil {
		return cur
	}
	d := sh.d[id]
	if d == nil {
		return cur
	}
	var out labelBag
	copied := false
	for ; d != nil; d = d.next {
		if snap.visible(d.info, d.ts, startTS, txID) {
			break
		}
		if !copied {
			out = cloneLabelBag(cur)
			copied = true
		}
		switch d.action {
		case undoAddLabel:
			out.add(d.lid)
		case undoRemoveLabel:
			out.del(d.lid)
		}
	}
	if !copied {
		return cur
	}
	return out
}

func (g *Graph[N, W]) labelBagAsOfLocked(sh *nodeLabelShard, id graph.NodeID, startTS, txID uint64) labelBag {
	cur := sh.m[id]
	if sh.d == nil {
		return cur
	}
	d := sh.d[id]
	if d == nil {
		return cur
	}
	// Walk backwards, applying undo records until the version visible at
	// startTS is reconstructed. The bag is copied before the first mutation so
	// the stored version is never disturbed.
	var out labelBag
	copied := false
	for ; d != nil; d = d.next {
		if !d.mustUndo(startTS, txID) {
			break
		}
		if !copied {
			out = cloneLabelBag(cur)
			copied = true
		}
		switch d.action {
		case undoAddLabel:
			out.add(d.lid)
		case undoRemoveLabel:
			out.del(d.lid)
		}
	}
	if !copied {
		// Every delta on the chain is already visible to this reader, so the
		// current value IS its version and no copy was needed.
		return cur
	}
	return out
}

// cloneLabelBag returns a deep-enough copy of b that mutating the copy cannot
// disturb the stored version. The map form must be copied; the singleton and
// small-slice forms are copied because the slice header aliases its backing
// array.
//
// It is called only when a chain walk actually has an undo to apply, so a read
// that finds no visible delta never pays for it.
func cloneLabelBag(b labelBag) labelBag {
	out := b
	switch {
	case b.m != nil:
		out.m = make(map[LabelID]struct{}, len(b.m))
		for k := range b.m {
			out.m[k] = struct{}{}
		}
	case len(b.ids) > 0:
		out.ids = make([]LabelID, len(b.ids))
		copy(out.ids, b.ids)
	}
	return out
}

// labelBagPlain is the non-MVCC read, exposed with the SAME signature shape as
// [Graph.labelBagAsOf] so the two can be benchmarked against each other fairly.
//
// It exists because the first control for that comparison was the read written
// inline in the benchmark loop, where the compiler inlines the shard index, the
// lock and the map access and never copies a 40-byte labelBag out through a
// return. Measured against that control the MVCC read looked 60 % slower; most
// of the gap was the call and the return copy, which the control was not
// paying and the real caller always would.
func (g *Graph[N, W]) labelBagPlain(id graph.NodeID) labelBag {
	sh := g.nodeLabelShardFor(id)
	sh.mu.RLock()
	cur := sh.m[id]
	sh.mu.RUnlock()
	return cur
}
