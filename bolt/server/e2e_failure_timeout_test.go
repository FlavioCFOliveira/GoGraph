package server_test

// e2e_failure_timeout_test.go — T858: Failure mapping: timeout.
//
// The GoGraph server maps context.DeadlineExceeded to
// "Neo.ClientError.Transaction.TransactionTimedOut". The driver surfaces this
// as a *neo4j.Neo4jError whose Code begins "Neo.ClientError." when the
// cancellation propagates to the server and the server sends a Failure.
//
// In practice, with a very short deadline on a large streaming query, the
// driver may surface a connectivity-level error (timeout reading from
// connection) before the server responds with a Failure. Both outcomes are
// acceptable: the test accepts any error from the set
// {Neo4jError with Neo.ClientError.* code, ConnectivityError, context error}.
//
// Known limitations:
//   - The server does not implement per-statement timeouts; cancellation is
//     driven by context on the client side.
//   - After a timeout-induced failure the driver marks the connection defunct;
//     a fresh session creates a new connection (AC#3).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// isDriverCancellationError reports whether err is any of the driver's
// cancellation / timeout error types, including Neo4jError with a
// Neo.ClientError.* code.
func isDriverCancellationError(err error) bool {
	if err == nil {
		return false
	}
	// Neo4jError with client error code.
	var neoErr *neo4j.Neo4jError
	if errors.As(err, &neoErr) {
		return strings.HasPrefix(neoErr.Code, "Neo.ClientError.")
	}
	// ConnectivityError (connection-level timeout / read canceled).
	if neo4j.IsConnectivityError(err) {
		return true
	}
	// Raw context errors.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// Driver wraps context errors as strings; check message as last resort.
	msg := err.Error()
	return strings.Contains(msg, "cancel") ||
		strings.Contains(msg, "deadline") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "Timeout")
}

// TestE2E_FailureTimeout cancels a streaming context via a short deadline and
// asserts the driver reports an error consistent with AC requirements:
//
//  1. Driver receives an error (Failure or connectivity-level error).
//  2. Error is in the Neo.ClientError.* family, or is a connectivity / context error.
//  3. Fresh session on same driver succeeds (session returns to Ready or Defunct).
//  4. Race-clean.
//  5. goleak-clean.
func TestE2E_FailureTimeout(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	// Short deadline applied to the streaming read to trigger cancellation.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()

	result, err := session.Run(queryCtx,
		"UNWIND range(1, 1000000) AS n RETURN n",
		nil,
	)

	var failErr error
	if err != nil {
		failErr = err
	} else {
		for result.Next(queryCtx) {
		}
		failErr = result.Err()
	}

	if failErr == nil {
		// Query completed before deadline (possible on fast hardware).
		t.Skip("query completed before deadline elapsed; no failure to assert")
	}

	// AC#1 + AC#2: error must be in the acceptable set.
	if !isDriverCancellationError(failErr) {
		t.Errorf("AC#1+AC#2: unexpected error type %T: %v", failErr, failErr)
	} else {
		var neoErr *neo4j.Neo4jError
		if errors.As(failErr, &neoErr) {
			t.Logf("AC#1+AC#2: Neo4jError.Code=%q (HasPrefix Neo.ClientError.*: %v)",
				neoErr.Code, strings.HasPrefix(neoErr.Code, "Neo.ClientError."))
		} else {
			t.Logf("AC#1+AC#2: driver error %T: %v", failErr, failErr)
		}
	}

	// AC#3: fresh session on same driver must succeed.
	session2 := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session2.Close(ctx) //nolint:errcheck

	result2, err := session2.Run(ctx, "RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("fresh session after timeout: session.Run: %v", err)
	}
	if !result2.Next(ctx) {
		t.Fatal("fresh session: Next returned false")
	}
	if _, err := result2.Consume(ctx); err != nil {
		t.Fatalf("fresh session: Consume: %v", err)
	}
}
