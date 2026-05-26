package server_test

// e2e_discard_test.go — T844: DISCARD remaining rows.
//
// result.Consume drains and discards the remaining rows of an open result
// cursor. After Consume the session must be usable for a second query.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_Discard opens a 1 000-row result, reads the first 10 rows, then
// calls result.Consume to discard the rest. Verifies:
//
//  1. Session returns to Ready: a subsequent query on the same session succeeds.
//  2. Server-side cursor is released (no goroutine leak, enforced by TestMain).
//  3. Race-clean.
func TestE2E_Discard(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	result, err := session.Run(ctx,
		"UNWIND range(1, 1000) AS n RETURN n",
		nil,
	)
	if err != nil {
		t.Fatalf("session.Run: %v", err)
	}

	// Read only the first 10 rows.
	for i := 0; i < 10; i++ {
		if !result.Next(ctx) {
			t.Fatalf("result.Next returned false at row %d", i)
		}
	}

	// AC#1 proxy + AC#2: Consume discards remaining rows; cursor released.
	if _, err := result.Consume(ctx); err != nil {
		t.Fatalf("result.Consume: %v", err)
	}

	// AC#1: session in Ready state — a new query must succeed.
	result2, err := session.Run(ctx, "RETURN 42 AS n", nil)
	if err != nil {
		t.Fatalf("second session.Run after Discard: %v", err)
	}
	if !result2.Next(ctx) {
		t.Fatal("second result: Next returned false")
	}
	if v, ok := result2.Record().Get("n"); !ok || v.(int64) != 42 {
		t.Errorf("second result value: got %v, want 42", v)
	}
	if _, err := result2.Consume(ctx); err != nil {
		t.Fatalf("second result.Consume: %v", err)
	}
}
