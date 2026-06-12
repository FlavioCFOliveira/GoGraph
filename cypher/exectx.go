package cypher

// exectx.go — engine-level explicit (multi-statement) transactions.
//
// [Engine.RunInTx] is autocommit: each call is its own transaction that becomes
// durable-then-visible at the end of its own visibility-barrier window. An
// [ExplicitTx], by contrast, spans MANY statements: writes from every
// [ExplicitTx.Exec] in the handle accumulate and become durable together at
// [ExplicitTx.Commit], or unwind together at [ExplicitTx.Rollback]. It is the
// engine substrate for the Bolt BEGIN / RUN / COMMIT / ROLLBACK protocol
// (bolt/server), where a single client transaction issues several statements
// and expects all-or-nothing semantics across them.
//
// # The two wirings
//
// The handle works on both engine wirings (see [Engine]):
//
//   - WAL-backed ([NewEngineWithStore]). BeginTx opens one [txn.Tx]; that tx
//     holds the store's single-writer mutex from BEGIN until COMMIT/ROLLBACK, so
//     concurrent writers serialise behind the open transaction (Isolation). On
//     Commit the WAL is fsynced ONCE for the whole transaction (Durability); on
//     Rollback the WAL transaction is discarded, so a fresh recovery observes
//     none of the rolled-back writes.
//
//   - store-less ([NewEngine]). There is no WAL, so durability does not apply
//     (nothing is persisted). Write-write Isolation is still enforced: BeginTx
//     takes the engine's writer mutex ([Engine.writeMu]) and holds it until
//     COMMIT/ROLLBACK, so a concurrent writer — autocommit or another explicit
//     transaction — blocks until this one finishes. Rollback is honoured in full
//     via the in-memory undo log.
//
// In both wirings the writer serialisation is the OUTERMOST lock and visMu
// (taken inside [lpg.Graph.ApplyAtomically] by each Exec / Commit / Rollback) is
// nested inside it. This matches the WAL-backed store-mutex → visMu order, so the
// two wirings share one deadlock-free lock ordering.
//
// # Atomicity and the undo log
//
// Every Exec applies its mutations to the live in-memory graph EAGERLY, recording
// the inverse of each into ONE shared [undoLog] that accumulates across the whole
// transaction (the design hook documented in undo.go). Rollback replays that log
// in reverse, inside the visibility barrier, restoring the graph to its
// pre-transaction state; the secondary-index buffer and the WAL transaction roll
// back alongside it. Commit fsyncs the WAL once, commits the index buffer, and
// discards the undo log.
//
// # Isolation scope (read-uncommitted for readers)
//
// Because writes are applied eagerly, a concurrent [lpg.Graph.View] / [Engine.Run]
// READER can observe an open transaction's not-yet-committed writes
// (read-uncommitted for readers). The guarantees this type DOES provide are
// write-write Isolation (the writer mutex held BEGIN→COMMIT) and atomic Rollback
// (the undo log). Full reader isolation requires deferred apply / the lock-free
// per-shard snapshot tracked as the end-state in docs/isolation-design.md, and is
// deliberately out of scope here: deferred apply would contradict the eager-apply
// undo design (#1282) and carries high openCypher-TCK risk. See that document for
// the tracked end-state.
//
// # Concurrency contract
//
// An ExplicitTx is NOT safe for concurrent use: it is owned by a single caller
// (one Bolt session, whose message loop is single-threaded per connection) and
// its methods must be called in sequence. Distinct ExplicitTx handles, and an
// ExplicitTx alongside autocommit [Engine.RunInTx] calls on the same engine, ARE
// safe to use concurrently — they serialise on the writer mutex described above.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// ErrTxFinished is returned by [ExplicitTx.Exec], [ExplicitTx.Commit], and
// [ExplicitTx.Rollback] when the transaction has already been committed or
// rolled back. The handle holds no resources after it finishes — the writer
// serialisation is released and any WAL transaction is closed — so a stale call
// is rejected rather than acting on a released transaction. Matchable with
// [errors.Is].
var ErrTxFinished = errors.New("cypher: explicit transaction already finished")

