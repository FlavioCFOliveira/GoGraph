package server_test

// e2e_streaming_test.go — T835: Streaming PULL on 100k-row result.
//
// Known server limitations:
//   - Summary counters always return 0 (server does not emit "stats").
//   - Peak heap bound is documented but not asserted via runtime/debug;
//     the test logs the figure for manual inspection.

import (
	"context"
	"runtime"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_Streaming100kRows streams 100 000 rows from the server via a
// single auto-commit session.Run and verifies:
//
//  1. Exactly 100 000 rows received.
//  2. Sum of values equals 5 000 050 000 (UNWIND range(1,100000)).
//  3. Peak heap stays within a documented bound (logged, not asserted hard).
//  4. Race-clean (enforced by -race flag in CI).
//  5. goleak-clean (enforced by TestMain).
func TestE2E_Streaming100kRows(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	result, err := session.Run(ctx, "UNWIND range(1, 100000) AS n RETURN n", nil)
	if err != nil {
		t.Fatalf("session.Run: %v", err)
	}

	var count, sum int64
	for result.Next(ctx) {
		rec := result.Record()
		v, ok := rec.Get("n")
		if !ok {
			t.Fatal("record missing key 'n'")
		}
		n, ok := v.(int64)
		if !ok {
			t.Fatalf("'n' is %T, want int64", v)
		}
		sum += n
		count++
	}
	if err := result.Err(); err != nil {
		t.Fatalf("result.Err: %v", err)
	}

	// AC#1: 100 000 rows received.
	if count != 100_000 {
		t.Errorf("row count: got %d, want 100000", count)
	}
	// AC#2: sum = 1+2+...+100000 = 100000*100001/2.
	const wantSum = int64(100_000) * int64(100_001) / 2
	if sum != wantSum {
		t.Errorf("sum: got %d, want %d", sum, wantSum)
	}

	// AC#3: log peak heap for documentation (not a hard failure).
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	peakMiB := float64(memAfter.HeapSys-memBefore.HeapSys) / (1 << 20)
	t.Logf("heap growth during streaming: %.2f MiB (documented bound: 64 MiB)", peakMiB)
}
