package lpg

// mvcc_txn.go — MVCC P1 (rmp #2278): the transaction clock and the shared
// commit record.
//
// # What P0 got wrong, and why it matters at scale
//
// The P0 spike stamped a timestamp onto each delta. That is fine for a probe
// that measures one modification, and wrong for a transaction: committing would
// have to WALK every delta the transaction created and store a timestamp into
// each. That makes commit O(writes), and — far worse — it makes atomic
// visibility impossible, because a reader arriving mid-walk would see part of
// the transaction and not the rest. Atomicity is the property the whole module
// is built on; a versioning substrate that cannot publish a transaction at one
// instant is not a substrate.
//
// Memgraph solves it by indirection, and the reason is stated in its own source
// (`src/storage/v2/transaction.hpp`): the commit record is heap-allocated
// "because `Delta`s have a pointer to it, and that pointer must stay valid
// after the `Transaction` is moved". Every delta of one transaction points at
// ONE record; publishing the transaction is a single store into it.
//
// # The timestamp space
//
// One uint64, split at [mvcc.TxIDBase], carries three states, which is what
// makes the visibility test a single comparison rather than a lookup:
//
//	ts <  mvcc.TxIDBase        committed, and ts is the commit timestamp
//	ts >= mvcc.TxIDBase        in flight, and ts is the transaction id
//	ts == mvcc.AbortedTS       aborted
//
// Commit timestamps and transaction ids are both monotonic and neither is ever
// reused, so a reader never has to ask a registry whether a writer is still
// alive.
//
// [mvcc.AbortedTS] is deliberately chosen ABOVE mvcc.TxIDBase and equal to no possible
// transaction id, so the ordinary visibility rule already handles it — an
// aborted change reads as "another transaction's uncommitted work" and is
// undone by everyone, forever, with no extra branch on the read path. The
// sentinel is distinguishable only so that garbage collection can recognise a
// chain it may reclaim eagerly.
//
// # Interaction with the existing undo log — RESOLVED: they compose
//
// GoGraph already rolls a failed statement back PHYSICALLY, replaying an undo
// log of inverse closures while the visibility barrier is held
// (cypher/undo.go, #1282). The worry was a double-undo: the stored value is
// already restored, and a reader applying the aborted transaction's deltas on
// top would land somewhere else entirely.
//
// It does not happen, because the undo log's inverses call the SAME lpg
// mutators, so each inverse records its own delta:
//
//	set L      stored: L present   chain: [undoRemove L]
//	inverse    stored: L absent    chain: [undoAdd L, undoRemove L]
//	reader     undoAdd -> present, undoRemove -> absent   = the original value
//
// So the undo log KEEPS ownership of physical rollback and needs no change,
// and MVCC needs no special abort path for the stored value.
// [labelTx.abort] marks the record so garbage collection can recognise a chain
// it may reclaim eagerly; correctness does not depend on it, since an unmarked
// aborted transaction keeps its transaction id and is undone by every reader
// anyway. The cost is that an aborted transaction leaves twice the deltas,
// which is acceptable: abort is the rare path and P6 reclaims them.
//
// Pinned by TestLabelTx_ComposesWithPhysicalUndo.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// commitInfo is [mvcc.CommitInfo], aliased so this package's call sites read
// unchanged. It MOVED to a shared package in P3 because the adjacency lives in
// [adjlist], which lpg imports, and both stores must answer the same question
// about the same transaction — so one transaction's labels, properties AND
// topology all become visible at the same instant, from one store.
type commitInfo = mvcc.CommitInfo

// labelTx is a transaction over the node-label delta chains.
//
// It is unexported on purpose. P1 delivers a substrate, not an API: exporting a
// transaction type now would commit the module to a shape before P2 and P3 have
// shown what properties and edges need from it.
//
// Not safe for concurrent use by multiple goroutines.
type labelTx[N comparable, W any] struct {
	g       *Graph[N, W]
	info    *commitInfo
	id      uint64
	startTS uint64
	done    bool
}

// nextCommitTS allocates the next commit timestamp. Monotonic and never reused.
func (g *Graph[N, W]) nextCommitTS() uint64 { return g.mvccClock.NextCommitTS() }

// readTS returns the timestamp a reader starting now must use.
//
// It is the current value of the clock rather than the next one: a transaction
// that commits at T is visible to a reader whose start timestamp is T or later,
// so a reader starting after that commit must observe at least T.
func (g *Graph[N, W]) readTS() uint64 { return g.mvccClock.ReadTS() }

// nextTxID allocates a transaction id, drawn from the range above [mvcc.TxIDBase] so
// it can never be mistaken for a commit timestamp.
func (g *Graph[N, W]) nextTxID() uint64 { return g.mvccClock.NextTxID() }

