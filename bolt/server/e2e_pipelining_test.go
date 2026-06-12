//go:build soak || nightly

package server_test

// e2e_pipelining_test.go — T889: 10k queries in single connection (pipelining, soak).
//
// Issues 10 000 sequential queries through a single session and verifies:
//  1. All 10 000 responses are delivered in issue order.
//  2. No message corruption (response values match expected).
//  3. Throughput is logged for documentation.
//  4. Race-clean (enforced by -race).
//  5. goleak-clean (enforced by TestMain).
//  6. Layer: soak (//go:build soak).
//
// Note: the neo4j-go-driver does not support true client-side pipelining
// (sending multiple requests before awaiting their responses) via the public
// session API. Each session.Run is issued and its result consumed sequentially.
// The test verifies that the connection handles 10k request/response cycles
// correctly without message interleaving or corruption.

import (
	"context"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_Pipelining10k issues 10 000 sequential queries on one session.
func TestE2E_Pipelining10k(t *testing.T) {
	const queryCount = 10_000

	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	start := time.Now()

	for i := 0; i < queryCount; i++ {
		result, err := session.Run(ctx,
			"RETURN $i AS n",
			map[string]any{"i": int64(i)},
		)
		if err != nil {
			t.Fatalf("query %d: session.Run: %v", i, err)
		}
		if !result.Next(ctx) {
			t.Fatalf("query %d: Next returned false", i)
		}
		v, ok := result.Record().Get("n")
		if !ok {
			t.Fatalf("query %d: record missing key 'n'", i)
		}
		got, ok := v.(int64)
		if !ok {
			t.Fatalf("query %d: 'n' is %T, want int64", i, v)
		}
		// AC#1 + AC#2: response in issue order, value not corrupted.
		if got != int64(i) {
			t.Fatalf("query %d: got n=%d, want %d (message corruption or out-of-order delivery)", i, got, i)
		}
		if _, err := result.Consume(ctx); err != nil {
			t.Fatalf("query %d: Consume: %v", i, err)
		}
	}

	elapsed := time.Since(start)

	// AC#3: log throughput.
	qps := float64(queryCount) / elapsed.Seconds()
	t.Logf("AC#3: %d queries in %v (%.0f q/s)", queryCount, elapsed, qps)
}
