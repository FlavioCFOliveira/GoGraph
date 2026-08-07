package adjlist

// writer.go — the adjacency's transaction-carrying write surface (rmp #2320).
//
// # Why an accessor and not a parameter on every exported method
//
// The transaction has to reach [AdjList.storeEntry], because that is where both
// answers it decides are needed: which commit record this version points at, and
// which transaction owns the shard's private slot-array builder. Getting it there
// is a parameter on the internal chain, and that part is not negotiable.
//
// What IS a choice is the public shape. Widening the eleven exported write
// methods would change signatures with over a thousand call sites in this
// module's tests alone, for a parameter that every one of them would pass empty;
// duplicating them into `…Tx` twins would double a public surface this project
// keeps deliberately small. An accessor does neither: the existing methods stay
// exactly as they are and keep meaning "no transaction", and a caller that HAS a
// transaction takes a [Writer] and calls the same names on it.
//
// It is also the shape the prior art uses. Memgraph puts the transaction on an
// accessor — `Transaction transaction_` is a member of
// `memgraph::storage::Accessor` (`src/storage/v2/storage.hpp`), which exposes the
// mutating API (`virtual VertexAccessor CreateVertex() = 0;`) — while the storage
// primitives underneath take it as an explicit parameter
// (`CreateAndLinkDelta(Transaction *transaction, TObj *object, Args &&...args)`,
// `src/storage/v2/mvcc.hpp`). Read at commit
// 572d5b4311a279de550522344a6f10d352d11c48 (branch master, 2026-08-03). GoGraph
// takes both halves for the same reason Memgraph does: the accessor is where a
// caller's work is scoped, and the parameter is where correctness is enforced.
//
// # What it costs
//
// Nothing measurable. [Writer] is two words — an [AdjList] pointer and a
// one-word [mvcc.Tx] — with no pointer methods and no state of its own, so
// constructing one at a call site is two register moves that do not escape. Each
// method below is a single forward to the `…Tx` form the exported method already
// delegates to, so the transaction-carrying path and the untransacted path run
// the same code with a different value in one parameter.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// Writer is an [AdjList] bound to ONE write transaction: every mutation made
// through it stamps its version with that transaction's shared commit record and
// claims a shard's copy-on-write builder under that transaction's identity.
//
// Obtain one with [AdjList.Writer]. The zero value is unusable; a Writer built
// from the zero [mvcc.Tx] is legal and behaves exactly as the [AdjList]'s own
// methods do — every write is its own transaction, committed the instant it is
// made.
//
// It is valid only while its transaction's bracket is open and must not be
// retained past it. A retained Writer does not corrupt anything — a retracted
// transaction's writes fall back to a fresh untransacted timestamp, see
// [AdjList.versionStamp] — but it silently stops being transactional, so hold it
// no longer than the work it was made for.
//
// It carries no state of its own, so it is safe for concurrent use exactly as
// far as the underlying [AdjList] and the transaction it names are: two
// goroutines must not drive ONE write transaction concurrently, and two
// goroutines with their own transactions may write concurrently.
type Writer[N comparable, W any] struct {
	a  *AdjList[N, W]
	tx mvcc.Tx
}

// Writer returns a's write surface bound to transaction tx.
//
// It allocates nothing.
func (a *AdjList[N, W]) Writer(tx mvcc.Tx) Writer[N, W] { return Writer[N, W]{a: a, tx: tx} }

// Tx returns the transaction this writer carries.
func (wr Writer[N, W]) Tx() mvcc.Tx { return wr.tx }

// AddEdge is [AdjList.AddEdge] inside this writer's transaction.
func (wr Writer[N, W]) AddEdge(src, dst N, w W) error {
	return wr.a.addEdge(src, dst, w, edgeExtra{}, wr.tx)
}

// AddEdgeH is [AdjList.AddEdgeH] inside this writer's transaction.
func (wr Writer[N, W]) AddEdgeH(src, dst N, w W, handle uint64) error {
	return wr.a.addEdge(src, dst, w, edgeExtra{handle: handle, hasHandle: true}, wr.tx)
}

// AddEdgeLabeled is [AdjList.AddEdgeLabeled] inside this writer's transaction.
func (wr Writer[N, W]) AddEdgeLabeled(src, dst N, w W, label uint32) error {
	return wr.a.addEdge(src, dst, w, edgeExtra{label: label, hasLabel: true}, wr.tx)
}

