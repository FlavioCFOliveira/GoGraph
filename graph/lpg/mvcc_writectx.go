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

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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
	// undoing marks the transaction as replaying its PHYSICAL undo log, during
	// which its writes are withdrawals of its own work rather than new updates —
	// so the doomed shortcut in [writeCtx.conflicts] must not refuse them
	// (rmp #2320).
	//
	// # The lost update this closes, measured
	//
	// GoGraph rolls a failed statement back physically: the undo log replays
	// inverse mutations through the same lpg mutators (cypher/undo.go, #1282).
	// Once those inverses carried the transaction — which they must, or the
	// inverse lands on a different commit record from the write it withdraws and
	// the chain no longer nets out — they inherited the doomed test, and a
	// transaction is doomed at exactly the moment its undo has to run. Every
	// inverse was refused, the rollback silently did nothing, and the writes the
	// statement had already applied stayed committed.
	//
	// That is not a hypothetical: examples/27_concurrent_txn's conservation
	// invariant broke by 1 694 729 cents at four writers, deterministically, with
	// `SET a.balance = …, b.balance = …` conflicting on the second property and
	// the first one never being withdrawn. Pinned by
	// TestWriteCtx_UndoOfADoomedTransactionIsNotRefused.
	//
	// # Why exempting the undo is sound, and not a hole
	//
	// The exemption is narrow: it removes ONLY the doomed shortcut, never the head
	// test. An inverse touches objects this transaction itself wrote, whose chain
	// head therefore carries this transaction's own id — which [mvcc.Visible]
	// classifies as visible, so the head test permits it on its own merits. If an
	// inverse ever reached an object another transaction owns, the head test would
	// still refuse it.
	//
	// ONE store falsifies that premise: the ADJACENCY, whose appends COMMUTE, so
	// another transaction's append can commit onto a node this transaction also
	// wrote — moving the head — without ever conflicting. The head test then
	// refuses this transaction's own inverse and the rolled-back edge leaks into
	// committed state (rmp #2445, found by the DST multi-session mode). The
	// adjacency conflict sites therefore carry their own undoing exemption; see
	// [adjVersions.checkAppend] and [adjVersions.noteExclusive].
	//
	// It is also what the prior art does. Memgraph's abort path walks the
	// transaction's own deltas and restores each object directly, without going
	// through PrepareForWrite at all — `InMemoryStorage::InMemoryAccessor::Abort`
	// in `src/storage/v2/inmemory/storage.cpp`, whereas every forward mutator in
	// `vertex_accessor.cpp` and `edge_accessor.cpp` opens with
	// `if (!PrepareForWrite(transaction_, …)) return SERIALIZATION_ERROR`. Read at
	// commit 572d5b4311a279de550522344a6f10d352d11c48 (branch master, 2026-08-03).
	//
	// Atomic for the same reason conflict is, and scoped by exactly one region:
	// [WriteTx.EnterUndo] / [WriteTx.ExitUndo] around the undo replay.
	undoing atomic.Bool
	// commitTS is a commit timestamp ALLOCATED BUT NOT YET PUBLISHED, or zero when
	// the transaction has not reached its durability point (rmp #2309).
	//
	// # Why the allocation moves before the fsync
	//
	// The MVCC clock is restored at recovery by DERIVING it from the WAL rather
	// than by trusting a persisted counter, which means the commit timestamp has to
	// be IN the durable record. It cannot be, if it is minted after the record is
	// written — and until this field existed it was: [Graph.endWrite] allocated it,
	// and endWrite runs from the deferred release, strictly after the WAL append
	// and fsync.
	//
	// So the order becomes allocate → encode → fsync → publish, which is
	// PostgreSQL's: the XID is assigned before XLogFlush, the flushed record
	// carries it, and only then is the commit marked visible. It is compatible with
	// [mvcc.Clock]'s documented allocate/store/publish order because an
	// allocated-but-unpublished timestamp is already a state the clock models — it
	// is what InFlightCommits counts.
	//
	// # The obligation this creates
	//
	// A timestamp that is allocated and then neither published nor abandoned STALLS
	// THE CONTIGUOUS FRONTIER PERMANENTLY: every later commit becomes invisible to
	// new readers and the commit log grows without bound. So every path out of a
	// transaction that allocated one must discharge it — publish on success,
	// [mvcc.Clock.AbandonCommitTS] on abort and on the versioned-nothing case. That
	// is why the discharge lives in endWrite, which every path reaches, rather than
	// beside each caller.
	//
	// Plain, not atomic: it is written by the committing goroutine before the fsync
	// and read by the same goroutine after it, with the WAL fsync between them.
	commitTS uint64
	// counts is the graph's write-side telemetry bank, carried rather than looked up
	// because [writeCtx] is not generic in the graph's type parameters and every
	// other piece of per-transaction state already travels with the write (rmp
	// #2312). Nil means "not counted", which is what a zero-value state used by a
	// test constructing a bare writeCtx gets.
	counts *mvcc.WriteCounters
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
	w := &writeCtx{startTS: startTS, txID: id, counts: &g.writeCounts}
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
// A single slot was the right size while the barrier admitted ONE write bracket at
// a time, so there was never a second live [writeCtx] to cache. Since rmp #2320
// removed the barrier from the ordinary write path the slot degrades in the only
// direction that is safe: concurrent writers that find it empty ALLOCATE, so
// contention costs an allocation and never correctness. Whether a per-P pool is now
// worth its constant is an open MEASUREMENT — the write-scaling gate clears 3x with
// the single slot, so it is not blocking, and it should be decided by benchmark
// rather than by reflex.
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
	w.counts = &g.writeCounts
	w.conflict.Store(nil)
	// EVERY mutable field must be reset here, because this state is RECYCLED. A
	// stale commitTS would be the worst of them: [Graph.endWrite] would publish a
	// timestamp belonging to a transaction that already committed, so two
	// transactions would share one instant and the second's writes would become
	// visible at the first's — a reader between them sees a state neither
	// transaction ever produced.
	//
	// This clear is DEFENCE IN DEPTH, not the load-bearing one, and the distinction
	// was established by removing it: every discharge path in [Graph.endWrite]
	// already zeroes the field (the publish after reading it, and
	// [Graph.abandonAllocatedCommitTS] on both failure exits), so the tests still
	// pass without this line and only fail when BOTH clears are gone. It stays
	// because the invariant it protects is "recycled state carries nothing", which
	// should hold whatever a future discharge path forgets — the failure mode is a
	// silent isolation violation, not a crash.
	//
	// TestAllocateCommitTS_RecycledStateCarriesNoStaleTimestamp pins the observable
	// property (no two transactions share an instant), which is what actually
	// matters; it is not a test of this line alone.
	w.commitTS = 0
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