// ErrStatementPipeline wraps a runtime pipeline error from [ExplicitTx.Exec].
// It signals that the query was compiled and ran to completion inside the
// visibility barrier but the execution pipeline failed (e.g. a constraint
// violation, a type error mid-pipeline, a validation error). The partial
// in-memory writes remain in the transaction's accumulated undo log; the
// caller (or the Bolt server layer) may decide whether to roll the whole
// transaction back.
//
// Callers that need to distinguish pipeline errors from compile-time or
// build errors use [errors.As] to unwrap this type; the wrapped error is the
// original pipeline error (matchable via [errors.Is] against sentinel errors
// such as [exec.ErrConstraintViolation]).
type ErrStatementPipeline struct{ Err error }

// Error implements the error interface.
func (e *ErrStatementPipeline) Error() string { return e.Err.Error() }

// Unwrap returns the underlying pipeline error so [errors.Is] and [errors.As]
// traversal works correctly.
func (e *ErrStatementPipeline) Unwrap() error { return e.Err }

// ExplicitTx is an open engine-level transaction spanning one or more
// statements. Obtain one from [Engine.BeginTx]; execute statements with
// [ExplicitTx.Exec] / [ExplicitTx.ExecAny]; finish with exactly one call to
// [ExplicitTx.Commit] or [ExplicitTx.Rollback].
//
// See the package file exectx.go for the full transaction, durability, and
// concurrency contract. In brief: writes accumulate and become durable together
// on Commit (WAL-backed) or unwind together on Rollback; the handle holds the
// engine's writer serialisation for its whole lifetime (write-write Isolation);
// it is NOT safe for concurrent use by multiple goroutines.
type ExplicitTx struct {
	eng *Engine

	// ctx bounds every statement run through this handle. It is the connection
	// context (optionally with a transaction timeout) supplied to BeginTx, so a
	// cancelled connection or an elapsed tx_timeout interrupts an in-flight Exec
	// and the writer mutex can never be held indefinitely.
	ctx context.Context

	// buf accumulates the secondary-index changes of every statement; committed
	// once on Commit, discarded on Rollback. Shared by all statement mutators.
	buf *exec.IndexBuffer

	// undo accumulates the inverse of every in-memory mutation across all
	// statements (the cross-statement accumulation hook in undo.go). Replayed in
	// reverse on Rollback; discarded on Commit. Shared by all statement mutators.
	undo *undoLog

	// walTx is the single WAL transaction backing the whole explicit transaction,
	// non-nil only on a WAL-backed engine. It holds the store's single-writer
	// mutex from BeginTx until Commit/Rollback. nil on a store-less engine, where
	// writer serialisation is the engine writer mutex instead (released via
	// unlockWriter).
	walTx *txn.Tx[string, float64]

	// unlockWriter releases the engine-level writer mutex on a store-less engine;
	// a no-op closure on a WAL-backed engine (the store mutex is released by
	// walTx instead). Called exactly once when the handle finishes.
	unlockWriter func()

	// finished is set by Commit/Rollback (and by a panic during Exec) so a second
	// finishing call, or any later Exec, is rejected with [ErrTxFinished] and the
	// writer serialisation is never released twice.
	finished bool
}

// BeginTx opens an explicit, multi-statement transaction bound to ctx and
// acquires the engine's writer serialisation: the store's single-writer mutex on
// a WAL-backed engine, or the engine writer mutex on a store-less engine. The
// caller MUST finish the returned handle with exactly one [ExplicitTx.Commit] or
// [ExplicitTx.Rollback]; until then the writer serialisation is held and
// concurrent writers block (write-write Isolation).
//
// ctx bounds every statement executed through the handle. Pass the connection
// context (optionally narrowed with a transaction timeout) so that a cancelled
// connection, a server shutdown, or an elapsed timeout interrupts an in-flight
// statement and guarantees the writer serialisation cannot be held forever.
//
// If ctx is already cancelled or its deadline has elapsed, BeginTx returns
// promptly without acquiring any lock, with an error wrapping the context error
// (matchable via [errors.Is] against [context.Canceled] /
// [context.DeadlineExceeded]).
//
// See exectx.go for the full transaction and concurrency contract, including the
// documented read-uncommitted-for-readers isolation scope.
func (e *Engine) BeginTx(ctx context.Context) (*ExplicitTx, error) {
	defer cmetrics.Time("cypher.BeginTx")()
	if err := checkContext(ctx); err != nil {
		cmetrics.IncCounter("cypher.BeginTx.errors", 1)
		return nil, err
	}
	// Acquire the engine writer serialisation FIRST (store-less only; no-op when
	// WAL-backed). It is the outermost lock; visMu nests inside it in every Exec.
	unlockWriter := e.lockWriter()

	tx := &ExplicitTx{
		eng:          e,
		ctx:          ctx,
		buf:          &exec.IndexBuffer{},
		undo:         &undoLog{},
		unlockWriter: unlockWriter,
	}
	// Open the WAL transaction on a WAL-backed engine. Store.BeginCtx takes the
	// store's single-writer lock (so the store-less writer mutex above is a
	// no-op in this wiring) and holds it until Commit/Rollback. The acquire is
	// context-aware: under write contention a caller whose ctx is cancelled or
	// whose deadline elapses gets back the context error instead of blocking on
	// the lock for the holder's full duration (task #1301). On that error the
	// store-less writer mutex acquired above must be released before returning,
	// or it would leak; the per-statement context bound is otherwise enforced in
	// Exec and by the deadline the Bolt layer derives onto ctx.
	if e.store != nil {
		walTx, beginErr := e.store.BeginCtx(ctx)
		if beginErr != nil {
			unlockWriter()
			cmetrics.IncCounter("cypher.BeginTx.errors", 1)
			return nil, beginErr
		}
		tx.walTx = walTx
	}
	cmetrics.IncCounter("cypher.BeginTx.opened", 1)
	return tx, nil
}

