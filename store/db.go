// Package store provides the composed teardown owner that bundles a
// WAL-backed store's durability pieces — a [wal.Writer] and an optional
// background [checkpoint.Checkpointer] — and closes them in the single
// crash-safe order.
//
// # Why a composed Close exists
//
// The persistence pieces each have their own lifecycle method ([wal.Writer.Close],
// [checkpoint.Checkpointer.Stop]), but closing them in the wrong order is a
// silent correctness bug: if the WAL is closed BEFORE the checkpoint goroutine
// is stopped, the still-running checkpoint loop calls [wal.Writer.Sync] /
// [wal.Writer.Truncate] on a closed writer, those calls return
// [wal.ErrWriterClosed], and the error is swallowed into the checkpointer's
// [checkpoint.Stats.LastError] rather than surfaced to the caller — and the
// goroutine keeps running past the process's shutdown intent until its own
// ticker happens to observe a stop signal. [DB.Close] performs the one correct
// order so every embedder gets it right without re-deriving it.
//
// # Teardown order
//
// [DB.Close] runs these steps, each idempotent and safe under a concurrent
// second [DB.Close]:
//
//  1. Optionally take a FINAL checkpoint (best-effort) while the checkpoint
//     loop is still alive, so a clean shutdown leaves the smallest possible
//     WAL tail to replay on the next open. Enabled with [WithFinalCheckpoint];
//     off by default. It must run BEFORE step 2 because once the loop is
//     stopped a checkpoint can no longer be requested
//     ([checkpoint.ErrCheckpointerStopped]).
//  2. Stop the checkpoint goroutine ([checkpoint.Checkpointer.Stop]). After
//     this returns the loop is gone, so no later WAL call can race the close.
//  3. Close the WAL ([wal.Writer.Close]), flushing and fsyncing any buffered
//     tail first.
//
// Stopping the checkpointer before closing the WAL is the invariant: it
// guarantees the checkpoint loop can never touch a closed WAL, so
// [wal.ErrWriterClosed] is never produced on the shutdown path and the
// goroutine never outlives the process's intent.
//
// # Quiescing writers
//
// [DB.Close] does NOT itself drain in-flight transactions: a [txn.Store]
// transaction holds the store's single-writer semaphore from Begin until
// Commit/Rollback, and a [cypher.Engine] write holds it for the statement, so
// the embedder is responsible for stopping new writes and letting the active
// one finish before calling Close (a Bolt server does this by draining its
// connections in [bolt/server.Server.Shutdown] before tearing down the DB).
// Close tears down the durability machinery; it is the caller's job to ensure
// no transaction is mid-commit when it runs. See docs/persistence.md
// ("Composed shutdown").
package store

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// DB satisfies io.Closer so it drops straight into any generic owner that
// closes an io.Closer on shutdown (e.g. bolt/server.Options.Closer).
var _ io.Closer = (*DB)(nil)

// Checkpointer is the behavioural view of a background checkpointer that a
// [DB] needs to tear it down: stop its goroutine and (for the optional final
// checkpoint) trigger one last fold. A *[checkpoint.Checkpointer] satisfies it,
// so [DB] need not be generic over the checkpointer's key/weight type
// parameters merely to own it. The interface is exported so [WithCheckpointer]
// has a named, documented parameter type rather than an opaque one.
type Checkpointer interface {
	// Stop signals the checkpoint goroutine to exit and blocks until it has.
	// It must be idempotent.
	Stop()
	// TriggerCtx requests one checkpoint and blocks until it completes,
	// honouring ctx. It returns a non-nil error once the loop has stopped
	// (e.g. [checkpoint.ErrCheckpointerStopped]); [DB] treats that as benign.
	TriggerCtx(ctx context.Context) error
}

// DB is a composed owner of a WAL-backed store's durability pieces — the WAL
// writer and an optional background checkpointer — that closes them in the one
// crash-safe order (see the package documentation). It does not own the
// in-memory graph or the [txn.Store]/[cypher.Engine] driving it: those carry
// no goroutine or file handle of their own and need no teardown.
//
// Construct a DB with [New] after wiring the WAL, store, engine, and (if used)
// the checkpointer the usual way; hand the DB to whatever owns the shutdown
// sequence (e.g. a [bolt/server.Server] via [bolt/server.Options.Closer]) and
// call [DB.Close] exactly where you would otherwise have hand-written
// "stop the checkpointer, then close the WAL".
//
// Concurrency: [DB.Close] is safe to call from any number of goroutines and
// any number of times; the underlying work runs once and every caller observes
// the same result.
type DB struct {
	wal            *wal.Writer
	cp             Checkpointer // nil when the DB owns no checkpointer
	finalCheckpt   bool
	closeOnce      sync.Once
	closeErr       error
	closeErrSetter sync.Mutex // guards closeErr publication for the racing-caller path
}

// Option customises a [DB] at construction. Options are applied in order by
// [New].
type Option func(*DB)

// WithCheckpointer makes the DB own the background checkpointer cp and stop it
// (before the WAL is closed) on [DB.Close]. Pass the same *[checkpoint.Checkpointer]
// that was [checkpoint.Checkpointer.Start]ed for this store. When omitted the
// DB owns no checkpointer and [DB.Close] closes only the WAL.
//
// A nil checkpointer is ignored (the DB keeps owning no checkpointer), so a
// caller can pass a possibly-nil checkpointer without a guard.
func WithCheckpointer(cp Checkpointer) Option {
	return func(d *DB) {
		if cp != nil {
			d.cp = cp
		}
	}
}

