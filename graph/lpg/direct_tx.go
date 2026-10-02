package lpg

// direct_tx.go — a direct Go-API write runs as an IMPLICIT single-operation
// transaction (rmp #2947).
//
// # The defect
//
// A direct mutator — [Graph.SetNodeProperty] and the rest, called outside any
// transaction — used to pass a nil *writeCtx, which every store reads as "never
// conflicts" and stamps with a fresh commit timestamp the instant it is written.
// That is right against COMMITTED history and wrong against an UNCOMMITTED
// version: displacing a version another transaction has not committed is a
// dirty write (P0 in Berenson et al., "A Critique of ANSI SQL Isolation Levels",
// 1995), and when that transaction rolled back, its undo restored the pre-image
// over the direct write, after the caller had been told it succeeded.
//
// Refusing at each store with a check made before the write (the first fix)
// was not enough. A direct write that touches several stores — removing a node,
// an edge or every edge out of a node — took its checks under other locks than
// the writes they guarded, and a peer slipping in between made it half-applied:
// the adjacency changed and a side store refused, or the node died with a peer's
// rolled-back label removal still owed.
//
// # The design
//
// Every direct mutator runs its ordinary transactional body, the `…Info` form a
// statement inside a transaction runs, through [Graph.direct], in one of two
// ways:
//
//   - The versioning substrate is disarmed (tests only): with a nil transaction,
//     as before.
//   - Otherwise: as an implicit transaction of its own. It claims, conflict-tests
//     and cross-checks exactly as a statement does, so a refusal is decided by
//     the same machinery, and on any refusal it ABORTS: its record is marked
//     aborted and every version it wrote is withdrawn before the call returns
//     ([Graph.abortWake]), so the call changed nothing. On success it commits at
//     one instant, so a reader never observes part of a multi-store direct write.
//
// # A direct write never joins a bracket (rmp #2947, audit F7)
//
// That holds while an exclusive bracket — [Graph.ApplyAtomically],
// [Graph.ApplyAtomicallyTx], [Graph.LockBarrierCtx] — is open, too. A direct
// write used to join the open exclusive bracket's transaction, because the
// bracket owns the ambient slot. The module cannot tell the bracket's own
// goroutine from an unrelated one, so a direct write from another goroutine
// joined as well. It was acknowledged, and then it was lost when the bracket
// aborted. A write that belongs to a bracket now says so: the bracket's
// function writes through [Graph.Writer] over the [WriteTx] that
// [Graph.ApplyAtomicallyTx] or [Graph.ApplyInsideLockedTx] hands it. A direct
// call made inside a bracket is its own implicit transaction. It commits at its
// own instant, and it conflicts with the bracket's uncommitted writes like any
// other transaction's.
//
// # Why the implicit transaction reads the latest committed state
//
// Its start timestamp is the top of the commit space ([implicitStartTS]), so it
// sees every committed version and conflicts with exactly the versions that are
// not committed: another transaction's in-flight or aborted ones. A direct write
// has no snapshot of its own to be stale against — it is one operation at the
// present instant — so refusing a version committed after some earlier instant
// would refuse writes that never raced with anything. It is
// [Graph.LatestViewOf]'s view, and like that view it needs no horizon slot: the
// newest version of every chain is never reclaimed.
//
// # The lightweight commit
//
// An implicit transaction is the state a statement's transaction carries, minus
// everything a single operation has no use for: no schema-gate hold (a direct
// write never took one), no horizon slot, no ambient slot, no commit applier and
// no writer telemetry. Its state is recycled through [Graph.implicitCtx]; its id
// is drawn from the ordinary sequence with [mvcc.ImplicitTxBit] set; its commit
// record is allocated only when it writes a version, and published with the same
// allocate-stamp-publish sequence [Graph.endWrite] uses.
//
// # Waiting for another direct write
//
// A refusal caused by ANOTHER implicit transaction is retried with a short
// bounded backoff; one caused by a durable store commit's bounded transaction
// ([Graph.ApplyDurable]) parks on that commit's end in arrival order, because
// the commit holds its claims across an fsync (see txwait.go, rmp #2965 round
// 5). Neither runs caller code between its first write and its end, so it
// always finishes, and two writes to the same node would otherwise refuse each
// other merely for being concurrent. Both are bounded by [directWaitBudget]. A
// refusal caused by an explicit transaction is returned at once: that
// transaction may be held open across client round-trips, and these methods
// take no context to bound a wait by. The retry runs from the top, after the refused attempt has aborted,
// so it never waits holding a lock.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrDirectWriteConflict is returned by a direct mutator of [Graph] — one called
// outside any transaction — when another transaction holds an uncommitted write
// on what it would change (rmp #2947): a node's properties, labels or existence,
// an edge's adjacency entry, or an edge's per-handle, per-ordinal or overflow
// records. The refused call changes nothing.
//
// It is RETRYABLE: the blocking transaction is either in flight or aborted and
// being withdrawn, and the same call succeeds once it has committed or rolled
// back. The returned error also wraps a [*mvcc.Conflict] naming the store and the
// blocking version, so [errors.As] recovers the detail and
// errors.Is(err, mvcc.ErrSerializationConflict) is true; its StartTS and TxID are
// zero.
//
// This holds inside an exclusive bracket ([Graph.ApplyAtomically] and its
// siblings) as well: a direct call is never part of the bracket's transaction,
// so the bracket's own uncommitted writes refuse it like any other
// transaction's. A bracket's function writes through [Graph.Writer] instead.
//
// It is distinct from [ErrIndexedRawWrite], which is NOT retryable: that refusal
// is decided by the graph's index catalogue and holds until the indexes are
// dropped, whereas this one is decided by one object's version chain and clears
// on its own. Callers should match both with [errors.Is].
var ErrDirectWriteConflict = errors.New("lpg: direct write refused: another transaction " +
	"holds an uncommitted write on the same object; retry once it commits or rolls back")