// Exec runs one statement inside the open transaction and returns a materialised
// [Result]. The statement's writes are applied eagerly and accumulate in the
// transaction; they are NOT made durable or finalised here — that happens once,
// at [ExplicitTx.Commit]. Closing the returned Result releases only its own
// iterator state; it never commits or rolls the transaction back.
//
// A DDL statement (CREATE/DROP INDEX or CONSTRAINT) is rejected: schema changes
// are not transactional in this engine and must be issued outside an explicit
// transaction (autocommit). A read-only statement is permitted and simply
// observes the transaction's current state.
//
// A statement that raises a runtime error is returned directly as the error
// return of Exec. The per-statement writes remain in the accumulated undo log,
// so the caller (the Bolt session) can roll the whole transaction back via
// [ExplicitTx.Rollback] after inspecting the error. A statement that panics is
// converted to an error wrapping
// [ErrInternalPanic]; the in-memory writes of the whole transaction are rolled
// back inside the visibility barrier, the writer serialisation is released, and
// the handle is marked finished (a subsequent Rollback is then a no-op).
//
// Exec returns [ErrTxFinished] if the transaction has already been committed or
// rolled back, or if ctx (the BeginTx context) is already done.
func (tx *ExplicitTx) Exec(query string, params map[string]expr.Value) (res *Result, err error) {
	defer cmetrics.Time("cypher.ExplicitTx.Exec")()
	if tx.finished {
		return nil, ErrTxFinished
	}
	// A panic anywhere in the statement is converted to ErrInternalPanic by this
	// boundary. Registered before the work below so it observes a panic raised in
	// build, drain, or commit-under-barrier. On a panic the in-memory undo was
	// already replayed inside the barrier (replayUndoOnPanic); here we release the
	// writer serialisation, roll back the WAL transaction, and mark the handle
	// finished so it cannot be used or double-released. recoverExecPanic does all
	// of that and sets err.
	defer tx.recoverExecPanic(&err)
	if err := checkContext(tx.ctx); err != nil {
		return nil, err
	}
	// DDL is not transactional: reject it inside an explicit transaction rather
	// than silently autocommitting a schema change in the middle of a tx.
	if ir.IsDDL(query) {
		return nil, fmt.Errorf("cypher: DDL statement %q is not allowed inside an explicit transaction", query)
	}

	queryReg := newNowAwareRegistry(tx.eng.reg, time.Now())

	entry, err := tx.eng.parseAndAnalyse(query)
	if err != nil {
		return nil, err
	}
	if entry.semaErr != nil {
		return nil, entry.semaErr
	}
	plan := entry.plan
	if err := tx.eng.checkParamTypes(plan, params); err != nil {
		return nil, err
	}

	// Build the mutator over the SHARED buf / walTx / undo so this statement's
	// mutations accumulate into the transaction. The adapter only captures
	// references; no graph reads happen until execUnderBarrier runs it under visMu.
	var mutator exec.GraphMutator
	if tx.walTx != nil {
		mutator = &walMutatorAdapter{g: tx.eng.g, tx: tx.walTx, buf: tx.buf, undo: tx.undo}
	} else {
		mutator = &lpgMutatorAdapter{g: tx.eng.g, buf: tx.buf, undo: tx.undo}
	}

	r, buildErr := tx.eng.execUnderBarrier(tx.ctx, plan, queryReg, params, mutator, tx.buf, tx.undo, tx.walTx, false)
	if buildErr != nil {
		return nil, fmt.Errorf("cypher: build plan: %w", buildErr)
	}
	if stmtErr := r.Err(); stmtErr != nil {
		return nil, &ErrStatementPipeline{Err: stmtErr}
	}
	return r, nil
}

