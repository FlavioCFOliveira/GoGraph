package server

import (
	"context"
	"time"

	"gograph/cypher"
)

// Tx wraps a cypher.Engine transaction context for an explicit Bolt transaction
// opened by a BEGIN message.
//
// Tx is NOT safe for concurrent use; it is owned by a single Session whose
// message loop is single-threaded per connection.
type Tx struct {
	// results holds all Result cursors opened within this transaction. They are
	// drained and closed on Rollback.
	results []*cypher.Result

	// ctx is derived from the connection context, optionally with a timeout.
	ctx context.Context

	// cancel cancels ctx when the transaction ends (commit, rollback, or reset).
	cancel context.CancelFunc

	// eng is the Cypher engine used to execute queries within this transaction.
	eng *cypher.Engine

	// mode is "w" for write transactions and "r" for read-only. Currently both
	// use RunInTxAny; the distinction is reserved for future enforcement.
	mode string
}

// newTx creates a new explicit transaction backed by eng.
// When timeout > 0, the transaction context is derived with that deadline.
func newTx(ctx context.Context, eng *cypher.Engine, mode string, timeout time.Duration) *Tx {
	txCtx := ctx
	cancel := context.CancelFunc(func() {}) // no-op default
	if timeout > 0 {
		txCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	return &Tx{
		ctx:    txCtx,
		cancel: cancel,
		eng:    eng,
		mode:   mode,
	}
}

// Run executes query inside the transaction, buffers the result cursor, and
// returns it to the caller for streaming.
func (tx *Tx) Run(query string, params map[string]any) (*cypher.Result, error) {
	result, err := tx.eng.RunInTxAny(tx.ctx, query, params)
	if err != nil {
		return nil, err
	}
	tx.results = append(tx.results, result)
	return result, nil
}

// Commit completes the transaction by closing the last open result cursor (so
// that index changes are applied) and cancelling the transaction context.
//
// For the in-memory engine, "commit" means draining the last result cursor to
// let any buffered writes flush. Earlier results have already been drained by
// the PULL/DISCARD handlers.
func (tx *Tx) Commit() error {
	defer tx.cancel()

	// Close all tracked cursors. The last open one triggers index buffer flush
	// (commit path) in cypher.Result.Close.
	for _, r := range tx.results {
		if err := r.Close(); err != nil {
			return err
		}
	}
	tx.results = nil
	return nil
}

// Rollback discards all buffered results by closing every open cursor and
// cancelling the transaction context.
//
// For the in-memory engine the graph mutations are already applied eagerly;
// Rollback prevents further mutations and cleans up resources.
func (tx *Tx) Rollback() error {
	defer tx.cancel()

	for _, r := range tx.results {
		_ = r.Close() //nolint:errcheck // best-effort close on rollback path
	}
	tx.results = nil
	return nil
}
