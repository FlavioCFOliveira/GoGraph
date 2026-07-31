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
// One uint64, split at [txIDBase], carries three states, which is what makes
// the visibility test a single comparison rather than a lookup:
//
//	ts <  txIDBase            committed, and ts is the commit timestamp
//	ts == txIDBase + n        in flight, and ts is the transaction id
//	ts == txAbortedTS         aborted
//
// Commit timestamps and transaction ids are both monotonic and neither is ever
// reused, so a reader never has to ask a registry whether a writer is still
// alive.
//
// [txAbortedTS] is deliberately chosen ABOVE txIDBase and equal to no possible
// transaction id, so the ordinary visibility rule already handles it — an
// aborted change reads as "another transaction's uncommitted work" and is
// undone by everyone, forever, with no extra branch on the read path. The
// sentinel is distinguishable only so that garbage collection can recognise a
// chain it may reclaim eagerly.
//
// # Interaction with the existing undo log — a reconciliation P2 owes
//
// GoGraph already rolls a failed statement back PHYSICALLY, through the
// in-memory undo log (#1282), so after an abort the stored value is already
// correct and an aborted delta would make a reader undo a change that is no
// longer there. Under MVCC only one of the two mechanisms may own rollback.
// P1 does not resolve that: it provides the marker and the tests, and
// [labelTx.abort] is documented as not yet wired to any statement path.
// Choosing between "MVCC owns rollback" and "the undo log owns it and deltas
// are discarded on abort" is P2's first decision, and it must be taken with the
// undo log's semantics in front of it, not guessed here.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// txAbortedTS marks a transaction whose changes must never become visible.
//
// It sits above [txIDBase] and equals no transaction id the clock can mint, so
// [nodeLabelDelta.mustUndo] classifies it as another transaction's uncommitted
// work and undoes it, for every reader, forever — without a dedicated branch on
// the read path.
const txAbortedTS = ^uint64(0)

// commitInfo is the commit record shared by every delta one transaction writes.
//
// Publishing the transaction is a single atomic store into ts, so all of its
// deltas change visibility at one instant however many there are. That is the
// whole reason the indirection exists; see the file comment.
//
// Safe for concurrent use.
type commitInfo struct {
	// ts is the transaction id while in flight, the commit timestamp once
	// committed, and [txAbortedTS] once aborted.
	ts atomic.Uint64
}

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
func (g *Graph[N, W]) nextCommitTS() uint64 { return g.mvccClock.Add(1) }

// readTS returns the timestamp a reader starting now must use.
//
// It is the current value of the clock rather than the next one: a transaction
// that commits at T is visible to a reader whose start timestamp is T or later,
// so a reader starting after that commit must observe at least T.
func (g *Graph[N, W]) readTS() uint64 { return g.mvccClock.Load() }

// nextTxID allocates a transaction id, drawn from the range above [txIDBase] so
// it can never be mistaken for a commit timestamp.
func (g *Graph[N, W]) nextTxID() uint64 { return txIDBase + g.mvccTxSeq.Add(1) }

// beginLabelTx starts a transaction over the label chains, capturing the start
// timestamp that decides what it can see.
//
// The order matters: the start timestamp is read BEFORE the transaction id is
// minted, so a transaction can never see a commit that happened after it began.
func (g *Graph[N, W]) beginLabelTx() *labelTx[N, W] {
	startTS := g.readTS()
	id := g.nextTxID()
	info := &commitInfo{}
	info.ts.Store(id)
	return &labelTx[N, W]{g: g, info: info, id: id, startTS: startTS}
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
	ts := t.g.nextCommitTS()
	t.info.ts.Store(ts)
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
	t.info.ts.Store(txAbortedTS)
}

// deltaStamp resolves how a new delta records its visibility.
//
// A transaction supplies its shared record and the inline timestamp is unused;
// an autocommit write supplies none, and gets a fresh commit timestamp inline
// with no record allocated. See [nodeLabelDelta.info] for why the two forms
// exist.
func (g *Graph[N, W]) deltaStamp(info *commitInfo) (*commitInfo, uint64) {
	if info != nil {
		return info, 0
	}
	return nil, g.nextCommitTS()
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
