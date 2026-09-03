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
// # What serialises an explicit transaction: NOTHING
//
// As of rmp #2305 there is no transaction-lifetime hold of any kind. The three that
// existed are all gone: the engine's writer mutex and the store's capacity-one
// semaphore (rmp #2306 — [txn.Store.BeginCtx] now only registers the transaction as
// an admitted writer for the quiesce accounting), and the graph's visibility barrier,
// which BeginTx took EXCLUSIVELY and held to COMMIT/ROLLBACK.
//
// What an open transaction holds instead is its own unpublished COMMIT RECORD and one
// reclamation-horizon slot. Each statement takes the schema barrier SHARED for its own
// duration ([lpg.Graph.ApplyInVersionedTx]) and releases it before returning, so
// between statements — across every client round-trip — nothing is held.
//
// The consequence for an ABANDONED transaction is worth stating, because it changed
// in kind rather than degree. It used to be an outage: one paused client blocked every
// other writer in the process. It is now a memory cost: no version the transaction
// could still read is reclaimable while it lives. Both are worth reclaiming, which is
// what [server.Options.MaxTxIdleTime] does, but only the first was an availability
// failure.
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
// [Engine.BeginTx] opens one write transaction — a commit record — and publishes it
// exactly once at COMMIT. That single publication is the transaction's commit instant:
// every version its statements wrote becomes visible together, and a rolled-back
// transaction's versions never become visible at all. Atomicity comes from the record,
// which is what MVCC is for; until rmp #2305 it came from an exclusive lock held for
// the transaction's whole lifetime.
//
// A concurrent [Engine.Run] reader takes no lock (rmp #2290) and pins a start
// timestamp, resolving every store as of that instant, so it observes the state before
// this transaction began, in full, for its whole duration — and is neither delayed by
// it nor able to see its uncommitted work. Writes within the transaction itself
// (across multiple [ExplicitTx.Exec] calls) ARE visible to its own subsequent
// statements, because a transaction reads its own versions through the
// ts == txID branch of the visibility rule. (task #1412, isolation option b;
// strengthened by rmp #2290 and rmp #2305.)
//
// # Operational contract: a write transaction blocks NOBODY
//
// An open explicit write transaction blocks neither readers nor writers.
//
// It used to block both, and each was fixed separately. Readers were freed by MVCC
// (rmp #2274 measured the defect: a long read plus one writer collapsed short-read
// throughput 50× and gave a 4.5 µs point query a 1m36s worst-case latency, because
// Go's sync.RWMutex parks every reader arriving behind a queued writer; the same
// measurement now reads 1.89× and 3.973 ms). WRITERS were freed by rmp #2305, which
// retired the exclusive barrier hold spanning BEGIN, every RUN/PULL, COMMIT and all
// the client think-time between them. Gated end-to-end against the official driver by
// bolt/server's TestE2E_TwoExplicitWriteTransactionsOverlap and
// TestE2E_AnIdleExplicitTransactionDoesNotStallAnotherWriter.
//
// Callers should still keep write transactions SHORT, for a different reason than
// before: an open transaction pins the reclamation horizon, so no version it could
// still read is freed while it lives. The cost of an abandoned transaction is version
// memory, not other clients' progress. Prefer autocommit for single-statement writes.
// The reader tail under a held write transaction is characterised by
// BenchmarkReaderLatencyUnderHeldWriteTx.
//
// # Concurrency contract
//
// An ExplicitTx is NOT safe for concurrent use: it is owned by a single caller
// (one Bolt session, whose message loop is single-threaded per connection) and
// its methods must be called in sequence. Distinct ExplicitTx handles, and an
// ExplicitTx alongside autocommit [Engine.RunInTx] calls on the same engine, ARE
// safe to use concurrently — one handle is driven by one goroutine, and two handles
// may now be open at once AND both make progress (rmp #2305, rmp #2306). A collision
// between two open handles on the same object is refused at the conflicting statement
// with a retriable serialization error; see
// cypher.TestExplicitTx_ConflictSurfacesAtTheConflictingStatement.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
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