// implicitStartTS is the start timestamp of every implicit transaction: the top
// of the commit-timestamp space, so [mvcc.Visible] admits every committed version
// and [mvcc.Conflicts] refuses exactly the uncommitted ones.
const implicitStartTS = mvcc.TxIDBase - 1

// directWaitBudget bounds how long one direct write keeps retrying while other
// implicit transactions refuse it. Such a transaction runs no caller code and
// always finishes, so the budget is a backstop against starvation, not a wait
// for a particular peer: it is exhausted only by sustained saturation of one
// object, which is then answered with the typed refusal rather than an unbounded
// wait. 50 ms was measured too short: under the race detector, twenty-four direct
// writers on four undirected pairs starved one writer past it
// (TestDirectEdge_OppositeDirectionsDoNotDeadlock, which expects no refusal when
// no explicit transaction is open).
const directWaitBudget = time.Second

// direct runs op as one direct write, in the transaction this file's comment
// describes, and returns its outcome: nil when it applied, the refusal wrapped
// with [ErrDirectWriteConflict] when a conflict refused it, and any other error
// op returned as is. On any error from an implicit transaction nothing op wrote
// remains.
//
// op must be re-runnable: an attempt refused by another implicit transaction is
// aborted and op runs again.
func (g *Graph[N, W]) direct(op func(tx *writeCtx) error) error {
	var start time.Time
	var inherited waitQueue
	woken := false
	// A panic out of op must not strand what this writer was handed: the
	// waiters behind it, and the hand-off token that defers fresh writers.
	defer func() {
		if woken {
			g.txWait.handoffDone()
		}
		if inherited.head != nil {
			g.txWait.handOff(inherited)
		}
	}()
	if g.mvccArmed {
		g.txWait.yieldToHandoffs() // see [txWaitTable.yieldToHandoffs]
	}
	for attempt := 0; ; attempt++ {
		if !g.mvccArmed {
			return op(nil)
		}
		err := g.runImplicit(op)
		if woken {
			g.txWait.handoffDone()
			woken = false
		}
		// A queue inherited from the bounded commit this write waited for is
		// handed on as soon as the one-operation attempt has ended, whatever its
		// outcome: an implicit transaction holds its versions for no longer than
		// the attempt itself (see txwait.go).
		if inherited.head != nil {
			g.txWait.handOff(inherited)
			inherited = waitQueue{}
		}
		if err == nil {
			return nil
		}
		var c *mvcc.Conflict
		if !errors.As(err, &c) {
			return err
		}
		if implicitBlocker(c.HeadTS) {
			if attempt == 0 {
				start = time.Now()
			}
			if time.Since(start) < directWaitBudget {
				if mvcc.IsBoundedTx(c.HeadTS) {
					// Park on the blocking commit's end, FIFO, holding nothing.
					// A direct mutator takes no context, so only the budget bounds
					// the wait and wait cannot return a context error.
					if q, ok, wk, _ := g.txWait.wait(context.Background(), c.HeadTS, start.Add(directWaitBudget)); ok {
						inherited, woken = q, wk
						continue
					}
					return directRefusal(c)
				}
				directBackoff(attempt)
				continue
			}
		}
		return directRefusal(c)
	}
}

// errImplicitPanic is the cause an implicit transaction is aborted with when its
// operation panics; the panic itself continues to the caller.
var errImplicitPanic = errors.New("lpg: direct write panicked")

// runImplicit runs op as one attempt of an implicit transaction and returns how
// it ended, as [Graph.finishImplicit] reports it.
//
// A panic in op is a programmer error and is not recovered: it continues to the
// caller. The transaction is still SETTLED on the way out — aborted, so every
// version it wrote is withdrawn and its record is not left in flight, where it
// would refuse every later write on what it touched. The settlement runs in a
// deferred call that does not recover, which is what lets the panic go on.
func (g *Graph[N, W]) runImplicit(op func(tx *writeCtx) error) error {
	w := g.acquireImplicit()
	settled := false
	defer func() {
		if !settled {
			// The panic in flight is the outcome; the abort's own result is
			// errImplicitPanic echoed back, and there is no caller to return it to.
			_ = g.finishImplicit(w, errImplicitPanic)
		}
	}()
	err := op(w)
	settled = true
	return g.finishImplicit(w, err)
}

