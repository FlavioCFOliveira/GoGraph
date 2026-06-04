package cypher

// undo.go — in-memory transaction-undo log for write queries.
//
// A Cypher write query applies its mutations to the live in-memory graph
// EAGERLY, row by row, inside the write visibility barrier
// ([lpg.Graph.ApplyAtomically]) so that later rows of the same statement read
// the writes of earlier rows. If the statement then errors or panics on a later
// row, the WAL transaction and the secondary-index buffer roll back, but the
// rows already applied to the in-memory graph would otherwise remain — an
// in-memory-vs-durable divergence that violates Atomicity: concurrent
// [lpg.Graph.View] readers and the next query observe a partial transaction
// until the process restarts and recovery rebuilds the graph from the WAL.
//
// undoLog closes that gap. As each mutation is applied to the in-memory graph
// the mutator adapter records its inverse here; on a pipeline error or panic the
// executor replays the log in reverse, while the visibility barrier is still
// held, so no reader ever observes the partial transaction. On success the log
// is discarded without replaying.
//
// The log is owned by the per-statement mutator adapter. It is deliberately
// allocated lazily — a read query never opens one, and a write query allocates
// the backing slice only on its first recorded mutation — so the read hot path
// pays nothing. undoLog is NOT safe for concurrent use: a single physical
// operator tree owns exactly one adapter, and all recording happens on the
// executing goroutine under the write barrier.
//
// The design keeps the undo entries as closures captured against the underlying
// *lpg.Graph rather than a typed op enum, because (a) the set of invertible
// operations is small and stable, (b) each inverse needs a different pre-image
// (a prior property value, a captured edge weight, a label), and (c) the
// upcoming Bolt real-transaction work (multi-statement transactions that replay
// undo on ROLLBACK) can accumulate entries across statements by sharing one
// undoLog instance — no per-statement reset is required, only a single replay
// at rollback time.

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrUndoFailed is returned when the in-memory transaction-undo replay itself
// fails — an inverse operation panicked while rolling back a write query's
// eager mutations. It is the in-memory analogue of
// [txn.ErrCommittedNotApplied]: it signals that the graph may be left in a
// state that neither fully contains nor fully excludes the failed transaction,
// so the inconsistency is surfaced to the caller (and counted via metrics)
// rather than silently ignored. A WAL-backed store reconciles to the durable
// state on the next reopen; the in-memory engine has no such backstop, so the
// caller must treat the graph as suspect. Callers may match it with
// [errors.Is].
var ErrUndoFailed = errors.New("cypher: in-memory transaction undo failed; graph may be inconsistent until reopen")

// undoLog accumulates the inverse of every in-memory mutation a write query
// applies, so the executor can roll the live graph back on error or panic. The
// zero value is an empty, ready-to-use log; the backing slice is allocated on
// the first call to record.
type undoLog struct {
	// inverses holds one closure per recorded mutation, in application order.
	// replay runs them in reverse (LIFO) so dependent mutations unwind in the
	// correct order (e.g. a property set on a node is undone before the node
	// that carried it is tombstoned).
	inverses []func()
}

// record appends inv as the inverse of a just-applied mutation. inv must be
// non-nil and must, when invoked, leave the in-memory graph exactly as it was
// immediately before the mutation this entry inverts. record is a no-op when
// the log is nil, which lets callers thread an optional *undoLog without nil
// checks at every call site (a nil log means "this adapter does not track
// undo", e.g. a read-only adapter).
func (u *undoLog) record(inv func()) {
	if u == nil {
		return
	}
	u.inverses = append(u.inverses, inv)
}

// replay undoes every recorded mutation in reverse order and reports whether
// the rollback completed cleanly. After replay the log is emptied so a second
// call is a no-op (idempotent): the executor's error path and a defensive
// panic-path call must not double-apply inverses.
//
// Each inverse runs under its own recover guard. A panic in one inverse is
// logged with its stack, counted via the cypher.RunInTx.undo.errors metric, and
// does not stop the remaining inverses from running (best-effort rollback);
// replay then returns false so the caller surfaces [ErrUndoFailed]. The recover
// guard also guarantees replay never itself unwinds past the visibility
// barrier, which would leave visMu held forever.
func (u *undoLog) replay() (ok bool) {
	if u == nil || len(u.inverses) == 0 {
		return true
	}
	ok = true
	for i := len(u.inverses) - 1; i >= 0; i-- {
		inv := u.inverses[i]
		if inv == nil {
			continue
		}
		if !runUndo(inv) {
			ok = false
		}
	}
	// Empty the log: the backing array is released for GC and a repeat replay
	// (error path followed by a defensive panic-path call) is a clean no-op.
	u.inverses = nil
	return ok
}

// runUndo invokes a single inverse under a recover guard, returning false when
// it panicked. The recovered panic is logged with a stack trace and counted; it
// is never re-raised, so one corrupt inverse cannot abort the rollback of the
// rest of the transaction or escape the visibility barrier.
func runUndo(inv func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			cmetrics.IncCounter("cypher.RunInTx.undo.errors", 1)
			slog.Default().Error("cypher: panic while undoing in-memory write-query mutation",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	inv()
	return true
}

// replayUndoOnPanic is the deferred in-barrier panic handler for a write
// query's ApplyAtomically closure. When a panic is in flight it replays the
// transaction-undo log — rolling the live in-memory graph back to its
// pre-statement state WHILE the visibility barrier (visMu) is still held by the
// enclosing [lpg.Graph.ApplyAtomically] frame — and then RE-RAISES the same
// panic value so the outer write-path recover ([recoverWriteQueryPanic]) still
// performs its WAL-transaction rollback and converts the panic to
// [ErrInternalPanic]. Replaying here, rather than after ApplyAtomically
// returns, is the whole point of #1282: the deferred visMu.Unlock lives in
// ApplyAtomically's frame and runs only once this closure has fully unwound, so
// a recover sited here observes the lock still held and no concurrent
// [lpg.Graph.View] reader can ever see the partial transaction.
//
// When no panic is in flight it is a no-op, so the happy path is unaffected.
// undo may be nil (no mutations tracked); replay tolerates that.
func replayUndoOnPanic(undo *undoLog) {
	if r := recover(); r != nil {
		// Best-effort in-memory rollback; a failed inverse is logged and counted
		// inside replay. We re-raise regardless so the panic boundary still
		// releases the WAL single-writer mutex — never leave it held.
		undo.replay()
		panic(r)
	}
}

// wrapUndoFailure combines a primary error with [ErrUndoFailed] so the caller
// can match either with errors.Is. cause is the error that triggered the
// rollback (the pipeline error, or the converted panic); the returned error
// reports that the rollback that followed also failed. When cause is nil — the
// panic path sets the primary error separately — it returns ErrUndoFailed
// alone, still wrapped for a consistent "cypher:" reading.
func wrapUndoFailure(cause error) error {
	if cause == nil {
		return fmt.Errorf("cypher: %w", ErrUndoFailed)
	}
	return fmt.Errorf("%w (additionally: %w)", cause, ErrUndoFailed)
}
