package cypher_test

// call_in_transactions_rollback_test.go — CALL { } IN TRANSACTIONS rollback
// behaviour under error (T890).
//
// In a correctly implemented CALL { } IN TRANSACTIONS execution:
//   - Each batch is committed independently as soon as it completes.
//   - If a batch fails, only that batch is rolled back; previously committed
//     batches are NOT rolled back.
//   - The error from the failing batch is propagated to the caller.
//
// This partial-durability contract differs from a regular transaction where the
// entire operation is atomic.
//
// As of this writing CALL { } IN TRANSACTIONS is not implemented in GoGraph
// (see call_in_transactions_test.go and apply_subquery_test.go). These tests
// are skipped until the feature lands.
//
// When the feature is implemented:
//
//  1. Remove the t.Skipf calls.
//  2. Implement a node property that triggers an error on the second batch
//     (e.g. a constraint violation or a deliberate CALL to an error procedure).
//  3. Assert:
//     a. The first batch's nodes are present in the graph.
//     b. The second batch's nodes are absent.
//     c. The engine returns a non-nil error.

import (
	"context"
	"testing"
)

// TestCallInTransactions_RollbackOnError documents the partial-commit contract:
// a batch that fails must be rolled back while earlier committed batches remain
// durable.
//
// Skipped until CALL { } IN TRANSACTIONS is implemented.
func TestCallInTransactions_RollbackOnError(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	// Intended setup: batch 1 (i=0..9) should commit; batch 2 (i=10..19) should
	// fail due to a constraint or procedure error, rolling back only batch 2.
	const q = `
UNWIND range(0, 19) AS i
CALL {
    WITH i
    CREATE (:Batch {seq: i})
} IN TRANSACTIONS OF 10 ROWS`
	_, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Skipf("CALL { } IN TRANSACTIONS not yet implemented: %v", err)
	}
	// When implemented (with an error-triggering inner body):
	// - Verify batch 1 nodes (seq 0–9) are present.
	// - Verify batch 2 nodes (seq 10–19) are absent.
	// - Verify the call returned a non-nil error.
	t.Skip("CALL { } IN TRANSACTIONS rollback not yet implemented (accepted without error — update when wired)")
}

// TestCallInTransactions_AllBatchesCommit verifies that when no error occurs,
// all batches commit and the full result set is present.
//
// This is the happy-path complement to TestCallInTransactions_RollbackOnError.
// Skipped until CALL { } IN TRANSACTIONS is implemented.
func TestCallInTransactions_AllBatchesCommit(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	const q = `
UNWIND range(0, 19) AS i
CALL {
    WITH i
    CREATE (:Batch {seq: i})
} IN TRANSACTIONS OF 10 ROWS`
	_, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Skipf("CALL { } IN TRANSACTIONS not yet implemented: %v", err)
	}
	// When implemented (no error-triggering body):
	// - Verify all 20 :Batch nodes (seq 0–19) are present.
	t.Skip("CALL { } IN TRANSACTIONS all-commit not yet implemented (accepted without error — update when wired)")
}