// ErrSerializationConflict is the typed, RETRIABLE write-write conflict error:
// the transaction attempted to displace a version another transaction wrote
// after this one began (first-committer-wins; the node is the unit of
// conflict). It can surface from a statement or from [ExplicitTx.Commit] —
// including for a write a void primitive recorded silently, per the doomed-tx
// contract (rmp #2354) — and the failed transaction applied NOTHING: roll it
// back and retry it whole. Matchable with [errors.Is].
//
// It is the [mvcc.ErrSerializationConflict] sentinel re-exported at the
// surface clients import (rmp #2437), the same classification Neo4j gives its
// TransientError.
var ErrSerializationConflict = mvcc.ErrSerializationConflict

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
// on Commit (WAL-backed) or unwind together on Rollback; the handle holds NO LOCK
// across its lifetime — each statement takes the schema barrier SHARED for its own
// duration and nothing is held between statements (rmp #2305), and the engine's
// former writer serialisation (Engine.writeMu) no longer exists at all (rmp #2306).
// Write-write isolation therefore comes from per-object conflict detection against
// this transaction's snapshot (rmp #2300), not from exclusion. A concurrent reader
// takes no barrier and observes snapshot isolation against the state before this
// transaction began, without waiting for it (rmp #2290); the handle itself is NOT
// safe for concurrent use by multiple goroutines.
type ExplicitTx struct {
	eng *Engine

	// sess is the caller's session when this handle was opened through one, and nil
	// otherwise (rmp #2329). It is what records this transaction's commit instant, so
	// the caller's NEXT operation waits for the frontier to reach it. A nil session
	// keeps the engine's sessionless contract: snapshot isolation with no
	// cross-transaction promise. See [Session].
	sess *lpg.Session[string, float64]

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
	// conTxn accumulates, across every statement, the UNIQUE values this
	// transaction has RELEASED and not yet committed (rmp #2366). Commit applies
	// them to the shared value-sets inside the barrier; Rollback drops them, which
	// costs nothing because a deferred release never touched anything shared. See
	// [exec.ConstraintTxn] for the interleaving that made deferral necessary.
	conTxn *exec.ConstraintTxn

	// touched accumulates, across every statement, the node keys the transaction
	// created, labelled, or stripped a property from, for the commit-time NOT
	// NULL existence check (#1754). It is nil unless the engine had at least one
	// existence constraint active when BeginTx ran, so a transaction with none
	// records nothing. Shared by all statement mutators; checked once at Commit.
	touched *touchedNodes
	// stampCon: see [mutationUndo.stampCon] (rmp #2353/#2355).
	stampCon bool

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
	// wtx is the zero value. Each [ExplicitTx.Exec]
	// rejects any writing/DDL statement with [ErrWriteInReadOnlyTx] before
	// execution and routes a read through the engine's concurrent read path
	// at the handle's ONE pinned snapshot (the view field, rmp #2307) — so
	// reads observe SNAPSHOT isolation for the WHOLE transaction (every
	// statement sees the same instant; a commit landing between two statements
	// is invisible to the second) and never block, or are blocked by, other
	// readers or writers. Commit and Rollback on a read-only handle are
	// teardown-only no-ops.
	readOnly bool

	// wtx is THE TRANSACTION, and since rmp #2305 it is the only thing this handle
	// holds for its whole lifetime.
	//
	// It is the commit record every statement's versions are stamped with, opened by
	// [lpg.Graph.BeginVersionedTx] at BEGIN and published exactly once by
	// [lpg.Graph.EndVersionedTx] in release(). That single publication is what makes
	// a multi-statement transaction become visible at ONE instant and a rolled-back
	// one leave no trace — atomicity from the record, not from exclusion.
	//
	// It replaced a barrierHeld flag. Until rmp #2305 BEGIN took the graph's schema
	// barrier EXCLUSIVELY and held it to COMMIT/ROLLBACK, so every statement had to
	// route through [lpg.Graph.ApplyInsideLockedTx] (which resolves the transaction
	// from the graph's AMBIENT slot, correct only while one writer can exist) and a
	// paused client blocked every other writer in the process. Now each statement
	// takes the barrier SHARED for its own duration through
	// [lpg.Graph.ApplyInVersionedTx], carrying this handle explicitly, and between
	// statements NOTHING is held.
	//
	// Zero for a read-only handle, for which EndVersionedTx is a no-op.
	wtx lpg.WriteTx

	// view is the read instant every statement of a READ-ONLY handle executes
	// at: one snapshot, opened by [Engine.BeginReadTx] and registered with the
	// reclamation horizon for the whole lifetime of the transaction (rmp #2307).
	// It is what makes an explicit read transaction SNAPSHOT-ISOLATED rather
	// than read-committed — without it each Exec opened a fresh instant and a
	// commit landing between two statements became visible mid-transaction.
	//
	// nil on a write handle, which needs no separate view because it must read the
	// PRESENT to see its own uncommitted writes: it resolves through its own
	// transaction id via [mvcc.Visible], not through a pinned start instant.
	//
	// This used to say the write handle holds visMu exclusively from BEGIN to
	// COMMIT so nothing else can publish a commit while it runs. THAT IS FALSE
	// since rmp #2305: no lock spans the handle, other transactions publish
	// commits freely while it is open, and what keeps this handle's own reads
	// stable is the transaction id it stamps its versions with — not exclusion.
	//
	// The horizon slot it occupies is released exactly once, in [release], so
	// every exit path — Commit, Rollback, a panic in Exec, a panic in
	// Commit/Rollback — returns it. A slot that is never returned pins the
	// watermark for the life of the process.
	view *pinnedView
}