// ExecAny is the [ExplicitTx.Exec] variant taking params as map[string]any,
// converting Go native values to [expr.Value] via [BindParams].
func (tx *ExplicitTx) ExecAny(query string, params map[string]any) (*Result, error) {
	converted, err := BindParams(params)
	if err != nil {
		return nil, err
	}
	return tx.Exec(query, converted)
}

// Commit makes the whole transaction durable and visible, then releases the
// writer serialisation. On a WAL-backed engine the WAL is fsynced exactly ONCE
// for every statement's accumulated writes (durable-then-visible, #1281) and the
// secondary-index buffer is committed; on a store-less engine the writes are
// already visible and Commit simply finalises the index buffer. The accumulated
// undo log is discarded. After Commit the handle is finished.
//
// Commit runs the finalisation inside the visibility barrier so that, on a
// WAL-backed engine, the fsync happens-before the index commit and no concurrent
// reader can observe a committed-but-not-durable state. If the WAL fsync fails,
// the transaction is rolled back instead (in-memory undo replayed, index and WAL
// rolled back) and the fsync error is returned wrapping it: a transaction whose
// durability could not be guaranteed is reported as failed, never acknowledged.
//
// Commit returns [ErrTxFinished] if the transaction was already committed or
// rolled back.
func (tx *ExplicitTx) Commit() (err error) {
	defer cmetrics.Time("cypher.ExplicitTx.Commit")()
	if tx.finished {
		return ErrTxFinished
	}
	// A panic during the in-barrier finalisation must still release the writer
	// serialisation and roll back the WAL transaction; convert it to an error.
	defer tx.recoverFinishPanic(&err)
	defer tx.release()

	var walErr error
	_ = tx.eng.g.ApplyAtomically(func() error {
		// Durability before visibility: fsync the WAL FIRST so the whole
		// transaction is durable the instant its writes are allowed to remain
		// observable past the barrier (#1281). Only then commit the secondary
		// indexes. If the fsync fails, roll everything back inside the barrier so
		// the non-durable transaction never stays visible.
		if tx.walTx != nil {
			if werr := tx.walTx.CommitWALOnly(); werr != nil {
				cmetrics.IncCounter("cypher.ExplicitTx.wal.commitErrors", 1)
				walErr = werr
				tx.rollbackInBarrierLocked()
				return nil
			}
		}
		if tx.buf != nil {
			tx.buf.Commit(tx.eng.g.IndexManager())
		}
		// Drop the undo log: the transaction is keeping its writes.
		tx.undo = nil
		return nil
	})
	if walErr != nil {
		return fmt.Errorf("cypher: commit WAL: %w", walErr)
	}
	cmetrics.IncCounter("cypher.ExplicitTx.committed", 1)
	return nil
}

// Rollback unwinds the whole transaction: it replays the accumulated in-memory
// undo log in reverse inside the visibility barrier (restoring the graph to its
// pre-transaction state), rolls back the secondary-index buffer, rolls back the
// WAL transaction (WAL-backed only, so a fresh recovery observes none of the
// writes), and releases the writer serialisation. After Rollback the handle is
// finished.
//
// Rollback is best-effort and total: it always releases the writer serialisation
// and finishes the handle, even if an inverse operation fails. It returns
// [ErrUndoFailed] (wrapped) when the in-memory undo replay itself failed — the
// graph may then be inconsistent until reopen, which a WAL-backed engine
// reconciles to the durable state and a store-less engine cannot. It returns
// [ErrTxFinished] if the transaction was already committed or rolled back.
func (tx *ExplicitTx) Rollback() (err error) {
	defer cmetrics.Time("cypher.ExplicitTx.Rollback")()
	if tx.finished {
		return ErrTxFinished
	}
	defer tx.recoverFinishPanic(&err)
	defer tx.release()

	undoOK := true
	_ = tx.eng.g.ApplyAtomically(func() error {
		undoOK = tx.rollbackInBarrierLocked()
		return nil
	})
	cmetrics.IncCounter("cypher.ExplicitTx.rolledBack", 1)
	if !undoOK {
		return wrapUndoFailure(nil)
	}
	return nil
}

