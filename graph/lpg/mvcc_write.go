package lpg

// mvcc_write.go — MVCC P4a (rmp #2288): one shared commit record per write, and
// the arming of the versioning substrate.
//
// # The defect this closes
//
// P1 to P3 built version chains in every store a read touches, but nothing
// armed them and no write allocated a commit record. A delta made outside a
// transaction therefore took its timestamp from [Graph.deltaStamp], which minted
// a FRESH one per delta. For a single-property write that is right — it is
// committed the instant it is made. For a multi-op statement it is wrong, and
// wrong in the way that matters most: `CREATE (a)-[:R]->(b)` would stamp the
// node, the edge and each property at different instants, so a reader could
// observe the node without the edge. Atomic visibility is the property the
// whole module rests on.
//
// # Where the record is allocated, and why there
//
// A write transaction already has exact brackets in this package —
// [Graph.ApplyAtomically] for one applied statement and
// [Graph.LockBarrier]/[Graph.UnlockBarrier] for an explicit multi-statement
// transaction — and those brackets already open and close the adjacency's
// commit window. The commit record is allocated and published on the same
// brackets, so a transaction's labels, properties, topology, relationship types
// and edge properties all point at ONE record and all become visible with one
// atomic store, however many stores they span.
//
// [Graph.ApplyInsideLocked] deliberately allocates NOTHING: it is the
// statement-inside-an-explicit-transaction path, and its statements must share
// the outer record or the transaction is not atomically visible.
//
// # Publication on rollback
//
// A rolled-back statement PUBLISHES its record rather than aborting it, and the
// reason is that GoGraph rolls back PHYSICALLY. The in-memory undo log
// (cypher/undo.go, #1282) replays inverse mutations through the same lpg
// mutators, so each inverse records its own delta on the same chain:
//
//	set L      stored: L present   chain: [undoRemove L]
//	inverse    stored: L absent    chain: [undoAdd L, undoRemove L]
//
// The stored value is already correct when the barrier is released. Committing
// the record makes the chain agree with it: a reader from before the statement
// walks both records and lands on the original value, and a reader from after
// it takes the stored value directly. Aborting the record would also be
// correct — every reader would undo both — but it would keep the chain alive
// past the point where anything needs it, and it would make the eager-reclaim
// signal ([mvcc.AbortedTS]) mean two different things. Pinned by
// TestLabelTx_ComposesWithPhysicalUndo.

// beginWrite opens the stamping window for one write transaction.
//
// It ALLOCATES NOTHING. The shared commit record is created by the first
// version that needs one, so a bracket that versions nothing — a read-only
// apply, or a write that changes no value — stays allocation-free, which
// [Graph.ApplyAtomically] is guarded to be. See [mvcc.WriteStamp].
//
// The caller must hold the visibility barrier in write mode. Nested calls are
// impossible by construction — the barrier is not re-entrant and
// [Graph.ApplyInsideLocked] does not call this — so the window never nests.
func (g *Graph[N, W]) beginWrite() {
	if !g.mvccArmed {
		return
	}
	g.stamp.Begin()
}

// endWrite publishes every version this write created, atomically, and closes
// the stamping window.
//
// One store, however many versions there are across however many stores: that
// is the whole reason the record is shared. It runs while the barrier is still
// held, so the instant at which a transaction becomes visible is exactly the
// instant it is today — this phase moves no visibility boundary.
//
// It publishes on the rollback path too; see the file comment for why.
func (g *Graph[N, W]) endWrite() {
	if !g.mvccArmed {
		return
	}
	info, created := g.stamp.End()
	if info == nil {
		// The transaction versioned nothing, so there is no record to publish,
		// nothing to reclaim, and no reason to allocate a commit timestamp.
		return
	}
	info.Commit(g.mvccClock.NextCommitTS())
	g.reclaimDebt.Add(created)
	g.reclaimIfDue()
}

// EnableMVCC arms the whole versioning substrate: node labels, node properties
// and the adjacency — which carries the per-slot relationship types and the
// columnar edge properties inside the same immutable entry, so versioning the
// entry versions all three.
//
// It is on by default (see [New]) and this entry point exists so a benchmark
// can compare both arms in ONE process rather than across two builds, which on
// this project's hardware has manufactured phantom regressions from a
// byte-identical control.
//
// Must be called before any write and never concurrently with another
// operation.
//
// Not safe for concurrent use.
func (g *Graph[N, W]) EnableMVCC() {
	g.mvccArmed = true
	g.labelDeltas = true
	g.propDeltas = true
	g.stamp.SetClock(&g.mvccClock)
	g.adj.EnableVersioning()
	g.adj.SetWriteStamp(&g.stamp)
}

// DisableMVCC disarms the versioning substrate.
//
// Reads then observe the current value with no version walk and writes record
// no versions, which is the pre-MVCC behaviour. It exists for the same
// same-process A/B reason as [Graph.EnableMVCC] and for a caller that knowingly
// wants neither snapshot isolation nor the per-modification cost.
//
// Must be called before any write and never concurrently with another
// operation.
//
// Not safe for concurrent use.
func (g *Graph[N, W]) DisableMVCC() {
	g.mvccArmed = false
	g.labelDeltas = false
	g.propDeltas = false
	g.adj.DisableVersioning()
	g.adj.SetWriteStamp(nil)
}

// MVCCEnabled reports whether the versioning substrate is armed.
func (g *Graph[N, W]) MVCCEnabled() bool { return g.mvccArmed }