// BeginTx opens an explicit, multi-statement transaction bound to ctx. It acquires
// NOTHING: no writer serialisation, no visibility barrier, no lock of any kind.
// Concurrency control is MVCC alone. The caller MUST finish the returned handle with
// exactly one [ExplicitTx.Commit] or [ExplicitTx.Rollback].
//
// This doc used to read "but it does take the graph's visibility barrier
// exclusively, which until rmp #2305 retires it still blocks concurrent writers for
// the transaction's lifetime". THAT IS FALSE and has been since rmp #2305 did retire
// it — the sentence outlived the change it was describing, and it contradicted both
// this file's own header ("What serialises an explicit transaction: NOTHING") and
// the [ExplicitTx.view] field doc. What keeps this handle's reads stable is the
// transaction id it stamps its versions with, not exclusion (rmp #2345).
//
// ctx bounds every statement executed through the handle. Pass the connection
// context (optionally narrowed with a transaction timeout) so that a cancelled
// connection, a server shutdown, or an elapsed timeout interrupts an in-flight
// statement. It no longer guards against a serialisation being held forever —
// there is none to hold — but it still bounds a statement queued behind a DDL.
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
// [mvcc.Gate.StrongLockCtx] and the acquireCtx helper beside it for why a queued
// lock acquisition cannot simply be abandoned and what is done instead.
//
// See exectx.go for the full transaction and concurrency contract, including the
// isolation scope: concurrent readers do NOT block while this transaction is
// open, and observe the state before it began until it commits (task #1412,
// strengthened by rmp #2290).
func (e *Engine) BeginTx(ctx context.Context) (*ExplicitTx, error) {
	return e.beginTxSession(ctx, nil)
}