// AddEdgeLabeledH is [AdjList.AddEdgeLabeledH] inside this writer's transaction.
func (wr Writer[N, W]) AddEdgeLabeledH(src, dst N, w W, handle uint64, label uint32) error {
	return wr.a.addEdge(src, dst, w, edgeExtra{handle: handle, hasHandle: true, label: label, hasLabel: true}, wr.tx)
}

// AddEdgeLabeledWithProp is [AdjList.AddEdgeLabeledWithProp] inside this
// writer's transaction.
func (wr Writer[N, W]) AddEdgeLabeledWithProp(src, dst N, w W, label uint32, payload any) error {
	return wr.a.addEdge(src, dst, w, edgeExtra{
		label: label, hasLabel: true,
		auxPayload: payload, hasAuxPayload: true,
	}, wr.tx)
}

// RemoveEdge is [AdjList.RemoveEdge] inside this writer's transaction.
func (wr Writer[N, W]) RemoveEdge(src, dst N) { wr.a.removeEdgeTx(src, dst, wr.tx) }

// RemoveEdgeByHandle is [AdjList.RemoveEdgeByHandle] inside this writer's
// transaction.
func (wr Writer[N, W]) RemoveEdgeByHandle(src, dst N, handle uint64) bool {
	return wr.a.removeEdgeByHandleTx(src, dst, handle, wr.tx)
}

// RemoveAllEdgesFrom is [AdjList.RemoveAllEdgesFrom] inside this writer's
// transaction.
func (wr Writer[N, W]) RemoveAllEdgesFrom(src N) { wr.a.removeAllEdgesFromTx(src, wr.tx) }

// UpdateEntryAux is [AdjList.UpdateEntryAux] inside this writer's transaction.
func (wr Writer[N, W]) UpdateEntryAux(
	src graph.NodeID,
	fn func(cur AuxColumn, neighbours []graph.NodeID) (AuxColumn, bool),
) bool {
	return wr.a.updateEntryAuxTx(src, fn, wr.tx)
}

// SetEdgeLabelSlot is [AdjList.SetEdgeLabelSlot] inside this writer's
// transaction.
func (wr Writer[N, W]) SetEdgeLabelSlot(src, dst graph.NodeID, v uint32) bool {
	return wr.a.setEdgeLabelSlotTx(src, dst, v, wr.tx)
}

// ClearEdgeLabelSlotValue is [AdjList.ClearEdgeLabelSlotValue] inside this
// writer's transaction.
func (wr Writer[N, W]) ClearEdgeLabelSlotValue(src, dst graph.NodeID, v uint32) bool {
	return wr.a.clearEdgeLabelSlotValueTx(src, dst, v, wr.tx)
}

// SetEdgeLabelSlotsAt is [AdjList.SetEdgeLabelSlotsAt] inside this writer's
// transaction.
func (wr Writer[N, W]) SetEdgeLabelSlotsAt(src, dst graph.NodeID, idxs []int, v uint32) int {
	return wr.a.setEdgeLabelSlotsAtTx(src, dst, idxs, v, wr.tx)
}

// ClearEdgeLabelSlotsValue is [AdjList.ClearEdgeLabelSlotsValue] inside this
// writer's transaction.
func (wr Writer[N, W]) ClearEdgeLabelSlotsValue(src, dst graph.NodeID, v uint32) int {
	return wr.a.clearEdgeLabelSlotsValueTx(src, dst, v, wr.tx)
}

// ClearEdgeLabelSlots is [AdjList.ClearEdgeLabelSlots] inside this writer's
// transaction.
func (wr Writer[N, W]) ClearEdgeLabelSlots(src, dst graph.NodeID) {
	wr.a.clearEdgeLabelSlotsTx(src, dst, wr.tx)
}

// SetEdgeLabelSlots is [AdjList.SetEdgeLabelSlots] inside this writer's
// transaction.
func (wr Writer[N, W]) SetEdgeLabelSlots(src graph.NodeID, updates map[graph.NodeID]uint32) int {
	return wr.a.setEdgeLabelSlotsTx(src, updates, wr.tx)
}
