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
//   - WAL-backed ([NewEngineWithStore]). BeginTx opens one [txn.Tx]; that tx is a
//     registered writer from BEGIN until COMMIT/ROLLBACK, which since rmp #2306
//     excludes no other writer — it only holds a quiesce off. On
//     Commit the WAL is fsynced ONCE for the whole transaction (Durability); on
//     Rollback the WAL transaction is discarded, so a fresh recovery observes
//     none of the rolled-back writes.
//
//   - store-less ([NewEngine]). There is no WAL, so durability does not apply
//     (nothing is persisted). BeginTx acquires NO writer serialisation at all
//     since rmp #2306: the engine's writer mutex used to be taken here and held
//     until COMMIT/ROLLBACK, which made a paused client block every other writer in
//     the process — and, because the acquire on the autocommit side was not
//     context-aware, block it with the blocked caller's deadline IGNORED (measured
//     at ten minutes against a 200 ms deadline). Write-write isolation now comes
//     from MVCC: two transactions overlap and a genuine collision is REFUSED by
//     per-object first-updater-wins detection (rmp #2300) rather than prevented.
//     Rollback is honoured in full via the in-memory undo log.
//
// # What still serialises, and what owns retiring it
//
// ONE transaction-lifetime hold remains: the graph's visibility barrier, taken
// exclusively by BeginTx and held to COMMIT/ROLLBACK. It is now the ONLY thing
// that makes a paused client block other writers, and retiring it is rmp #2305.
//
// The store's capacity-one semaphore was the other one; rmp #2306 retired it, so
// [txn.Store.BeginCtx] now only registers the transaction as an admitted writer
// for the quiesce accounting.
//
// Schema changes are a separate matter and keep a lock of their own
// ([Engine.schemaMu]): a DDL scans, validates and only then registers, so the
// exclusive barrier — held for the registration alone — cannot make the sequence
// atomic against a second DDL.
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
// # Isolation scope (snapshot isolation for readers)
//
// [Engine.BeginTx] acquires the graph's transaction-visibility write lock
// ([lpg.Graph.LockBarrier]) for the whole lifetime of the transaction — from BEGIN
// until COMMIT or ROLLBACK. That excludes other WRITERS, and nothing else: since
// rmp #2290 a concurrent [Engine.Run] reader does not take the barrier at all. It
// pins a start timestamp and resolves every store as of that instant, so it
// observes the state before this transaction began, in full, for its whole
// duration — and is not delayed by it. Writes within the transaction itself
// (across multiple [ExplicitTx.Exec] calls) are visible to the subsequent
// statements in the same transaction because they share the live in-memory graph
// and read it with no snapshot. (task #1412, isolation option b; strengthened by
// rmp #2290.)
//
// # Operational contract: a write transaction blocks other WRITERS
//
// Because [Engine.BeginTx] holds the engine-wide visibility barrier
// ([lpg.Graph.LockBarrier], an exclusive lock) for the ENTIRE lifetime of the
// transaction, every concurrent WRITE is blocked for as long as it stays open.
// That window spans not just the statements' execution but also the client
// network round-trips and think-time BETWEEN BEGIN, each RUN/PULL, and COMMIT.
//
// It no longer blocks READERS. It used to, and that was the module's worst
// availability defect: a long read plus one writer collapsed short-read
// throughput 50× and gave a 4.5 µs point query a 1m36s worst-case latency,
// because Go's sync.RWMutex parks every reader arriving behind a queued writer
// (rmp #2274). MVCC removed it — the same measurement now reads 1.89× and
// 3.973 ms.
//
// Callers should still keep write transactions SHORT and not hold one open
// across user or network think-time, because a held one still serialises every
// other WRITER. Prefer autocommit for single-statement writes. The reader tail
// under a held write transaction is characterised by
// BenchmarkReaderLatencyUnderHeldWriteTx.
//
// # Concurrency contract
//
// An ExplicitTx is NOT safe for concurrent use: it is owned by a single caller
// (one Bolt session, whose message loop is single-threaded per connection) and
// its methods must be called in sequence. Distinct ExplicitTx handles, and an
// ExplicitTx alongside autocommit [Engine.RunInTx] calls on the same engine, ARE
// safe to use concurrently — one handle is driven by one goroutine, and two handles
// may now be open at once (rmp #2306).

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

