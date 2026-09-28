package exec

import "context"

// Operator is the core abstraction of the Volcano iterator model. Every node
// in a physical query plan implements this interface.
//
// # Lifecycle
//
//  1. [Init] is called exactly once before the first call to [Next].
//  2. [Next] is called repeatedly until it returns (false, nil) or an error.
//  3. [Close] is called exactly once, regardless of whether [Next] returned
//     an error. Implementations must release all resources in [Close].
//
// # Cancellation
//
// Every [Next] implementation must check ctx.Done() at the top of the call.
// For long-running inner loops that produce more than 4096 tuples without
// returning, check ctx.Done() every 4096 iterations.
//
// # Concurrency
//
// An Operator instance is NOT safe for concurrent use. Each goroutine in a
// parallel pipeline segment owns its own operator tree.
type Operator interface {
	// Init initialises the operator and its children. ctx is stored for later
	// use in Next; implementations must not begin producing rows in Init.
	Init(ctx context.Context) error

	// Next advances the operator by one row, writing the result into out.
	// It returns (true, nil) if a row was written, (false, nil) at end-of-stream,
	// or (false, err) on error. After returning (false, _), Next must not be
	// called again.
	//
	// Implementations check ctx.Done() on every call. Long-running loops check
	// ctx.Done() every 4096 iterations.
	Next(out *Row) (bool, error)

	// Close releases all resources held by this operator (open file handles,
	// memory, goroutines). It must be called exactly once by the pipeline
	// driver, even when Next returned an error.
	Close() error
}

// nextRow pulls the next row of src through slot, an operator-owned [Row] field,
// and returns the row src wrote together with Next's result.
//
// It exists so that a parent operator's per-row child pull does not heap-allocate.
// The idiomatic `var row Row; child.Next(&row)` moves row to the heap on every
// call, because the address of a local is handed to an interface method whose
// implementation the compiler cannot see (one 24-byte slice header per row). slot
// is a field of the parent, which is already heap-allocated, so taking its
// address allocates nothing.
//
// slot is reset to nil before every call, so src receives exactly what a fresh
// local would have given it: an empty receiver with no backing array. This keeps
// the one producer that appends into its receiver ([ProcedureCallOp]'s void
// passthrough, `append((*out)[:0], …)`) from writing into a backing array that a
// previous call left in the slot and that may belong to another operator.
//
// The returned Row is a copy of the slice header, not of the values: it aliases
// the producer's buffer and is valid only until the next Next call on src, the
// contract [Row] documents. Resetting or reusing slot never changes a Row already
// returned, and no [Operator] retains its out pointer beyond the Next call, so one
// slot serves every child pull of its operator (an Apply's outer and inner pulls
// alike). slot needs no reset on Init or Close: nextRow clears it before each use.
//
// Like the operator that owns slot, nextRow is NOT safe for concurrent use; a
// parallel worker builds its own operator tree and therefore its own slots.
func nextRow(src Operator, slot *Row) (Row, bool, error) {
	*slot = nil
	ok, err := src.Next(slot)
	return *slot, ok, err
}
