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
	curRow  Row // positional view of the current row, set by Next alongside current
	err     error
	closed  bool
}

// Run initialises plan, stores the column names, and returns a [ResultSet]
// ready for iteration. The caller drives iteration via [ResultSet.Next] and
// must call [ResultSet.Close] when done.
//
// Run does not pull any rows; all work happens lazily in [ResultSet.Next].
// The Record map is pre-allocated once here and reused across every Next call
// to eliminate the per-row allocation that previously dominated heap usage.
func Run(ctx context.Context, plan Operator, cols []string) *ResultSet {
	rs := &ResultSet{
		plan:    plan,
		cols:    cols,
		ctx:     ctx,
		current: make(Record, len(cols)), // pre-allocated; reused by each Next call
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

	// Retain only the positional view of the row. The map projection into
	// rs.current is built lazily by Record/TakeRow, so a caller that consumes
	// rows positionally (or never reads them at all — e.g. a count(*) drain, or
	// the materialisation path) pays no per-row map-rehash cost. The operator
	// owns row's backing array and reuses it on the next Next, so callers that
	// retain it across Next must copy (the godoc contract on Row says so).
	rs.curRow = row
	return true
}

// Row returns the current row as a positional slice of values whose indices
// correspond to [ResultSet.Columns]. The slice is owned by the operator tree
// and is reused on the next [ResultSet.Next] call; callers that retain values
// beyond the next Next must copy them. This is the allocation-free accessor:
// unlike [ResultSet.Record] it never builds a map. Must only be called after a
// successful Next.
func (rs *ResultSet) Row() Row {
	return rs.curRow
}

// Record returns the current row as a map. Must only be called after a
// successful Next.
//
// The returned map is owned by the ResultSet and reused by the next Next call;
// callers that need to retain a row beyond the next Next must copy it (or use
// [ResultSet.TakeRecord]). The map is built lazily on the first Record call for
// the current row, so a caller that consumes rows positionally via
// [ResultSet.Row] never pays for it.
func (rs *ResultSet) Record() Record {
	rs.fillCurrent()
	return rs.current
}

// fillCurrent projects the positional current row into the reused rs.current
// map. Splitting it out lets Record and TakeRecord share the projection.
func (rs *ResultSet) fillCurrent() {
	for i, col := range rs.cols {
		if i < len(rs.curRow) {
			rs.current[col] = rs.curRow[i]
		} else {
			rs.current[col] = nil
		}
	}
}

// TakeRecord returns the current row and transfers ownership of its backing
// map to the caller, installing a fresh map for subsequent Next calls. Unlike
// [ResultSet.Record] — whose result is reused on the next Next — the map
// returned here is safe to retain. The materialisation path uses this to drain
// rows under the transaction-visibility barrier without the extra per-row copy
// that re-hashing every column into a new map would cost. Must only be called
// after a successful Next.
func (rs *ResultSet) TakeRecord() Record {
	rs.fillCurrent()
	rec := rs.current
	rs.current = make(Record, len(rs.cols))
	return rec
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