// ErrTxPoisoned is returned by [ExplicitTx.Commit] when a prior
// [ExplicitTx.Exec] call returned an [ErrStatementPipeline] error. A poisoned
// transaction cannot be committed — its partial writes must be unwound by
// calling [ExplicitTx.Rollback] instead. Matchable with [errors.Is].
var ErrTxPoisoned = errors.New("cypher: transaction poisoned by a prior failed Exec statement — call Rollback")

// ErrWriteInReadOnlyTx is returned by [ExplicitTx.Exec] when a writing clause
// (CREATE/MERGE/SET/REMOVE/DELETE/DETACH) or a DDL statement (CREATE/DROP INDEX
// or CONSTRAINT) is issued inside a read-only explicit transaction opened with
// [Engine.BeginReadTx]. A read-only transaction holds neither the engine's
// writer serialisation, the visibility barrier, nor a WAL transaction, so a
// write has no lock, no barrier, and no durable log to record into; it is
// rejected BEFORE any execution so no state change can occur. Matchable with
// [errors.Is].
var ErrWriteInReadOnlyTx = errors.New("cypher: write or DDL statement not allowed in a read-only transaction")

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
// on Commit (WAL-backed) or unwind together on Rollback; the handle holds both
// the engine's writer serialisation and the graph's transaction-visibility write
// lock (visMu) for its whole lifetime — write-write Isolation for writers; a
// concurrent reader takes no barrier and observes snapshot isolation against
// the state before this transaction began, without waiting for it (rmp #2290);
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

	// cbuf accumulates the relationship count-store deltas and dirty markings of
	// every statement (#2082); applied to the engine's count store once on Commit
	// (after the WAL fsync), discarded on Rollback. Shared by all statement
	// mutators exactly like buf, so counts flip atomically with the graph. nil on
	// a read-only handle.
	cbuf *exec.CountBuffer

	// undo accumulates the inverse of every in-memory mutation across all
	// statements (the cross-statement accumulation hook in undo.go). Replayed in
	// reverse on Rollback; discarded on Commit. Shared by all statement mutators.
	undo *undoLog

	// touched accumulates, across every statement, the node keys the transaction
	// created, labelled, or stripped a property from, for the commit-time NOT
	// NULL existence check (#1754). It is nil unless the engine had at least one
	// existence constraint active when BeginTx ran, so a transaction with none
	// records nothing. Shared by all statement mutators; checked once at Commit.
	touched *touchedNodes

	// walTx is the single WAL transaction backing the whole explicit transaction,
	// non-nil only on a WAL-backed engine. It holds the store's writer
	// mutex from BeginTx until Commit/Rollback — the remaining half of rmp #2306.
	// nil on a store-less engine, which since rmp #2306 acquires NO writer
	// serialisation at all: concurrency control there is MVCC and nothing else.
	walTx *txn.Tx[string, float64]

	// finished is set by Commit/Rollback (and by a panic during Exec) so a second
	// finishing call, or any later Exec, is rejected with [ErrTxFinished] and the
	// writer serialisation is never released twice.
	finished bool

	// failed is set when an Exec call returns an [ErrStatementPipeline] error.
	// A poisoned transaction cannot be committed (Commit returns [ErrTxPoisoned]);
	// the caller must call Rollback to unwind the partial writes accumulated in
	// the undo log.
	failed bool

	// readOnly is true when the handle was opened by [Engine.BeginReadTx]. A
	// read-only transaction acquires NONE of the writer serialisation, the
	// visibility barrier, or a WAL transaction: walTx, buf and undo are nil and
	// barrierHeld is false. Each [ExplicitTx.Exec]
	// rejects any writing/DDL statement with [ErrWriteInReadOnlyTx] before
	// execution and routes a read through the engine's concurrent read path
	// ([Engine.Run]), which pins its own per-statement snapshot — so reads
	// observe per-statement SNAPSHOT isolation (a stable instant within a
	// statement, a fresh one between statements) and never block, or are
	// blocked by, other readers or writers. Commit and Rollback on a read-only
	// handle are teardown-only no-ops.
	readOnly bool

	// barrierHeld is true when BeginTx has acquired the graph's
	// transaction-visibility write lock (visMu) for the whole lifetime of this
	// transaction (task #1412, isolation option b). When true, each Exec routes
	// its in-barrier work through [lpg.Graph.ApplyInsideLocked] (which assumes
	// the lock is already held) instead of [lpg.Graph.ApplyAtomically] (which
	// would re-acquire and panic on re-entrancy). Commit and Rollback release
	// visMu via [lpg.Graph.UnlockBarrier] after their own in-barrier work.
	barrierHeld bool

	// view is the read instant every statement of a READ-ONLY handle executes
	// at: one snapshot, opened by [Engine.BeginReadTx] and registered with the
	// reclamation horizon for the whole lifetime of the transaction (rmp #2307).
	// It is what makes an explicit read transaction SNAPSHOT-ISOLATED rather
	// than read-committed — without it each Exec opened a fresh instant and a
	// commit landing between two statements became visible mid-transaction.
	//
	// nil on a write handle, which needs no separate view: it holds visMu
	// exclusively from BEGIN to COMMIT, so nothing else can publish a commit
	// while it runs, and it must read the present to see its own writes.
	//
	// The horizon slot it occupies is released exactly once, in [release], so
	// every exit path — Commit, Rollback, a panic in Exec, a panic in
	// Commit/Rollback — returns it. A slot that is never returned pins the
	// watermark for the life of the process.
	view *pinnedView
}

