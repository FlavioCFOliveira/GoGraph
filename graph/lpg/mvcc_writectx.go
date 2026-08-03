package lpg

// mvcc_writectx.go — the per-transaction state a write carries (rmp #2301).
//
// Design: docs/design-per-transaction-write-state.md.
//
// # What was per-GRAPH, and why that stopped working
//
// Three pieces of write-side state were single fields precisely because there
// was exactly one writer: the write stamp holding "the commit record of the
// write currently under the barrier", the adjacency's commit window, and the
// re-entrancy guard's single writer goroutine id (audit findings E3, E4, E16).
// The write stamp's Begin said so in its own doc: "the caller must be the only
// writer for the window's duration … so Begin never overwrites a live record."
//
// E3 is the one that hurts, and it is NOT a data race — every field was atomic,
// so -race is silent on it. A second Begin destroyed the first transaction's
// window, so its record was never published and its versions kept an in-flight
// transaction id for ever: invisible to every reader, unreclaimable by every
// reclaimer, reported by nothing. Measured, with the lost record and the stolen
// version count, in the design doc §1.
//
// All three are now per-transaction: the stamping state on [mvcc.TxState] here,
// the adjacency's window on a per-shard owner token, and the guard on a set of
// goroutine ids.
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

// writeCtx is one write transaction's identity AND its whole mutable write
// state, as carried into the stores it touches.
//
// It is passed by pointer, and its identity fields are never mutated after
// construction, so two concurrent writers hold two distinct values and neither
// can observe the other's — which is the whole point, and what a per-graph field
// could not give. The state that does change during a transaction — its commit
// record, its version count, its conflict flag — changes only on the
// transaction's own value.
//
// The zero value is not usable; obtain one from [Graph.beginWriteCtx] or
// [Graph.acquireWriteCtx], or take the nil pointer, which means "not in a
// transaction".
type writeCtx struct {
	// tx is this transaction's stamping state: the commit record every version it
	// writes points at, and how many of them there are. Publishing the
	// transaction stays ONE atomic store however many stores it spans — that
	// indirection is the reason the record is heap allocated and it must not
	// regress into a per-delta timestamp.
	//
	// BY VALUE, and the pointer to it is what [mvcc.WriteStamp] publishes, so a
	// bracket needs one object rather than two. The record inside it is still
	// allocated lazily, so a transaction that versions nothing allocates nothing.
	tx mvcc.TxState
	// snap is the read view this transaction resolves through, held inline so
	// [Graph.writerView] can hand out &w.snap without allocating.
	snap Snapshot
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

// beginWriteCtx opens per-transaction write state, NOT published on the graph's
// slot: it is carried by the caller.
//
// The order matters and is the same as [Graph.beginLabelTx]'s: the start
// timestamp is read BEFORE the transaction id is minted, so a transaction can
// never see a commit that happened after it began.
func (g *Graph[N, W]) beginWriteCtx() *writeCtx {
	startTS := g.readTS()
	id := g.nextTxID()
	w := &writeCtx{startTS: startTS, txID: id}
	w.snap = Snapshot{startTS: startTS, txID: id}
	w.tx.Arm(id)
	return w
}

// acquireWriteCtx takes per-transaction write state for a barrier bracket,
// recycled when possible.
//
// # Why recycled at all
//
// The state has to be per-transaction — that is audit finding E3 and the whole
// of rmp #2301 — and opening a bracket has to allocate nothing, which
// TestBarrierGuard_ApplyAtomicallyAllocatesNothing has asserted since #2168.
// A field on the Graph satisfies the second and fails the first; a fresh
// allocation per bracket satisfies the first and fails the second. Recycling is
// what satisfies both.
//
// # Why ONE slot and not a sync.Pool — measured
//
// sync.Pool is the reflex for this shape and it was tried first. It cost too
// much for what an empty bracket is: BenchmarkBarrier_ApplyAtomically went from
// 19.1 ns/op to 31.3 ns/op, +64 %, all of it Get/Put bookkeeping
// (runtime_procPin plus the per-P poolLocal walk) on a bracket whose entire job
// is two atomic stores. One atomic slot does the same work in a Swap and a
// Store.
//
// A single slot is the right size because the barrier admits ONE write bracket at
// a time, so there is never a second live [writeCtx] to cache. When rmp #2304
// removes the barrier the slot degrades in the only direction that is safe:
// concurrent writers that find it empty ALLOCATE, so contention costs an
// allocation and never correctness. That is the point at which a per-P pool
// becomes worth its constant, and it should be re-measured then rather than
// assumed now.
//
// # Why a recycled state can refuse to be reused
//
// [mvcc.TxState.Arm] refuses a state that still holds a record, which
// happens when an unsynchronised public-API mutator allocated one into it after
// its owner had finished. Reusing it would publish that stranded version with the
// WRONG transaction, so the cached value is dropped and a fresh one taken.
// Dropping it is safe: it is unreachable from anything but the stranded version,
// which keeps its own reference.
//
// The state is returned ARMED, ready for [mvcc.WriteStamp.Publish]. Arming here
// rather than in Publish is what makes the refusal impossible to ignore: nothing
// but this function can reach a state that is out of the free slot and not yet
// published, so the Arm here cannot lose a race and needs no retry.
func (g *Graph[N, W]) acquireWriteCtx(startTS, txID uint64) *writeCtx {
	w := g.writeCtxFree.Swap(nil)
	if w == nil || !w.tx.Arm(txID) {
		w = &writeCtx{}
		w.tx.Arm(txID)
	}
	w.startTS, w.txID = startTS, txID
	w.snap = Snapshot{startTS: startTS, txID: txID}
	w.conflict.Store(nil)
	return w
}

// releaseWriteCtx offers bracket state back for reuse.
//
// The caller must have closed the window ([mvcc.WriteStamp.End]) and released
// the horizon slot first, and must not reference w afterwards. Nothing else
// retains it: a version holds the *[mvcc.CommitInfo] inside, which is a separate
// object and is never recycled, and the adjacency holds a transaction TOKEN
// rather than a pointer.
func (g *Graph[N, W]) releaseWriteCtx(w *writeCtx) {
	g.writeCtxFree.Store(w)
}

// record returns the commit record to stamp a version with, allocating this
// transaction's record if the version asking is its first, or nil when there is
// no transaction.
//
// Every call site creates a version, so the count [mvcc.TxState.Ensure] keeps is
// exactly the transaction's version count — which is what charges the
// reclamation debt at commit. Before rmp #2301 a threaded transaction's versions
// were charged to NOTHING: the same accounting hole rmp #2289 had already had to
// close for untransacted writes.
func (w *writeCtx) record() *commitInfo {
	if w == nil {
		return nil
	}
	return w.tx.Ensure()
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