// rollbackInBarrierLocked replays the accumulated undo log, rolls back the index
// buffer, and rolls back the WAL transaction. It MUST be called inside the
// visibility barrier ([lpg.Graph.ApplyAtomically]) so the rolled-back writes
// never become observable to a concurrent reader. It returns whether the
// in-memory undo replay completed cleanly. Shared by Rollback and by Commit's
// fsync-failure branch. The undo runs first so the secondary indexes are dropped
// only after the graph entries they describe are gone; the WAL transaction is
// rolled back last (it holds no in-memory state). [txn.Tx.Rollback] is idempotent
// against an already-finished transaction.
//
// After undo replay, the constraint registry's UNIQUE value-sets are reseeded
// from the restored graph so that any values recorded during the rolled-back
// statements do not produce phantom reservations (#1342).
func (tx *ExplicitTx) rollbackInBarrierLocked() (undoOK bool) {
	undoOK = true
	if tx.undo != nil && !tx.undo.replay() {
		undoOK = false
	}
	tx.undo = nil
	// Reseed the constraint registry from the restored graph. Runs after undo
	// so the graph is back to its pre-transaction state before the value-sets
	// are rebuilt.
	if tx.eng.constraintReg != nil {
		reseedConstraintsInsideBarrier(tx.eng.constraintReg, tx.eng.g)
	}
	if tx.buf != nil {
		tx.buf.Rollback()
	}
	if tx.walTx != nil {
		_ = tx.walTx.Rollback() // release store single-writer mutex; in-memory state already restored
	}
	return undoOK
}

// release finishes the handle and releases the engine writer serialisation
// exactly once. On a store-less engine unlockWriter unlocks [Engine.writeMu]; on
// a WAL-backed engine it is a no-op (the store mutex is released by walTx's own
// Commit/Rollback). Idempotent via the finished flag.
func (tx *ExplicitTx) release() {
	if tx.finished {
		return
	}
	tx.finished = true
	if tx.unlockWriter != nil {
		tx.unlockWriter()
	}
}

// recoverExecPanic is the deferred recover boundary for [ExplicitTx.Exec]. The
// in-memory undo for the whole transaction was already replayed inside the
// barrier by replayUndoOnPanic before the panic reached here; this handler then
// rolls back the WAL transaction, releases the writer serialisation, marks the
// handle finished (so a subsequent Rollback is a no-op against the now-empty
// undo log), and converts the panic to an error wrapping [ErrInternalPanic].
//
// errp must be a pointer: the deferred recover writes through Exec's named error
// return on Exec's stack frame, so this is structurally required, not the style
// choice gocritic's ptrToRefParam assumes.
//
//nolint:gocritic // ptrToRefParam: errp must be the caller's named-return pointer
func (tx *ExplicitTx) recoverExecPanic(errp *error) {
	if r := recover(); r != nil {
		if tx.walTx != nil {
			_ = tx.walTx.Rollback() //nolint:errcheck // rollback error is not actionable while converting a panic
		}
		tx.release()
		convertQueryPanic(r, errp, "cypher.ExplicitTx.Exec", "cypher.ExplicitTx.Exec.panics")
	}
}

// recoverFinishPanic is the deferred recover boundary for [ExplicitTx.Commit] and
// [ExplicitTx.Rollback]. release runs via its own defer (registered after this
// one, so it executes first on unwind and the writer serialisation is freed
// regardless); this handler only converts a panic raised during the in-barrier
// finalisation to an error wrapping [ErrInternalPanic].
//
// errp must be a pointer for the same named-return reason as [recoverExecPanic].
//
//nolint:gocritic // ptrToRefParam: errp must be the caller's named-return pointer
func (tx *ExplicitTx) recoverFinishPanic(errp *error) {
	if r := recover(); r != nil {
		convertQueryPanic(r, errp, "cypher.ExplicitTx.finish", "cypher.ExplicitTx.finish.panics")
	}
}