// BeginTx opens an explicit, multi-statement transaction bound to ctx. It acquires
// NO writer serialisation — concurrency control is MVCC alone since rmp #2306 —
// but it does take the graph's visibility barrier exclusively, which until
// rmp #2305 retires it still blocks concurrent writers for the transaction's
// lifetime. The caller MUST finish the returned handle with exactly one
// [ExplicitTx.Commit] or [ExplicitTx.Rollback].
//
// ctx bounds every statement executed through the handle. Pass the connection
// context (optionally narrowed with a transaction timeout) so that a cancelled
// connection, a server shutdown, or an elapsed timeout interrupts an in-flight
// statement and guarantees the writer serialisation cannot be held forever.
//
// ctx also bounds the ACQUISITION, not only the statements that follow. BeginTx
// takes three things in order — the writer serialisation, the WAL transaction,
// and the visibility barrier — and every one of them honours ctx, so a caller
// whose deadline elapses while queued behind another writer or behind an
// in-flight reader gets the context error rather than a transaction it is no
// longer entitled to. When that happens NOTHING is left held and no handle is
// returned. Before rmp #2174 two of the three acquisitions ignored ctx entirely:
// the round-3 audit measured a 50 ms deadline returning after 601 ms, and after
// 11.60 s under load, both times with err=nil and a live transaction, which also
// made the Bolt tx_timeout inert at BEGIN.
//
// If ctx is already cancelled or its deadline has elapsed, BeginTx returns
// promptly without acquiring any lock, with an error wrapping the context error
// (matchable via [errors.Is] against [context.Canceled] /
// [context.DeadlineExceeded]).
//
// The error is returned within the deadline plus a small, bounded margin: the
// margin is one scheduling hop, not the holder's remaining tenure. See
// internal/ctxlock for why a queued lock acquisition cannot simply be abandoned
// and what is done instead.
//
// See exectx.go for the full transaction and concurrency contract, including the
// isolation scope: concurrent readers do NOT block while this transaction is
// open, and observe the state before it began until it commits (task #1412,
// strengthened by rmp #2290).
func (e *Engine) BeginTx(ctx context.Context) (*ExplicitTx, error) {
	defer cmetrics.Time("cypher.BeginTx").Stop()
	if err := checkContext(ctx); err != nil {
		cmetrics.IncCounter("cypher.BeginTx.errors", 1)
		return nil, err
	}
	// NO writer serialisation is acquired (rmp #2306). BEGIN used to take the
	// engine's writer mutex here and hold it until COMMIT or ROLLBACK, which made a
	// paused client block every other writer in the process. Concurrency control is
	// MVCC: two transactions overlap, and a genuine collision is refused by
	// per-object detection rather than prevented by exclusion.
	tx := &ExplicitTx{
		eng:  e,
		ctx:  ctx,
		buf:  &exec.IndexBuffer{},
		cbuf: &exec.CountBuffer{},
		undo: &undoLog{},
	}
	// Allocate the touched-node set only when an existence constraint is active,
	// so a transaction with none records nothing (#1754).
	if e.constraintReg != nil && e.constraintReg.HasAnyNotNull() {
		tx.touched = &touchedNodes{}
	}
	// Open the WAL transaction on a WAL-backed engine. Store.BeginCtx registers this
	// transaction as an admitted writer until Commit/Rollback. It no longer excludes
	// anybody (rmp #2306 retired the capacity-one semaphore), so the only thing that
	// can make this call block is a quiesce in progress — and it stays
	// context-aware for that case: a caller whose ctx is cancelled or whose deadline
	// elapses gets the context error back instead of waiting out the quiesce
	// (task #1301, rmp #2174). Nothing acquired before this point needs releasing on
	// the error path.
	if e.store != nil {
		walTx, beginErr := e.store.BeginCtx(ctx)
		if beginErr != nil {
			cmetrics.IncCounter("cypher.BeginTx.errors", 1)
			return nil, beginErr
		}
		tx.walTx = walTx
	}
	// Hold the visibility barrier for the whole transaction so concurrent readers
	// never observe uncommitted writes (task #1412, isolation option b). The
	// barrier is acquired AFTER walTx is open (store lock outer, visMu inner) to
	// preserve the established lock ordering. The acquire honours ctx (rmp #2174):
	// before that, the wait ignored the caller's deadline entirely and the audit
	// measured a 232x overrun that still returned err=nil.
	//
	// THIS IS THE LAST TRANSACTION-LIFETIME EXCLUSIVE LOCK, and retiring it is
	// rmp #2305. It is what still makes a paused client block every other writer;
	// rmp #2306 removed the engine's writer mutex from this path, so the barrier is
	// now the only thing left holding that property.
	if berr := tx.eng.g.LockBarrierCtx(ctx); berr != nil {
		if tx.walTx != nil {
			_ = tx.walTx.Rollback() // nothing was written; discard the empty txn
		}
		cmetrics.IncCounter("cypher.BeginTx.errors", 1)
		return nil, berr
	}
	tx.barrierHeld = true
	cmetrics.IncCounter("cypher.BeginTx.opened", 1)
	return tx, nil
}

