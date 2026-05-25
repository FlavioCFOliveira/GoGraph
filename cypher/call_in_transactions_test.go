package cypher_test

// call_in_transactions_test.go — CALL { } IN TRANSACTIONS OF n ROWS tests
// (T888).
//
// CALL { … } IN TRANSACTIONS OF n ROWS is a Cypher 5 feature that executes the
// inner subquery in separate, independently committed transactions of at most n
// rows each. It is primarily used for large bulk-write operations where a single
// transaction would be impractical.
//
// As of this writing GoGraph does not support CALL { … } inline subqueries at
// all (see apply_subquery_test.go). CALL { } IN TRANSACTIONS is therefore also
// unsupported.
//
// These tests document the intended behaviour and serve as a regression guard.
// When the feature is implemented:
//
//  1. Remove the t.Skipf calls.
//  2. Fill in the expected post-execution graph state.
//  3. Verify that batching is respected (exactly ceil(N/batchSize) commits).
//
// Intended behaviour (for reference):
//
//	// Batch-import 30 nodes in batches of 10:
//	UNWIND range(0, 29) AS i
//	CALL {
//	    WITH i
//	    CREATE (:Item {seq: i})
//	} IN TRANSACTIONS OF 10 ROWS
//
//	After execution: 30 :Item nodes exist; the import committed in 3 batches.

import (
	"context"
	"testing"
)

// TestCallInTransactions_BoundedBatch documents the primary use-case:
// CALL { … } IN TRANSACTIONS OF n ROWS should batch inner subquery execution
// into transactions of at most n rows.
//
// Skipped until CALL { } IN TRANSACTIONS is implemented.
func TestCallInTransactions_BoundedBatch(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	const q = `
UNWIND range(0, 29) AS i
CALL {
    WITH i
    CREATE (:Item {seq: i})
} IN TRANSACTIONS OF 10 ROWS`
	_, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Skipf("CALL { } IN TRANSACTIONS OF n ROWS not yet implemented: %v", err)
	}
	// When implemented: verify 30 :Item nodes exist.
	t.Skip("CALL { } IN TRANSACTIONS not yet implemented (accepted without error — update when wired)")
}