// WithFinalCheckpoint makes [DB.Close] take one best-effort checkpoint, while
// the checkpoint loop is still alive, before stopping it — so a clean shutdown
// folds the WAL tail into the snapshot and the next open replays the minimum.
// It has no effect unless the DB also owns a checkpointer ([WithCheckpointer]).
//
// The final checkpoint is best-effort: its error (including a benign
// [checkpoint.ErrCheckpointerStopped] if the loop has already exited) does NOT
// fail [DB.Close]; only the WAL-close error is returned, because durability of
// already-committed transactions does not depend on the final checkpoint
// running. Off by default to keep Close cheap and side-effect-free for callers
// that do not need a compacted shutdown.
func WithFinalCheckpoint() Option {
	return func(d *DB) { d.finalCheckpt = true }
}

// New returns a composed [DB] that owns wal and, when [WithCheckpointer] is
// supplied, the background checkpointer, and closes them in the crash-safe
// order on [DB.Close]. wal must not be nil.
//
// Typical wiring (string-keyed, WAL + engine + checkpointer):
//
//	wlog, _ := wal.Open(walPath)
//	st := txn.NewStoreWithCodec(g, wlog, txn.NewStringCodec())
//	eng := cypher.NewEngineWithStore(st)
//	cp := checkpoint.New(cfg, g, wlog, &unusedMu,
//		checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
//		checkpoint.WithMapperCodec[string, float64](st.Codec()),
//		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot))
//	cp.Start(ctx)
//	db := store.New(wlog, store.WithCheckpointer(cp), store.WithFinalCheckpoint())
//	defer db.Close() // or db.CloseCtx(ctx) to bound the final checkpoint
func New(w *wal.Writer, opts ...Option) *DB {
	d := &DB{wal: w}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Close tears the store's durability pieces down (see [DB.CloseCtx]) using a
// background context, so the optional final checkpoint runs unbounded. It is
// the [io.Closer] implementation, which is why its signature carries no
// context: this is the method a generic owner such as
// [bolt/server.Options.Closer] (an [io.Closer]) calls. Callers that want to
// bound the final checkpoint with a deadline call [DB.CloseCtx] directly.
func (d *DB) Close() error {
	return d.CloseCtx(context.Background())
}

// CloseCtx tears the store's durability pieces down in the single crash-safe
// order and returns the first error that matters for durability — the WAL
// close error.
//
// The order (see the package documentation):
//
//  1. If [WithFinalCheckpoint] is set and a checkpointer is owned, take one
//     best-effort final checkpoint, honouring ctx, while the loop is alive.
//     Its error is intentionally discarded.
//  2. Stop the checkpoint goroutine, if any, so it can no longer touch the WAL.
//  3. Close the WAL.
//
// CloseCtx is idempotent and safe under concurrent callers: the teardown runs
// exactly once (guarded by a [sync.Once]); a second or racing call (including a
// later [DB.Close]) returns the same error the first run produced, never a
// spurious [wal.ErrWriterClosed] from a double WAL close.
//
// ctx bounds only the optional final checkpoint (step 1); steps 2 and 3 run to
// completion regardless of ctx so the goroutine is always joined and the WAL
// always closed — abandoning them on ctx cancellation would reintroduce the
// goroutine/file-handle leak this type exists to prevent.
func (d *DB) CloseCtx(ctx context.Context) error {
	defer metrics.Time("store.DB.Close")()
	d.closeOnce.Do(func() {
		err := d.closeOnce0(ctx)
		d.closeErrSetter.Lock()
		d.closeErr = err
		d.closeErrSetter.Unlock()
		if err != nil {
			metrics.IncCounter("store.DB.Close.errors", 1)
		}
	})
	d.closeErrSetter.Lock()
	defer d.closeErrSetter.Unlock()
	return d.closeErr
}

// closeOnce0 runs the teardown body exactly once under the sync.Once.
func (d *DB) closeOnce0(ctx context.Context) error {
	// Step 1: optional best-effort final checkpoint, BEFORE the loop is
	// stopped (a stopped loop rejects the request with ErrCheckpointerStopped).
	// Its error never fails Close: already-committed transactions are durable
	// in the WAL regardless of whether this compaction runs.
	if d.finalCheckpt && d.cp != nil {
		if cpErr := d.cp.TriggerCtx(ctx); cpErr != nil &&
			!errors.Is(cpErr, context.Canceled) &&
			!errors.Is(cpErr, context.DeadlineExceeded) {
			// A non-context error (or ErrCheckpointerStopped) is surfaced only
			// as a metric, not returned, so a flaky final checkpoint can never
			// block or fail a shutdown that has already secured durability.
			metrics.IncCounter("store.DB.Close.finalCheckpointErrors", 1)
		}
	}
	// Step 2: stop the checkpoint goroutine so it can no longer Sync/Truncate
	// the WAL. This MUST precede the WAL close — the whole reason the type
	// exists. Stop blocks until the goroutine has exited, so once it returns
	// no WAL call can race the close below.
	if d.cp != nil {
		d.cp.Stop()
	}
	// Step 3: close the WAL. Now that the loop is gone this is the last access
	// to the writer, so ErrWriterClosed can only arise from a genuine
	// double-close by the embedder (which the sync.Once already prevents for
	// DB.Close itself).
	return d.wal.Close()
}
