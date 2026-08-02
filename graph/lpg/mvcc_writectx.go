package lpg

// mvcc_writectx.go — the per-transaction state a write carries (rmp #2301).
//
// # What was per-GRAPH, and why that stops working
//
// Three pieces of write-side state are single fields on the Graph precisely
// because there is exactly one writer: the write stamp holding "the commit
// record of the write currently under the barrier", the adjacency's commit
// window, and the re-entrancy guard's single writer goroutine id (audit
// findings E3, E4, E16). Each becomes a data race the moment two writers
// overlap, and [mvcc.WriteStamp.Begin] says so in its own doc: "the caller must
// be the only writer for the window's duration … so Begin never overwrites a
// live record."
//
// # The failure that made this concrete
//
// rmp #2300 wired write-write conflict detection and it read the writer's
// snapshot from a per-GRAPH field. `make ci` went red on TestGraph_Concurrent
// with a FALSE conflict: 64 goroutines writing disjoint nodes conflicted with
// each other, because reclaimAfterDirectWrite opens an ApplyAtomically bracket
// to run a reclamation sweep (graph/lpg/mvcc_gc.go:135) and, while it is open,
// every other goroutine writing through the direct Go API sees THAT bracket's
// snapshot as its own.
//
// There is no per-goroutine signal to repair that with — the only structure
// that knows which goroutine holds the barrier is barrierGuard, and it is
// `//go:build race || gograph_debug`, absent from a release build. The state
// has to travel WITH the write instead of being looked up beside it.
//
// # The shape
//
// [writeCtx] is that state, threaded through the write path as a parameter, the
// way Memgraph threads `Transaction *transaction` into every accessor
// (memgraph/memgraph, branch master, read 2026-08-02; src/storage/v2/). It
// replaces the bare `info *commitInfo` those functions already took, so the
// threading is a widening of an existing parameter rather than new plumbing.
//
// A nil *writeCtx means "no transaction": a direct Go-API mutation, committed
// the instant it is made. It has no snapshot to be stale against and takes no
// conflict check, which is the correct reading rather than a concession — that
// call is per-operation atomic by contract, not transactional.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// writeCtx is one write transaction's identity, as carried into the stores it
// touches.
//
// It is passed by pointer and never mutated after construction, so two
// concurrent writers hold two distinct values and neither can observe the
// other's — which is the whole point, and what a per-graph field could not give.
//
// The zero value is not usable; obtain one from [Graph.beginWriteCtx] or take
// the nil pointer, which means "not in a transaction".
type writeCtx struct {
	// info is the commit record every version this transaction writes points
	// at, so publishing the transaction stays ONE atomic store however many
	// stores it spans. That indirection is the reason the record is heap
	// allocated; it must not regress into a per-delta timestamp.
	info *commitInfo
	// startTS is the instant this transaction reads at, and txID its identity.
	// Together they are what [mvcc.Visible] needs, so a transaction sees its own
	// uncommitted work and nobody else's — and what [mvcc.Conflicts] needs, so
	// it can tell a version it may overwrite from one it may not.
	startTS uint64
	txID    uint64
}

// beginWriteCtx opens per-transaction write state.
//
// The order matters and is the same as [Graph.beginLabelTx]'s: the start
// timestamp is read BEFORE the transaction id is minted, so a transaction can
// never see a commit that happened after it began.
func (g *Graph[N, W]) beginWriteCtx() *writeCtx {
	startTS := g.readTS()
	id := g.nextTxID()
	return &writeCtx{info: mvcc.NewCommitInfo(id), startTS: startTS, txID: id}
}

// record returns the commit record to stamp a version with, or nil when there
// is no transaction.
func (w *writeCtx) record() *commitInfo {
	if w == nil {
		return nil
	}
	return w.info
}

// conflicts reports whether this transaction may displace a version whose
// effective timestamp is headTS.
//
// A nil receiver — a direct Go-API mutation outside any transaction — never
// conflicts: it is committed the instant it is made and has no snapshot to be
// stale against.
//
// This is where the per-transaction state pays for itself. The predicate is the
// same one rmp #2300 defined, but the startTS and txID it reads now travel with
// the write instead of being looked up on the graph, so a concurrent writer
// cannot be tested against a transaction that is not its own.
func (w *writeCtx) conflicts(headTS uint64) bool {
	if w == nil {
		return false
	}
	return mvcc.Conflicts(headTS, w.startTS, w.txID)
}

// conflictErr builds the typed serialization error for a conflict this
// transaction hit in store.
func (w *writeCtx) conflictErr(store string, headTS uint64) error {
	return mvcc.NewConflict(store, headTS, w.startTS, w.txID)
}

// headStamp returns the effective timestamp of the newest version recorded for
// id in this label shard, or zero when the chain is empty.
//
// The caller must hold the shard lock. Zero means nothing has written this node
// since the last reclamation, which never conflicts: reclamation only frees
// versions below the watermark, and anything below it is visible to every live
// transaction.
func (sh *nodeLabelShard) headStamp(id graph.NodeID) uint64 {
	d := sh.d[id]
	if d == nil {
		return 0
	}
	if d.info != nil {
		return d.info.TS()
	}
	return d.ts
}

// headStamp returns the effective timestamp of the newest version recorded for
// id in this property shard, or zero when the chain is empty.
//
// The caller must hold the shard lock. See [nodeLabelShard.headStamp].
func (s *nodePropShard) headStamp(id graph.NodeID) uint64 {
	d := s.d[id]
	if d == nil {
		return 0
	}
	if d.info != nil {
		return d.info.TS()
	}
	return d.ts
}