// beginLabelTx starts a transaction over the label chains, capturing the start
// timestamp that decides what it can see.
//
// The order matters: the start timestamp is read BEFORE the transaction id is
// minted, so a transaction can never see a commit that happened after it began.
func (g *Graph[N, W]) beginLabelTx() *labelTx[N, W] {
	startTS := g.readTS()
	id := g.nextTxID()
	return &labelTx[N, W]{g: g, info: mvcc.NewCommitInfo(id), id: id, startTS: startTS}
}

// commit publishes every delta this transaction wrote, atomically.
//
// One store, regardless of how many deltas there are: that is the property the
// shared commit record exists to provide. After it returns, a reader whose
// start timestamp is at or after the allocated commit timestamp sees all of the
// transaction's changes, and one that started earlier sees none of them. There
// is no interval in which it sees some.
func (t *labelTx[N, W]) commit() uint64 {
	if t.done {
		panic("lpg: labelTx committed or aborted twice")
	}
	t.done = true
	// Allocate, store, publish — in that order; see [mvcc.Clock.ReadTS].
	ts := t.g.nextCommitTS()
	t.info.Commit(ts)
	t.g.mvccClock.PublishCommitTS(ts)
	return ts
}

// abort marks the transaction's changes permanently invisible.
//
// Also one store. See the file comment: this is NOT yet wired to any statement
// path, because GoGraph's existing undo log already rolls a failed statement
// back physically and only one of the two mechanisms may own rollback.
func (t *labelTx[N, W]) abort() {
	if t.done {
		panic("lpg: labelTx committed or aborted twice")
	}
	t.done = true
	t.info.Abort()
}

// deltaStamp resolves how a new delta records its visibility, in three cases
// that are deliberately ordered cheapest-first.
//
// An explicit caller's record wins: [labelTx] threads its own, and that is the
// substrate's own transaction type. Otherwise the write inherits the record of
// whatever transaction currently holds the barrier, which is what makes a
// multi-op statement atomically visible — without it `CREATE (a)-[:R]->(b)`
// would stamp the node, the edge and each property at different instants and a
// reader could observe the node without the edge (rmp #2288). Only a write with
// neither — a direct Go-API mutation outside any transaction — takes a fresh
// commit timestamp inline, which is right: it is committed the instant it is
// made and needs no shared mutable record. See [nodeLabelDelta.info] for why
// the inline form exists at all.
func (g *Graph[N, W]) deltaStamp(info *commitInfo) (*commitInfo, uint64) {
	if info != nil {
		return info, 0
	}
	if !g.mvccArmed {
		// Unversioned: nothing reads this timestamp back, but publish it anyway
		// so the two counters cannot drift if the substrate is armed later.
		ts := g.nextCommitTS()
		g.mvccClock.PublishCommitTS(ts)
		return nil, ts
	}
	// Through the SHARED stamp, not straight to the clock. Going straight to
	// the clock produced the right timestamp and lost the accounting: the
	// stamp is what counts versions made outside any transaction, and without
	// that count nothing ever charges the reclamation debt for a direct Go-API
	// write. Measured before the fix: 20 000 direct writes left 40 000 live
	// version records and a debt of ZERO, so the sweep could never fire and the
	// memory was unbounded (rmp #2289).
	return g.stamp.Stamp()
}

// setNodeLabel writes a label inside this transaction. The delta it records
// stays invisible to every other reader until [labelTx.commit].
func (t *labelTx[N, W]) setNodeLabel(n N, name string) error {
	return t.g.setNodeLabelInfo(n, name, t.info)
}

// removeNodeLabel removes a label inside this transaction.
func (t *labelTx[N, W]) removeNodeLabel(n N, name string) {
	t.g.removeNodeLabelInfo(n, name, t.info)
}

// labelsOf reads a node's label set as this transaction must see it: its own
// uncommitted writes included, every other in-flight transaction's excluded,
// and committed work only if it committed at or before this transaction began.
func (t *labelTx[N, W]) labelsOf(id graph.NodeID) labelBag {
	return t.g.labelBagAsOf(id, t.startTS, t.id)
}

// setNodeProperty writes a property inside this transaction.
func (t *labelTx[N, W]) setNodeProperty(n N, key string, v PropertyValue) error {
	return t.g.setNodePropertyInfo(n, key, v, t.info)
}

// delNodeProperty deletes a property inside this transaction.
func (t *labelTx[N, W]) delNodeProperty(n N, key string) {
	t.g.delNodePropertyInfo(n, key, t.info)
}

// propsOf reads a node's property set as this transaction must see it.
func (t *labelTx[N, W]) propsOf(id graph.NodeID) propBag {
	return t.g.propBagAsOf(id, t.startTS, t.id)
}
