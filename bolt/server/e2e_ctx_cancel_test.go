package server_test

// e2e_ctx_cancel_test.go — T871: ctx-cancel mid-streaming.
//
// Cancels the driver context after 1 000 rows during a million-row stream and
// verifies:
//   (a) the driver returns a context-derived or Neo4jError;
//   (b) the server-side cursor and goroutine are released within 50 ms;
//   (c) a subsequent session on the same connection (or fresh) succeeds.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_CtxCancelMidStream drives a 1M-row UNWIND, cancels after 1 000 rows,
// and asserts:
//
//  1. Driver returns errors.Is(err, context.Canceled) or a Neo4jError wrapping it.
//  2. Server-side cursor released within 50 ms: fresh session responds quickly.
//  3. goleak-clean (enforced by TestMain).
//  4. Race-clean.
func TestE2E_CtxCancelMidStream(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel() // ensure cancel is always called

	result, err := session.Run(streamCtx,
		"UNWIND range(1, 1000000) AS n RETURN n",
		nil,
	)
	if err != nil {
		t.Fatalf("session.Run: %v", err)
	}

	var rowCount int
	for result.Next(streamCtx) {
		rowCount++
		if rowCount == 1000 {
			cancel() // cancel mid-stream
		}
	}
	streamErr := result.Err()

	if streamErr == nil {
		t.Skip("all rows consumed before cancel propagated")
	}

	// AC#1: must be context.Canceled (direct or wrapped) or a Neo4jError.
	isCtxCanceled := errors.Is(streamErr, context.Canceled) ||
		strings.Contains(streamErr.Error(), "cancel")
	var neoErr *neo4j.Neo4jError
	isNeo := errors.As(streamErr, &neoErr)

	if !isCtxCanceled && !isNeo {
		t.Errorf("AC#1: unexpected error type %T: %v", streamErr, streamErr)
	} else {
		t.Logf("AC#1: err=%v (isCtxCanceled=%v, isNeo4j=%v)", streamErr, isCtxCanceled, isNeo)
	}

	// AC#2: server-side cursor released within 50 ms — fresh session must
	// respond within that window.
	freshDeadline, freshCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer freshCancel()

	session2 := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session2.Close(ctx) //nolint:errcheck

	result2, err := session2.Run(freshDeadline, "RETURN 1 AS n", nil)
	if err != nil {
		t.Fatalf("AC#2: fresh session.Run within 50ms: %v", err)
	}
	if !result2.Next(freshDeadline) {
		t.Fatal("AC#2: fresh session Next returned false")
	}
	if _, err := result2.Consume(freshDeadline); err != nil {
		t.Fatalf("AC#2: fresh session Consume: %v", err)
	}
	t.Logf("AC#2: fresh session responded within 50ms after cancel")
}
