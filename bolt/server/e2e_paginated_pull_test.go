package server_test

// e2e_paginated_pull_test.go — T839: Paginated PULL with explicit FetchSize.
//
// The neo4j-go-driver v5 fetches records in server-side batches when
// FetchSize is set. The driver handles pagination transparently: callers
// just call result.Next and receive records regardless of batch count.
//
// Known limitations:
//   - The number of individual PULL RPCs is not observable from the public
//     driver API. The test verifies total row count and summary delivery
//     instead of counting individual PULL messages.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_PaginatedPull streams 10 000 rows with FetchSize=500 and verifies:
//
//  1. Total row count equals 10 000.
//  2. Sum assertion holds (sum of 1..10000 = 50 005 000).
//  3. Summary is delivered after the last batch (Consume returns without error).
//  4. Race-clean.
//  5. goleak-clean.
func TestE2E_PaginatedPull(t *testing.T) {
	const total = 10_000
	const fetchSize = 500

	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{
		FetchSize: fetchSize,
	})
	defer session.Close(ctx) //nolint:errcheck

	result, err := session.Run(ctx,
		"UNWIND range(1, $n) AS i RETURN i",
		map[string]any{"n": int64(total)},
	)
	if err != nil {
		t.Fatalf("session.Run: %v", err)
	}

	var count, sum int64
	for result.Next(ctx) {
		rec := result.Record()
		v, ok := rec.Get("i")
		if !ok {
			t.Fatal("record missing key 'i'")
		}
		n, ok := v.(int64)
		if !ok {
			t.Fatalf("'i' is %T, want int64", v)
		}
		sum += n
		count++
	}
	if err := result.Err(); err != nil {
		t.Fatalf("result.Err: %v", err)
	}

	// AC#1 + AC#2: row count and sum.
	if count != total {
		t.Errorf("row count: got %d, want %d", count, total)
	}
	const wantSum = int64(total) * int64(total+1) / 2
	if sum != wantSum {
		t.Errorf("sum: got %d, want %d", sum, wantSum)
	}

	// AC#3: summary delivered after last batch (Consume must not error).
	summary, err := result.Consume(ctx)
	if err != nil {
		t.Fatalf("result.Consume (post-drain): %v", err)
	}
	t.Logf("summary.ResultAvailableAfter: %v", summary.ResultAvailableAfter())
}
