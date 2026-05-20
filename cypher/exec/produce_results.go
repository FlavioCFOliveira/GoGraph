package exec

// produce_results.go — ProduceResults sink and Result iterable (task-246).
//
// ProduceResults is the terminal operator that wraps a physical plan and
// exposes the results as a streaming iterator conforming to the [Result]
// interface. Callers drive the iterator with Next/Record/Err/Close; the
// underlying Operator lifecycle (Init/Next/Close) is managed transparently.
//
// # Usage
//
//	rs := exec.Run(ctx, plan, []string{"n", "r", "m"})
//	defer rs.Close()
//	for rs.Next() {
//	    rec := rs.Record()
//	    // rec["n"] etc.
//	}
//	if err := rs.Err(); err != nil { ... }
//
// # Concurrency
//
// ResultSet is NOT safe for concurrent use.

import (
	"context"
	"fmt"
)

// ─────────────────────────────────────────────────────────────────────────────
// Record
// ─────────────────────────────────────────────────────────────────────────────

// Record is a single result row, accessed by column name.
// The underlying map is owned by the [ResultSet]; callers must copy values
// they need to retain beyond the next [ResultSet.Next] call.
type Record map[string]interface{}

// ─────────────────────────────────────────────────────────────────────────────
// Result interface
// ─────────────────────────────────────────────────────────────────────────────

// Result is a forward-only, streaming iterator over query result rows.
//
// # Lifecycle
//
//  1. Call [Next] in a loop until it returns false.
//  2. After the loop, check [Err] for any error that terminated iteration.
//  3. Call [Close] exactly once to release resources.
//
// # Concurrency
//
// Result implementations are NOT safe for concurrent use.
type Result interface {
	// Next advances to the next result row. It returns true if a row is
	// available, false at end-of-stream or on error. After Next returns false,
	// callers should check Err.
	Next() bool

	// Record returns the current row as a [Record] (column name → value).
	// Record must not be called before the first successful Next or after Next
	// returns false.
	Record() Record

	// Err returns the first error encountered during iteration, or nil.
	Err() error

	// Columns returns the ordered list of column names for this result set.
	// The slice is stable across calls and is not modified after construction.
	Columns() []string

	// Close releases all resources held by this result set, including the
	// underlying operator tree. It must be called exactly once.
	Close() error
}

// ─────────────────────────────────────────────────────────────────────────────
// ResultSet
// ─────────────────────────────────────────────────────────────────────────────

// ResultSet is the concrete implementation of [Result] returned by [Run].
//
// ResultSet is NOT safe for concurrent use.
type ResultSet struct {
	plan    Operator
	cols    []string
	ctx     context.Context //nolint:containedctx // stored for streaming lifecycle
	current Record
	err     error
	closed  bool
}

// Run initialises plan, stores the column names, and returns a [ResultSet]
// ready for iteration. The caller drives iteration via [ResultSet.Next] and
// must call [ResultSet.Close] when done.
//
// Run does not pull any rows; all work happens lazily in [ResultSet.Next].
func Run(ctx context.Context, plan Operator, cols []string) *ResultSet {
	rs := &ResultSet{
		plan: plan,
		cols: cols,
		ctx:  ctx,
	}
	if err := plan.Init(ctx); err != nil {
		rs.err = fmt.Errorf("exec: plan init: %w", err)
		rs.closed = true
	}
	return rs
}

// Next advances to the next result row. It returns true when a row is
// available (accessible via [Record]), and false at end-of-stream or on error.
func (rs *ResultSet) Next() bool {
	if rs.closed || rs.err != nil {
		return false
	}
	if err := rs.ctx.Err(); err != nil {
		rs.err = err
		return false
	}

	var row Row
	ok, err := rs.plan.Next(&row)
	if err != nil {
		rs.err = err
		return false
	}
	if !ok {
		return false
	}

	// Build Record: copy values to avoid aliasing the operator's row buffer.
	rec := make(Record, len(rs.cols))
	for i, col := range rs.cols {
		if i < len(row) {
			rec[col] = row[i]
		} else {
			rec[col] = nil
		}
	}
	rs.current = rec
	return true
}

// Record returns the current row. Must only be called after a successful Next.
func (rs *ResultSet) Record() Record {
	return rs.current
}

// Err returns the first error encountered during iteration, or nil.
func (rs *ResultSet) Err() error {
	return rs.err
}

// Columns returns the ordered list of column names. The slice is never nil and
// is stable for the lifetime of the ResultSet.
func (rs *ResultSet) Columns() []string {
	return rs.cols
}

// Close releases all resources held by the ResultSet, including the underlying
// operator tree. It must be called exactly once. Calling Close after a
// previous Close is a no-op.
func (rs *ResultSet) Close() error {
	if rs.closed {
		return nil
	}
	rs.closed = true
	return rs.plan.Close()
}
