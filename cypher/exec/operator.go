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