// beginTxSession is [Engine.BeginTx] optionally bound to a caller session; see
// [Session] and [ExplicitTx.sess].
func (e *Engine) beginTxSession(ctx context.Context, sess *lpg.Session[string, float64]) (*ExplicitTx, error) {
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
		// Shared across every statement, exactly like buf and undo: a release taken
		// in one statement is committed or dropped with the WHOLE transaction (rmp
		// #2366). It allocates its map only on the first release, so a transaction
		// under a schema with no UNIQUE constraint pays one pointer.
		conTxn: &exec.ConstraintTxn{},
	}
	// Allocate the touched-node set only when an existence constraint is active,
	// so a transaction with none records nothing (#1754).
	if e.constraintReg != nil && e.constraintReg.HasAnyNotNull() {
		tx.touched = &touchedNodes{}
	}
	// The per-node CONSTRAINT stamp is gated separately and more widely; see the
	// matching comment on the autocommit path (rmp #2353, widened by rmp #2355).
	tx.stampCon = e.constraintReg != nil &&
		(e.constraintReg.HasAnyNotNull() || e.constraintReg.HasAnyUnique())
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
	// Open the transaction. This takes NO LOCK (rmp #2305): it allocates the commit
	// record every statement will stamp its versions with, registers with the
	// reclamation horizon, and returns. Concurrent readers never observe uncommitted
	// writes because the record is unpublished until COMMIT, not because anything is
	// excluded — which is what MVCC is for.
	//
	// This replaced an EXCLUSIVE hold on the graph's schema barrier taken here and
	// released only at COMMIT/ROLLBACK. That hold was the last transaction-lifetime
	// module-wide lock in the module, and over Bolt it meant a client that sent BEGIN
	// and then stopped talking blocked EVERY other writer in the process for as long
	// as its transaction stayed open. Gated end-to-end against the official driver by
	// bolt/server's TestE2E_TwoExplicitWriteTransactionsOverlap and
	// TestE2E_AnIdleExplicitTransactionDoesNotStallAnotherWriter, both verified to
	// FAIL against the build that held it.
	//
	// Taken LAST, after every failure return above, so a BeginTx that returns an
	// error never leaves a commit record unpublished or a horizon slot pinned.
	// Through the SESSION when there is one: it waits for the frontier to reach this
	// caller's last commit BEFORE the transaction takes its snapshot, so the
	// transaction observes everything the caller has already done (rmp #2329).
	tx.sess = sess
	if sess != nil {
		wtx, werr := sess.BeginVersionedTxCtx(ctx)
		if werr != nil {
			cmetrics.IncCounter("cypher.BeginTx.errors", 1)
			return nil, werr
		}
		tx.wtx = wtx
	} else {
		tx.wtx = tx.eng.g.BeginVersionedTx()
	}
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
//   - otherwise runs through the engine's concurrent read path at the
//     transaction's ONE pinned snapshot, taken here at BEGIN and held for the
//     handle's whole lifetime (rmp #2307). Reads therefore observe SNAPSHOT
//     isolation across the statements of the transaction — every statement
//     executes at the same instant, so a commit made by anyone else between two
//     statements is invisible to the second (stronger than Neo4j's documented
//     read-committed default; matching Memgraph's default) — and run fully in
//     parallel with other readers and writers.
//
// The pinned snapshot registers with the version-reclamation horizon for the
// transaction's lifetime, so finish the handle promptly: an abandoned read
// transaction pins version memory until it is torn down.
//
// If ctx is already cancelled or its deadline has elapsed, BeginReadTx returns
// promptly with an error wrapping the context error (matchable via [errors.Is]
// against [context.Canceled] / [context.DeadlineExceeded]).
func (e *Engine) BeginReadTx(ctx context.Context) (*ExplicitTx, error) {
	return e.beginReadTxSession(ctx, nil)
}