// BeginReadTx opens a read-only explicit transaction bound to ctx. Unlike
// [Engine.BeginTx], it acquires NO writer serialisation, opens NO WAL
// transaction, and does NOT hold the visibility barrier: a read-only
// transaction has no durability obligation and never serialises behind, or
// blocks, a concurrent writer. The caller MUST still finish the returned handle
// with exactly one [ExplicitTx.Commit] or [ExplicitTx.Rollback]; on a read-only
// handle both are teardown-only no-ops (they release nothing, since nothing was
// acquired).
//
// Every statement run through [ExplicitTx.Exec] on the handle:
//
//   - is rejected with [ErrWriteInReadOnlyTx] BEFORE execution if it contains a
//     writing clause ([QueryHasWritingClause]) or is DDL ([ir.IsDDL]) — the
//     rejection is what keeps the lock-free read path safe, since a write would
//     otherwise run with no writer lock, no barrier, and no WAL; and
//   - otherwise runs through the engine's concurrent read path ([Engine.Run]),
//     taking its OWN per-statement [lpg.Graph.View] snapshot. Reads therefore
//     observe READ-COMMITTED isolation across the statements of the transaction
//     (each RUN sees the latest committed state, matching Neo4j's default), and
//     run fully in parallel with other readers and writers.
//
// If ctx is already cancelled or its deadline has elapsed, BeginReadTx returns
// promptly with an error wrapping the context error (matchable via [errors.Is]
// against [context.Canceled] / [context.DeadlineExceeded]).
func (e *Engine) BeginReadTx(ctx context.Context) (*ExplicitTx, error) {
	defer cmetrics.Time("cypher.BeginReadTx").Stop()
	if err := checkContext(ctx); err != nil {
		cmetrics.IncCounter("cypher.BeginReadTx.errors", 1)
		return nil, err
	}
	cmetrics.IncCounter("cypher.BeginReadTx.opened", 1)
	return &ExplicitTx{
		eng:      e,
		ctx:      ctx,
		readOnly: true,
		// One read instant for the whole transaction, registered with the
		// reclamation horizon here and released exactly once in release()
		// (rmp #2307). Taken LAST, after every failure return above, so a
		// BeginReadTx that returns an error never leaves a slot pinned.
		view: &pinnedView{snap: e.g.BeginRead()},
		// buf, undo, walTx remain nil; barrierHeld stays false.
	}, nil
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
	defer cmetrics.Time("cypher.ExplicitTx.Exec").Stop()
	if tx.finished {
		return nil, ErrTxFinished
	}
	// Read-only transaction: reject any writing/DDL statement BEFORE execution
	// (no writer lock, no barrier, no WAL backs this handle), and route every
	// permitted read through the engine's concurrent read path so it takes its
	// own per-statement snapshot (snapshot isolation within a statement, a fresh
	// instant between statements). This path never touches buf/undo/walTx (all
	// nil) or the visibility barrier.
	if tx.readOnly {
		if err := checkContext(tx.ctx); err != nil {
			return nil, err
		}
		// SHOW CONSTRAINTS / SHOW INDEXES are classified as DDL for dispatch but
		// are pure reads (schema introspection): permit them on a read-only
		// transaction. Every schema-WRITING DDL statement (CREATE/DROP
		// INDEX|CONSTRAINT) stays rejected, since it would run with no writer
		// lock, no barrier, and no WAL. SHOW then routes through Run, which
		// dispatches it to the read-only runShow* handler.
		if queryHasWritingClause(query) || (ir.IsDDL(query) && !ir.IsShow(query)) {
			return nil, ErrWriteInReadOnlyTx
		}
		// At the TRANSACTION's instant, not a fresh one (rmp #2307). Passing
		// tx.view is the whole difference between snapshot isolation and
		// read-committed here: runRead executes at that snapshot and, because
		// the handle owns it, does not release it when the statement ends.
		return tx.eng.runRead(tx.ctx, query, params, tx.view)
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
	// SHOW CONSTRAINTS / SHOW INDEXES are pure reads (schema introspection), so
	// they are permitted inside an explicit transaction. They read the current
	// committed schema — schema-writing DDL cannot run inside a transaction, so
	// the committed schema is exactly what this transaction observes — via the
	// engine's concurrent read path (its own snapshot); they touch neither this
	// transaction's writer lock, WAL, nor undo log.
	if ir.IsShow(query) {
		return tx.eng.Run(tx.ctx, query, params)
	}
	// Other DDL is not transactional: reject it inside an explicit transaction
	// rather than silently autocommitting a schema change in the middle of a tx.
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
	if err := checkParamPresence(entry.paramRefs, params); err != nil {
		return nil, err
	}
	if err := checkParamTypesCached(entry, params); err != nil {
		return nil, err
	}

	// Build the mutator over the SHARED buf / walTx / undo so this statement's
	// mutations accumulate into the transaction. The adapter only captures
	// references; no graph reads happen until execUnderBarrier runs it under visMu.
	var mutator exec.GraphMutator
	// Pre-set cs and cbuf to the engine's count store and the handle's SHARED count
	// buffer so every statement's count deltas accumulate together and the handle
	// flushes them once at Commit (#2082), mirroring the shared index buffer.
	if tx.walTx != nil {
		mutator = &walMutatorAdapter{g: tx.eng.g, tx: tx.walTx, buf: tx.buf, undo: tx.undo, touched: tx.touched, cbuf: tx.cbuf, eng: tx.eng}
	} else {
		mutator = &lpgMutatorAdapter{g: tx.eng.g, buf: tx.buf, undo: tx.undo, touched: tx.touched, cbuf: tx.cbuf, eng: tx.eng}
	}

	// Route through ApplyInsideLockedTx when the barrier is held for the whole tx
	// lifetime (barrierHeld=true), since ApplyAtomicallyTx would panic on re-entry.
	//
	// The *Tx forms (rmp #2304) hand the statement the transaction it runs as, so
	// its reads resolve through that transaction rather than through the graph's
	// ambient slot. Both stay EXCLUSIVE here: an explicit transaction holds the
	// barrier from BEGIN to COMMIT, and retiring THAT hold is rmp #2305, not this
	// task. Only the autocommit path became shared.
	applyFn := tx.eng.g.ApplyAtomicallyTx
	if tx.barrierHeld {
		applyFn = tx.eng.g.ApplyInsideLockedTx
	}
	r, buildErr := tx.eng.execUnderBarrier(tx.ctx, plan, queryReg, params, mutator, tx.buf, tx.undo, tx.walTx, false, applyFn, tx.touched)
	if buildErr != nil {
		return nil, fmt.Errorf("cypher: build plan: %w", buildErr)
	}
	if stmtErr := r.Err(); stmtErr != nil {
		tx.failed = true
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
// rolled back, and [ErrTxPoisoned] if a prior [ExplicitTx.Exec] call returned
// an [ErrStatementPipeline] error (call [ExplicitTx.Rollback] instead).
func (tx *ExplicitTx) Commit() (err error) {
	defer cmetrics.Time("cypher.ExplicitTx.Commit").Stop()
	if tx.finished {
		return ErrTxFinished
	}
	// Read-only transaction: teardown only. No writer lock, no barrier, and no
	// WAL transaction were acquired, so there is nothing to make durable or
	// release beyond marking the handle finished (release() guards on
	// barrierHeld is false here). A second call is ErrTxFinished.
	if tx.readOnly {
		tx.release()
		cmetrics.IncCounter("cypher.ExplicitTx.committed", 1)
		return nil
	}
	if tx.failed {
		return ErrTxPoisoned
	}
	// A panic during the in-barrier finalisation must still release the writer
	// serialisation and roll back the WAL transaction; convert it to an error.
	defer tx.recoverFinishPanic(&err)
	defer tx.release()

	var walErr error
	var notNullErr error
	applyFn := tx.eng.g.ApplyAtomically
	if tx.barrierHeld {
		applyFn = tx.eng.g.ApplyInsideLocked
	}
	_ = applyFn(func() error {
		// Commit-time NOT NULL existence check (#1754, ACID Consistency). Runs
		// FIRST, inside the barrier, BEFORE the WAL fsync, so a node left in its
		// final committed state carrying a constrained label but lacking the
		// required property rejects the WHOLE transaction atomically: the
		// accumulated in-memory undo is replayed and the index/WAL rolled back,
		// exactly like the fsync-failure branch below. touched is nil (check a
		// no-op) unless the engine had an existence constraint active at BeginTx.
		if nnErr := tx.touched.checkNotNullConstraints(tx.eng.constraintReg, tx.eng.g); nnErr != nil {
			cmetrics.IncCounter("cypher.ExplicitTx.constraint.notNullViolations", 1)
			notNullErr = nnErr
			if undoOK := tx.rollbackInBarrierLocked(); !undoOK {
				notNullErr = wrapUndoFailure(notNullErr)
			}
			return nil
		}
		// Durability before visibility: fsync the WAL FIRST so the whole
		// transaction is durable the instant its writes are allowed to remain
		// observable past the barrier (#1281). Only then commit the secondary
		// indexes. If the fsync fails, roll everything back inside the barrier so
		// the non-durable transaction never stays visible.
		if tx.walTx != nil {
			if werr := tx.walTx.CommitWALOnly(); werr != nil {
				cmetrics.IncCounter("cypher.ExplicitTx.wal.commitErrors", 1)
				walErr = werr
				if undoOK := tx.rollbackInBarrierLocked(); !undoOK {
					walErr = wrapUndoFailure(walErr)
				}
				return nil
			}
		}
		if tx.buf != nil {
			tx.buf.Commit(tx.eng.g.IndexManager())
		}
		// Relationship count-store (#2082): apply the whole transaction's count
		// deltas after the WAL fsync, alongside the index buffer, so counts flip
		// atomically with the graph writes they describe.
		if tx.cbuf != nil {
			recordCountCommit(tx.eng.countStore, tx.cbuf) // observability (#2087)
			tx.cbuf.Commit(tx.eng.countStore)
		}
		// Drop the undo log: the transaction is keeping its writes.
		tx.undo = nil
		return nil
	})
	if notNullErr != nil {
		return notNullErr
	}
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
	defer cmetrics.Time("cypher.ExplicitTx.Rollback").Stop()
	if tx.finished {
		return ErrTxFinished
	}
	// Read-only transaction: teardown only (see Commit). Nothing to unwind —
	// no undo log, index buffer, WAL transaction, or held barrier — so this
	// just finishes the handle. A second call is ErrTxFinished.
	if tx.readOnly {
		tx.release()
		cmetrics.IncCounter("cypher.ExplicitTx.rolledBack", 1)
		return nil
	}
	defer tx.recoverFinishPanic(&err)
	defer tx.release()

	undoOK := true
	applyFn := tx.eng.g.ApplyAtomically
	if tx.barrierHeld {
		applyFn = tx.eng.g.ApplyInsideLocked
	}
	_ = applyFn(func() error {
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
	// NO constraint reseed (rmp #2321). The undo replay above already released this
	// transaction's own value-set reservations, because each was journaled as an
	// inverse when it was taken (cypher/exec/constraint_journal.go). Rebuilding the
	// value-sets from the graph — which is what stood here — also destroyed
	// CONCURRENT writers' reservations, since a rebuild cannot see a commit that is
	// not yet durable.
	if tx.buf != nil {
		tx.buf.Rollback()
	}
	// Discard the count-store deltas: nothing was applied (no undo needed, #2082).
	if tx.cbuf != nil {
		tx.cbuf.Rollback()
	}
	if tx.walTx != nil {
		_ = tx.walTx.Rollback() // deregister the store writer; in-memory state already restored
	}
	return undoOK
}

// release finishes the handle and releases the engine writer serialisation
// exactly once. When barrierHeld is true it releases the transaction-visibility
// write lock (visMu via [lpg.Graph.UnlockBarrier]).
//
// There is no engine writer serialisation left to release: rmp #2306 retired it, so
// the acquisition order it used to be the outer half of no longer exists. On a
// WAL-backed engine the store's writer admission is still released by walTx's own
// Commit/Rollback. Idempotent via the finished flag.
func (tx *ExplicitTx) release() {
	if tx.finished {
		return
	}
	tx.finished = true
	// Return the transaction's horizon slot before anything else (rmp #2307).
	// The finished guard above makes this exactly-once across every exit path,
	// and clearing view makes a double release unrepresentable rather than
	// merely unlikely: EndRead on an already-returned slot would corrupt the
	// watermark for every other reader.
	if tx.view != nil {
		tx.eng.g.EndRead(tx.view.snap)
		tx.view = nil
	}
	if tx.barrierHeld {
		tx.eng.g.UnlockBarrier()
		tx.barrierHeld = false
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
