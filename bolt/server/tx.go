package server

import (
	"context"
	"errors"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// Tx wraps an engine-level explicit transaction ([cypher.ExplicitTx]) for a
// single Bolt transaction opened by a BEGIN message. Every RUN issued between
// BEGIN and COMMIT/ROLLBACK executes against the SAME underlying engine
// transaction, so the statements are atomic together: COMMIT makes them durable
// and visible as one unit, ROLLBACK unwinds all of them (#1280). This replaces
// the previous behaviour in which each RUN opened and committed its own
// autocommit transaction and ROLLBACK undid nothing.
//
// Tx is NOT safe for concurrent use; it is owned by a single Session whose
// message loop is single-threaded per connection.
type Tx struct {
	// results holds the Result cursors opened within this transaction so they can
	// be drained and closed on teardown. Closing a cursor releases only its own
	// iterator state — the engine transaction is committed or rolled back through
	// engTx, never through these cursors.
	results []*cypher.Result

	// engTx is the open engine transaction. All statements run against it; it is
	// committed by Commit and rolled back by Rollback. It holds the engine's
	// writer serialisation (the store single-writer mutex when WAL-backed, the
	// engine writer mutex when store-less) from BEGIN until it finishes, so a
	// concurrent writer blocks until this transaction commits or rolls back
	// (write-write Isolation).
	engTx *cypher.ExplicitTx

	// cancel cancels the per-transaction context derived from the connection
	// context (with the transaction timeout, if any). It is invoked when the
	// transaction ends so the derived context and its timer are released.
	cancel context.CancelFunc

	// mode is "w" for write transactions and "r" for read-only.
	//
	//   - "w" opens a writing transaction via cypher.Engine.BeginTx: it holds the
	//     engine's writer serialisation and the visibility barrier from BEGIN
	//     until COMMIT/ROLLBACK, so concurrent writers serialise behind it and
	//     concurrent readers observe only committed state (write-write isolation
	//     for writers, read-committed for readers).
	//
	//   - "r" opens a read-only transaction via cypher.Engine.BeginReadTx: it
	//     acquires no writer lock, no barrier, and no WAL transaction, so it
	//     never serialises behind or blocks a concurrent writer. Each RUN takes
	//     its own per-statement View snapshot (read-committed across statements);
	//     any writing/DDL RUN is refused with cypher.ErrWriteInReadOnlyTx before
	//     execution. Commit and Rollback are teardown-only no-ops.
	//
	// The read-only/writing state itself lives in the engine transaction
	// (cypher.ExplicitTx); this field records the requested mode for the session.
	mode string
}

// newTx opens a new explicit transaction backed by eng, rooted at ctx (the
// CONNECTION context, so a dropped connection or server shutdown interrupts an
// in-flight statement) and bounded by timeout when timeout > 0. A finite timeout
// guarantees the engine writer serialisation the transaction holds can never be
// retained indefinitely.
//
// For a writing transaction (mode != "r") newTx acquires the engine writer
// serialisation via [cypher.Engine.BeginTx]; for a read-only transaction
// (mode == "r") it opens a lock-free read-only handle via
// [cypher.Engine.BeginReadTx], which holds no writer lock or barrier. On failure
// (a context already done before BEGIN) it returns the error and holds no
// resources.
func newTx(ctx context.Context, eng *cypher.Engine, mode string, timeout time.Duration) (*Tx, error) {
	txCtx := ctx
	cancel := context.CancelFunc(func() {}) // no-op default
	if timeout > 0 {
		txCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	var (
		engTx *cypher.ExplicitTx
		err   error
	)
	if mode == "r" {
		engTx, err = eng.BeginReadTx(txCtx)
	} else {
		engTx, err = eng.BeginTx(txCtx)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return &Tx{
		engTx:  engTx,
		cancel: cancel,
		mode:   mode,
	}, nil
}

// Run executes query inside the transaction WITHOUT committing, buffers the
// result cursor, and returns it to the caller for streaming. The statement's
// writes accumulate in the engine transaction and become durable/visible only on
// Commit.
//
// Runtime pipeline errors (where the query compiled and executed under the
// visibility barrier but the execution pipeline failed — e.g. a constraint
// violation or a type error mid-pipeline) are wrapped in a zero-row
// [cypher.Result] whose Err() carries the error. This preserves the Bolt v5
// state-machine contract: the session stays in TX_STREAMING so the driver can
// drain the cursor via PULL (where the FAILURE surfaces), rather than receiving
// a FAILURE directly from RUN. Build-phase errors (context cancellation,
// parse/sema/plan failures, DDL rejection) are propagated as a non-nil error
// return so the server enters FAILED at RUN time.
func (tx *Tx) Run(query string, params map[string]any) (*cypher.Result, error) {
	result, err := tx.engTx.ExecAny(query, params)
	if err != nil {
		// [cypher.ExplicitTx.Exec] now wraps runtime pipeline errors in
		// [cypher.ErrStatementPipeline] (#1378). Surface those via the cursor
		// (TX_STREAMING→FAILED at PULL time) to preserve the Bolt v5 driver's
		// state-machine expectations. All other errors (build phase, context,
		// DDL) propagate as runErr so the session enters FAILED at RUN time.
		var pipeErr *cypher.ErrStatementPipeline
		if errors.As(err, &pipeErr) {
			result = cypher.NewErrResult(pipeErr.Unwrap())
			tx.results = append(tx.results, result)
			return result, nil
		}
		return nil, err
	}
	tx.results = append(tx.results, result)
	return result, nil
}

// Commit makes every statement issued since BEGIN durable and visible as one
// atomic unit, then releases the transaction's resources. The engine fsyncs the
// WAL exactly once for the whole transaction (WAL-backed) and commits the
// secondary-index buffer; on a store-less engine the writes are already visible
// and the index buffer is finalised. The writer serialisation is released.
//
// Open result cursors are closed first (releasing their iterator state); the
// commit decision itself is made by the engine transaction, not by the cursors.
func (tx *Tx) Commit() error {
	defer tx.cancel()
	tx.closeCursors()
	return tx.engTx.Commit()
}

// Rollback unwinds every statement issued since BEGIN — restoring the in-memory
// graph to its pre-transaction state via the engine's accumulated undo log, and
// (WAL-backed) discarding the WAL transaction so a fresh recovery observes none
// of the writes — then releases the transaction's resources. It is best-effort
// and always releases the writer serialisation, even if an inverse operation
// fails.
func (tx *Tx) Rollback() error {
	defer tx.cancel()
	tx.closeCursors()
	return tx.engTx.Rollback()
}

// closeCursors drains and closes every tracked result cursor. Closing a cursor
// releases only its own iterator state; it never finalises the engine
// transaction (the cursors carry no transaction authority — see
// [cypher.ExplicitTx.Exec]).
func (tx *Tx) closeCursors() {
	for _, r := range tx.results {
		_ = r.Close() //nolint:errcheck // best-effort cursor close; the engine tx owns commit/rollback
	}
	tx.results = nil
}