// beginReadTxSession is [Engine.BeginReadTx] optionally bound to a caller session.
// The session's only role on a read handle is the WAIT before the snapshot is taken:
// a read transaction publishes no instant, so there is nothing to record.
func (e *Engine) beginReadTxSession(ctx context.Context, sess *lpg.Session[string, float64]) (*ExplicitTx, error) {
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
		view: &pinnedView{snap: beginReadFor(ctx, e, sess)},
		// buf, undo, walTx remain nil; wtx stays the zero value.
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
	// permitted read through the engine's concurrent read path at the
	// transaction's ONE pinned snapshot (rmp #2307) — snapshot isolation for
	// the whole transaction, not per statement. This path never touches
	// buf/undo/walTx (all nil) or the visibility barrier.
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

	entry, autoParams, err := tx.eng.parseAndAnalyse(query)
	params = mergeAutoParams(params, autoParams)
	if err != nil {
		return nil, err
	}
	if entry.semaErr != nil {
		return nil, entry.semaErr
	}

	// EXPLAIN / PROFILE prefix (rmp #2721). Diverted before the mutator is built
	// and before the statement takes the schema barrier, so an EXPLAIN inside an
	// open write transaction still executes nothing.
	//
	// The plan is built — and, for PROFILE, executed — against its OWN read
	// snapshot rather than this transaction's uncommitted state, exactly as
	// [Engine.Explain] and [Engine.Profile] do when called on the same engine. A
	// prefixed statement therefore does not observe writes this transaction has
	// not committed. That is a diagnostic reading the committed graph, not a
	// statement of the transaction.
	if entry.planMode != parser.PlanModeNone {
		return tx.eng.runPlanPrefixed(tx.ctx, entry, params, nil)
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
		mutator = &walMutatorAdapter{g: tx.eng.g, tx: tx.walTx, buf: tx.buf, undo: tx.undo, touched: tx.touched, stampCon: tx.stampCon, cbuf: tx.cbuf, eng: tx.eng, conTxn: tx.conTxn}
	} else {
		mutator = &lpgMutatorAdapter{g: tx.eng.g, buf: tx.buf, undo: tx.undo, touched: tx.touched, stampCon: tx.stampCon, cbuf: tx.cbuf, eng: tx.eng, conTxn: tx.conTxn}
	}

	// One statement, one SHARED hold on the schema barrier, carrying THIS handle's
	// transaction (rmp #2305). The hold lasts for the statement and no longer, so
	// nothing is held across the client's next round-trip; the statement's writes
	// are stamped with tx.wtx and stay invisible until COMMIT publishes it.
	//
	// The transaction is passed explicitly rather than resolved from the graph's
	// ambient slot, which is the lesson of rmp #2320: with concurrent writers that
	// slot names whichever transaction published last, so reading it would attribute
	// one transaction's writes to another. ApplyInsideLockedTx does exactly that
	// lookup and is therefore no longer usable here.
	//
	// The acquisition is bounded by the transaction's own context, so a statement
	// waiting behind a concurrent DDL honours the caller's deadline (rmp #2174).
	applyFn := func(fn func(lpg.WriteTx) error) error {
		return tx.eng.g.ApplyInVersionedTx(tx.ctx, tx.wtx, fn)
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
	// wtx is the zero value here). A second call is ErrTxFinished.
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
	var conflictErr error
	// The finalisation runs under a SHARED hold carrying this transaction, exactly as
	// a statement does. It is UNCANCELLABLE: the statements have already applied, so
	// abandoning the finalisation would leave the commit record neither published nor
	// aborted, which stalls the contiguous commit frontier permanently. release()
	// publishes the record afterwards, so durable-then-visible is preserved — the WAL
	// fsync below happens first.
	applyFn := func(fn func() error) error {
		return tx.eng.g.ApplyInVersionedTx(context.Background(), tx.wtx,
			func(lpg.WriteTx) error { return fn() })
	}
	_ = applyFn(func() error {
		// SERIALIZATION-CONFLICT BACKSTOP (rmp #2354, ACID Atomicity + Isolation).
		//
		// Runs before everything else, inside the barrier, BEFORE the WAL fsync.
		//
		// tx.failed above catches only a statement that RETURNED an error. A conflict
		// hit by a primitive that cannot return one — a label removal, a property
		// delete, any of the five per-edge side stores — is RECORDED on the
		// transaction instead and surfaces nowhere unless it is asked for
		// ([lpg.WriteTx.Err], and see it for the prior art). Without this check such a
		// transaction committed successfully having silently dropped that write:
		//
		//	T1: REMOVE n:Acct   (uncommitted)
		//	T2: REMOVE n:Acct   → conflict RECORDED, statement returns nil
		//	T2: COMMIT          → nil, and the label is still there
		//
		// which is a lost update with nothing reporting it. Measured exactly that by
		// TestLabelStore_ConcurrentRemove, which fails against a build without this.
		// lpg's own labelTx.commit has always asked; this path never did.
		//
		// A doomed transaction is rolled back atomically here, exactly like the NOT
		// NULL violation and the fsync failure below, so none of its eager writes
		// survive the barrier.
		if cerr := tx.wtx.Err(); cerr != nil {
			cmetrics.IncCounter("cypher.ExplicitTx.serializationConflicts", 1)
			conflictErr = cerr
			if undoOK := tx.rollbackInBarrierLocked(); !undoOK {
				conflictErr = wrapUndoFailure(conflictErr)
			}
			return nil
		}
		// Commit-time NOT NULL existence check (#1754, ACID Consistency). Runs
		// FIRST, inside the barrier, BEFORE the WAL fsync, so a node left in its
		// final committed state carrying a constrained label but lacking the
		// required property rejects the WHOLE transaction atomically: the
		// accumulated in-memory undo is replayed and the index/WAL rolled back,
		// exactly like the fsync-failure branch below. touched is nil (check a
		// no-op) unless the engine had an existence constraint active at BeginTx.
		// Through THIS transaction's view (rmp #2350); see the same call in
		// commitUnderBarrier.
		if nnErr := tx.touched.checkNotNullConstraints(tx.eng.constraintReg, tx.eng.g.WriterViewOf(tx.wtx)); nnErr != nil {
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
			// Allocate this transaction's MVCC instant HERE, before the fsync, so
			// the durable OpCommit record carries it and recovery can derive the
			// clock from the WAL (rmp #2309). release() publishes it afterwards, so
			// the allocate → encode → fsync → publish order holds and
			// durable-then-visible is preserved.
			if werr := tx.walTx.CommitWALOnly(tx.eng.g.AllocateCommitTS(tx.wtx)); werr != nil {
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
		// APPLY THE DEFERRED UNIQUE RELEASES (rmp #2366), on the same
		// durable-then-visible side of the barrier as the index buffer: a value this
		// transaction vacated becomes available exactly when the graph stops holding
		// it, and not before. Every failure branch above rolls back through
		// rollbackInBarrierLocked, which drops the same marks instead.
		tx.eng.constraintReg.CommitTxn(tx.conTxn)
		// Drop the undo log: the transaction is keeping its writes.
		tx.undo = nil
		return nil
	})
	// Reported BEFORE the NOT NULL verdict: a doomed transaction's own view is a
	// state that will never commit, so a constraint conclusion drawn from it is
	// drawn from an invalid premise. The conflict is also the RETRIABLE answer, and
	// it is the one the caller can act on.
	if conflictErr != nil {
		return conflictErr
	}
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
	// Uncancellable shared hold carrying this transaction; see [ExplicitTx.Commit].
	applyFn := func(fn func() error) error {
		return tx.eng.g.ApplyInVersionedTx(context.Background(), tx.wtx,
			func(lpg.WriteTx) error { return fn() })
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
	//
	// The DEFERRED RELEASES are dropped rather than inverted (rmp #2366): they never
	// reached the shared value-set, so there is nothing to put back and nothing that
	// could collide with a peer's own committed release. This is what makes the
	// rollback order-independent.
	tx.conTxn.Reset()
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

// release finishes the handle exactly once, publishing the transaction's commit
// record — or marking it aborted if the transaction was doomed — via
// [lpg.Graph.EndVersionedTx].
//
// That single publication is the transaction's commit instant, and it is the LAST
// thing release does that is observable: every statement's versions become visible
// together, after the WAL fsync the caller's finalisation already performed.
//
// There is nothing else left to release. rmp #2306 retired the engine's writer
// mutex and the store's capacity-one semaphore, and rmp #2305 retired the
// transaction-lifetime barrier hold; on a WAL-backed engine the store's writer
// registration is cleared by walTx's own Commit/Rollback. Idempotent via the
// finished flag.
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
	// Publish (or abort) the transaction's commit record and return its horizon
	// slot. Exactly once, guaranteed by the finished flag above; a second call would
	// return an already-returned slot and corrupt the reclamation watermark for
	// every other transaction. A no-op on a read-only handle, whose wtx is the zero
	// value.
	// Through the SESSION when there is one, which is what records the instant. The
	// lpg contract is explicit that closing with Graph.EndVersionedTx instead
	// publishes correctly but does NOT advance the session's floor, so the session
	// would silently lose its guarantee from that point on.
	if tx.sess != nil {
		tx.sess.EndVersionedTx(tx.wtx)
	} else {
		tx.eng.g.EndVersionedTx(tx.wtx)
	}
	tx.wtx = lpg.WriteTx{}
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
// [ExplicitTx.Rollback]. It rolls the WAL transaction back — exactly as its
// sibling [ExplicitTx.recoverExecPanic] does — and converts a panic raised
// during the in-barrier finalisation to an error wrapping [ErrInternalPanic].
//
// The WAL rollback is what CLEARS THE STORE'S WRITER REGISTRATION, and nothing
// else on this path does (rmp #2707). release() does not: its own doc records
// why — "on a WAL-backed engine the store's writer registration is cleared by
// walTx's own Commit/Rollback" — and on the panic path neither Commit nor
// Rollback has run. Without this line [txn.Store]'s in-flight count leaks by
// one, and [txn.Store.drainInflight] is an UNCANCELLABLE wait for that count to
// reach zero, so the next [txn.Store.RunUnderCommitLock] — the seam the
// checkpointer and store.DB.Close both take — never returns: shutdown hangs for
// ever and the WAL grows unbounded. A leaked containment boundary is worse than
// the panic it contains.
//
// It is safe on EVERY panic instant, which is the property that lets one
// unconditional call cover the whole finalisation:
//
//   - Panic BEFORE the WAL fsync (the reachable window): the transaction is
//     unfinished, so Rollback discards the buffered ops and calls exitWriter
//     exactly once. Nothing durable is discarded — no OpCommit marker was ever
//     fsynced, so recovery would drop those frames anyway.
//   - Panic AFTER the WAL fsync: [txn.Tx.CommitWALOnly] has already marked the
//     transaction finished and already called exitWriter through its own defer,
//     so Rollback short-circuits on the finished flag, returns ErrTxFinished,
//     and does NOT decrement the count a second time. A durable commit is never
//     undone here: Rollback "discards buffered ops without touching the WAL".
//
// release is NOT called here, and must not be: it runs via its own defer,
// registered AFTER this one at both call sites, so on unwind it executes FIRST
// (defers are LIFO) and the transaction is already finished by the time this
// handler runs.
//
// errp must be a pointer for the same named-return reason as [recoverExecPanic].
//
//nolint:gocritic // ptrToRefParam: errp must be the caller's named-return pointer
func (tx *ExplicitTx) recoverFinishPanic(errp *error) {
	if r := recover(); r != nil {
		if tx.walTx != nil {
			_ = tx.walTx.Rollback() //nolint:errcheck // rollback error is not actionable while converting a panic
		}
		convertQueryPanic(r, errp, "cypher.ExplicitTx.finish", "cypher.ExplicitTx.finish.panics")
	}
}
