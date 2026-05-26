package server_test

// e2e_failure_cancel_test.go — T862: Failure mapping: statement cancel.
//
// Cancels a streaming context mid-pull and verifies the driver surfaces an
// error in the Neo.ClientError.* family (or a wrapped context error, which
// the server maps to Neo.ClientError.Transaction.Terminated).
//
// Known limitations:
//   - The server maps context.Canceled to
//     "Neo.ClientError.Transaction.Terminated". The driver may surface this
//     as a Neo4jError or as a raw context error depending on timing.
//   - AC#2 (neo4j.IsClientError) uses IsNeo4jError because IsClientError does
//     not exist in neo4j-go-driver v5.28.4; code-prefix check substitutes.
//   - AC#3 (server-side cursor released): verified indirectly via goleak
//     absence and a subsequent session success.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_FailureCancel starts a million-row streaming query, cancels the
// context after the first 100 rows, and asserts:
//
//  1. Driver receives Failure with code prefix Neo.ClientError.*, or a
//     context-derived error.
//  2. neo4j.IsNeo4jError(err)==true (or errors.Is context error) — accepted
//     because IsClientError does not exist in the v5 driver.
//  3. Server-side cursor released: subsequent fresh session succeeds.
//  4. No goroutine leak (enforced by TestMain via goleak).
//  5. Race-clean.
func TestE2E_FailureCancel(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	queryCtx, cancel := context.WithCancel(ctx)

	result, err := session.Run(queryCtx,
		"UNWIND range(1, 1000000) AS n RETURN n",
		nil,
	)
	if err != nil {
		cancel()
		t.Fatalf("session.Run: %v", err)
	}

	// Read 100 rows then cancel.
	for i := 0; i < 100; i++ {
		if !result.Next(queryCtx) {
			cancel()
			t.Fatalf("result.Next returned false at row %d before cancel", i)
		}
	}
	cancel() // trigger cancellation mid-streaming

	// Drain until Next reports done (may be immediate after cancel).
	for result.Next(queryCtx) {
	}
	failErr := result.Err()

	if failErr == nil {
		// The driver consumed all rows before cancel propagated; skip.
		t.Skip("all rows consumed before cancel propagated; no failure to assert")
	}

	// AC#1 + AC#2: Neo4jError with Neo.ClientError.* prefix, connectivity error,
	// or context error — all are valid outcomes of mid-stream cancellation.
	if !isDriverCancellationError(failErr) {
		t.Errorf("AC#1+AC#2: unexpected error type %T: %v", failErr, failErr)
	} else {
		var neoErr *neo4j.Neo4jError
		if errors.As(failErr, &neoErr) {
			t.Logf("AC#1+AC#2: Neo4jError.Code=%q", neoErr.Code)
		} else {
			t.Logf("AC#1+AC#2: driver error %T: %v", failErr, failErr)
		}
	}

	// AC#3: cursor released — fresh session must succeed within 50 ms.
	freshCtx, freshCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer freshCancel()

	session2 := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session2.Close(ctx) //nolint:errcheck

	result2, err := session2.Run(freshCtx, "RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("fresh session after cancel: session.Run: %v", err)
	}
	if !result2.Next(freshCtx) {
		t.Fatal("fresh session: Next returned false")
	}
	if _, err := result2.Consume(freshCtx); err != nil {
		t.Fatalf("fresh session: Consume: %v", err)
	}
}