// adjTx is this transaction as the ADJACENCY must receive it: a carried handle
// rather than an ambient lookup (rmp #2320).
//
// The adjacency needs two things from a write's transaction and neither can be
// resolved beside the write once two brackets overlap — the shared commit record
// its version points at, and the identity that owns a shard's copy-on-write
// builder. [mvcc.Tx] carries both, and the nil receiver — a direct Go-API
// mutation outside any transaction — yields the zero value, which adjlist reads
// as "not transactional" exactly as this package reads a nil *writeCtx.
//
// Threading the node side alone was not enough and the shortfall was measured,
// not reasoned about: `Graph.AddEdgeH`, `Graph.SetEdgeProperty` and
// `Graph.DelEdgeProperty` write through the adjacency and not through any
// node-side store, so a statement touching topology or a columnar edge property
// still split across two commit records with its label and property writes
// correctly threaded.
func (w *writeCtx) adjTx() mvcc.Tx {
	if w == nil {
		return mvcc.Tx{}
	}
	return mvcc.NewTx(&w.tx)
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
	//
	// UNLESS the transaction is unwinding: an inverse is the withdrawal of a write
	// this transaction already made, and refusing it leaves that write applied and
	// committed. See [writeCtx.undoing] for the lost update that measured. The head
	// test below still runs, so the exemption cannot let an inverse step on another
	// transaction's version.
	if w.conflict.Load() != nil && !w.undoing.Load() {
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
//
// store must be one of the [mvcc] store constants, so the per-store counter it is
// attributed to has bounded cardinality by construction; see [mvcc.StoreNodeLabels]
// and the rest.
func (w *writeCtx) conflictErr(store string, headTS uint64) error {
	c := mvcc.NewConflict(store, headTS, w.startTS, w.txID)
	if !w.conflict.CompareAndSwap(nil, c) {
		return w.conflict.Load()
	}
	// Counted HERE, on the WINNING CAS, and that placement is the accuracy of the
	// series (rmp #2312). A doomed transaction meets a conflict again on every write it
	// still attempts, so counting where conflicts are DETECTED would report refused
	// writes and scale with transaction size. The CAS succeeds exactly once per
	// transaction, so this counts DOOMED TRANSACTIONS — which is what a contention rate
	// means, and what an operator can divide by lpg.mvcc.commits.
	//
	// The per-store series is what says WHICH structure is contended; without it an
	// operator sees that the workload contends but not on what. The store is resolved
	// to a dense index from the closed set in [mvcc.ConflictStoreIndex], so the
	// cardinality is bounded by construction and an unrecognised name loses its
	// attribution rather than the count.
	//
	// Off the hot path by construction: once per conflicting transaction, never at all
	// on a workload that does not contend.
	idx := mvcc.ConflictStoreIndex(store)
	if w.counts != nil {
		w.counts.Conflict(idx)
	}
	metrics.IncCounter("lpg.mvcc.conflicts", 1)
	metrics.IncCounter(conflictStoreCounters[idx], 1)
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
//
// While the transaction is unwinding it reports FALSE, in lockstep with
// [writeCtx.conflicts]: every caller reads this to mean "the write I just
// attempted was refused", and during the undo replay no write is refused. The two
// must give the same answer or a caller would skip bookkeeping for a write that
// actually landed — [Graph.clearEdgePairState] is exactly such a caller. Whether
// the transaction can still COMMIT is a different question and is answered by
// [writeCtx.err], which this exemption does not touch.
func (w *writeCtx) doomed() bool {
	return w != nil && w.conflict.Load() != nil && !w.undoing.Load()
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
