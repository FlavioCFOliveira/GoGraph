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
	"sync/atomic"

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
	// conflict is the first serialization conflict this transaction hit, and
	// the ONLY mutable field here. It is the flag Memgraph carries as
	// `transaction->must_abort` (memgraph/memgraph, branch master, read
	// 2026-08-02; src/storage/v2/mvcc.hpp PrepareForWrite sets it,
	// src/storage/v2/storage.cpp Commit reads it and returns
	// SerializationError). Detection RECORDS rather than returns because
	// several push primitives return nothing — removeNodeLabelInfo,
	// delNodePropertyInfo and the five per-edge side stores — and a
	// serialization failure aborts the WHOLE transaction, not one label write,
	// so threading an error out of each would cascade signature changes through
	// the public Go API for a failure that is not per-operation at all.
	//
	// Atomic because the flag is the one thing two goroutines writing through
	// the SAME transaction could touch at once, and -race is this task's
	// acceptance instrument. It is read once per version actually recorded,
	// never on a read path.
	conflict atomic.Pointer[mvcc.Conflict]
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
	// An already-doomed transaction refuses every further write, so the answer
	// does not depend on this object at all. The load is ordered first because
	// it is the cheaper test and because a doomed transaction must not be able
	// to slip a write past on an object that happens not to conflict.
	if w.conflict.Load() != nil {
		return true
	}
	return mvcc.Conflicts(headTS, w.startTS, w.txID)
}

// conflictErr records a conflict this transaction hit in store and returns the
// typed serialization error for it.
//
// It RECORDS as well as returns, because the transaction is now doomed however
// the caller treats the return: a primitive that can report the failure and one
// that cannot must leave the transaction in the same state, or a statement
// whose only conflicting write went through a void-returning primitive would
// commit having silently dropped it. That was measured, not assumed —
// TestWriteCtx_VoidPrimitiveConflictDoomsTheTransaction fails with a lost
// update against a build that only returns.
//
// The FIRST conflict wins. A doomed transaction may run more writes before its
// caller notices, and the conflict that explains the failure is the one that
// caused it, not the last one it tripped over on the way out.
func (w *writeCtx) conflictErr(store string, headTS uint64) error {
	c := mvcc.NewConflict(store, headTS, w.startTS, w.txID)
	if !w.conflict.CompareAndSwap(nil, c) {
		return w.conflict.Load()
	}
	return c
}

// doomed reports whether this transaction has already hit a serialization
// conflict and can no longer commit.
//
// A doomed transaction SKIPS its remaining writes. Applying one would put a
// version on a chain whose head belongs to someone else, and the transaction is
// going to abort regardless — Memgraph's PrepareForWrite callers return without
// writing for the same reason.
//
// A nil receiver — a direct Go-API mutation outside any transaction — is never
// doomed.
func (w *writeCtx) doomed() bool {
	return w != nil && w.conflict.Load() != nil
}

// err returns the conflict that doomed this transaction, or nil.
func (w *writeCtx) err() error {
	if w == nil {
		return nil
	}
	if c := w.conflict.Load(); c != nil {
		return c
	}
	return nil
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
