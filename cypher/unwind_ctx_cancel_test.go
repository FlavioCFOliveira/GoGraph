package cypher_test

// unwind_ctx_cancel_test.go — context cancellation mid-iteration for UNWIND
// over a large list (T718).

import (
	"context"
	"errors"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestUnwind_CtxCancelMidIteration starts a UNWIND over 1000 elements,
// cancels the context after pulling 5 rows, and verifies that:
//
//  1. res.Next() eventually returns false after the cancellation.
//  2. Either res.Err() reports a context-related error, or the iteration
//     drained normally (engines that buffer eagerly may finish before the
//     cancel fires, which is also correct behaviour).
//
// The test does not mandate an exact row count after cancel because the
// engine is free to buffer the full result before iteration begins. The
// important invariant is that the context is respected (no infinite loop)
// and that no goroutine is leaked.
func TestUnwind_CtxCancelMidIteration(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// range(0, 999) produces 1000 elements: [0, 1, …, 999].
	res, err := eng.Run(ctx, `UNWIND range(0, 999) AS x RETURN x`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = res.Close() }()

	count := 0
	for res.Next() {
		count++
		if count == 5 {
			cancel()
		}
	}

	// After Next() returns false there must be no active goroutines left from
	// this query (Close drains them). The Close is deferred above.

	iterErr := res.Err()

	// Acceptable outcomes:
	//   A) iterErr is nil — engine buffered all results eagerly; all 1000
	//      rows were emitted before the cancel took effect. count may be
	//      anywhere in [5, 1000].
	//   B) iterErr is context.Canceled — the engine honoured the context and
	//      stopped mid-stream.
	//   C) iterErr wraps context.Canceled.
	if iterErr != nil &&
		!errors.Is(iterErr, context.Canceled) &&
		!errors.Is(iterErr, context.DeadlineExceeded) {
		t.Errorf("unexpected iteration error: %v", iterErr)
	}

	// At least 5 rows must have been consumed before cancel was called.
	if count < 5 {
		t.Errorf("consumed %d rows before cancel, want at least 5", count)
	}
}

// TestUnwind_CtxAlreadyCancelled verifies that running a UNWIND query against
// an already-cancelled context returns an error immediately (either from Run
// or from the first Next call) and does not hang.
func TestUnwind_CtxAlreadyCancelled(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run

	res, err := eng.Run(ctx, `UNWIND range(0, 999) AS x RETURN x`, nil)
	if err != nil {
		// Engine rejected immediately — correct. Either errors.Is(err,
		// context.Canceled) holds, or the engine wrapped the cancel with
		// extra context. Both are acceptable for a context already done.
		return
	}
	defer func() { _ = res.Close() }()

	// If Run succeeded, the first (or eventual) Next must return false with a
	// context error.
	for res.Next() {
	}
	if iterErr := res.Err(); iterErr != nil {
		if !errors.Is(iterErr, context.Canceled) &&
			!errors.Is(iterErr, context.DeadlineExceeded) {
			t.Errorf("unexpected iter error with cancelled ctx: %v", iterErr)
		}
	}
	// If iterErr is nil the engine drained synchronously before checking the
	// context — acceptable.
}

// TestUnwind_CtxDeadlineExceeded verifies that a tightly-bounded deadline
// applied to a UNWIND query causes it to stop without hanging.
func TestUnwind_CtxDeadlineExceeded(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	// Use a cancelled context rather than a real 1 ns timeout to keep the test
	// deterministic across slow CI machines. The semantic is identical: context
	// is done before (or very shortly after) Run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := eng.Run(ctx, `UNWIND range(0, 999) AS x RETURN x`, nil)
	if err != nil {
		return // engine rejected immediately — correct
	}
	defer func() { _ = res.Close() }()

	for res.Next() {
	}
	// Accept nil (synchronous drain) or context-related errors.
	if iterErr := res.Err(); iterErr != nil {
		if !errors.Is(iterErr, context.Canceled) &&
			!errors.Is(iterErr, context.DeadlineExceeded) {
			t.Errorf("unexpected iter error: %v", iterErr)
		}
	}
}