// acquireImplicit opens an implicit transaction: recycled state, a fresh id with
// [mvcc.ImplicitTxBit] set, and the latest-committed snapshot.
func (g *Graph[N, W]) acquireImplicit() *writeCtx {
	w, _ := g.implicitCtx.Get().(*writeCtx)
	if w == nil || !w.tx.Reusable() {
		w = &writeCtx{}
	}
	id := g.mvccClock.NextImplicitTxID()
	w.tx.Arm(id)
	w.startTS, w.txID = implicitStartTS, id
	w.snap.startTS, w.snap.txID, w.snap.slot = implicitStartTS, id, 0
	w.snap.memo = &w.snapMemo
	if len(w.snapMemo.verdict) != 0 {
		clear(w.snapMemo.verdict)
	}
	w.conflict.Store(nil)
	w.undoing.Store(false)
	w.commitTS, w.allocRec, w.applier, w.counts = 0, nil, nil, nil
	w.abandon = false
	return w
}

// finishImplicit ends the implicit transaction w whose operation returned err:
// it aborts it on any error — err itself, or the conflict a void store recorded
// on w — and commits it otherwise, then recycles w. It returns the error the
// operation ended with.
//
// The abort withdraws every version the transaction wrote before returning (see
// [Graph.abortWake]), which is what makes a refused direct write change nothing.
// The commit allocates, stamps and publishes the record in the order
// [Graph.endWrite] documents, and charges the versions to the reclamation debt.
func (g *Graph[N, W]) finishImplicit(w *writeCtx, err error) error {
	info, versions := w.tx.Retract()
	if err == nil {
		err = w.err()
	}
	if err != nil {
		if info != nil {
			g.abortWake(versions-g.abortRecord(&w.tx, info), &w.tx)
		}
		g.implicitCtx.Put(w)
		return err
	}
	if info != nil {
		ts := g.mvccClock.AllocateFor(info, true)
		info.Commit(ts)
		g.mvccClock.PublishCommit(info, ts)
	}
	g.implicitCtx.Put(w)
	// The versions this transaction made, and those raw adjacency writes made
	// with no transaction since the last charge, which nothing else drains now
	// that a direct write no longer goes through [Graph.reclaimAfterDirectWrite]
	// with a nil transaction.
	g.chargeReclaimDebt(versions + g.stamp.TakeUntracked())
	return nil
}

// implicitBlocker reports whether a version with effective timestamp head is one
// a direct write may wait out: another implicit transaction's or a bounded
// transaction's ([mvcc.BoundedTxBit], a durable store commit, see
// [Graph.ApplyDurable]), in flight, or an aborted one, which its abort is
// withdrawing. Neither kind runs caller code before it ends.
func implicitBlocker(head uint64) bool {
	return mvcc.IsImplicitTx(head) || mvcc.IsBoundedTx(head) || head == mvcc.AbortedTS
}

// directBackoff pauses before attempt+1 of a direct write: a yield for the first
// four attempts, which is all a writer behind one other implicit transaction
// usually needs, then a sleep doubling from 1 µs to at most 256 µs. The cap was
// chosen when [directWaitBudget] was 50 ms, and these figures date from then;
// they were not re-measured after the budget was raised to 1 s. Measured on
// one hot object (one node property, one hub's appends), in single one-second
// runs: no refusal at 8, 64 or 256 concurrent writers, at most 0.19 % at 1024,
// and no call slower than the 50 ms budget. A cap of 64 µs refused less at 1024
// writers and cost 43 % of the hub's append throughput at 64; a cap of 1 ms
// refused 0.4 % at 1024.
func directBackoff(attempt int) {
	if attempt < 4 {
		runtime.Gosched()
		return
	}
	time.Sleep(time.Microsecond << min(attempt-4, 8))
}

// directRefusal returns the refusal for a direct write blocked by the version c
// names, and counts it.
func directRefusal(c *mvcc.Conflict) error {
	metrics.IncCounter("lpg.mvcc.direct_write_conflicts", 1)
	return fmt.Errorf("%w: %w", ErrDirectWriteConflict, mvcc.NewConflict(c.Store, c.HeadTS, 0, 0))
}

// adjErr records a conflict an adjacency write reported for tx on the
// transaction and returns it; any other error is returned as is. The adjacency
// refuses a transactional write that would build on an implicit transaction's
// uncommitted entry, and an implicit one that would build on any other
// transaction's ([adjlist.AdjList] direct_conflict.go), and that refusal must
// doom the transaction like every other store's.
func adjErr(tx *writeCtx, err error) error {
	if err == nil || tx == nil {
		return err
	}
	var c *mvcc.Conflict
	if errors.As(err, &c) {
		return tx.conflictErr(mvcc.StoreAdjacency, c.HeadTS)
	}
	return err
}
