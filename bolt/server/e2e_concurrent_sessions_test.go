//go:build soak || nightly

package server_test

// e2e_concurrent_sessions_test.go — T875: 100 concurrent sessions (soak layer).
//
// Spawns 100 goroutines each opening an independent session, running a query,
// and verifying the result. All goroutines use the same driver.
//
// Acceptance criteria:
//  1. All 100 sessions complete without error.
//  2. Race-clean (enforced by -race).
//  3. goleak-clean (enforced by TestMain).
//  4. p99 latency logged for documentation (not a hard gate in this test).
//  5. Layer: soak (//go:build soak).

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// TestE2E_ConcurrentSessions100 exercises 100 concurrent sessions.
func TestE2E_ConcurrentSessions100(t *testing.T) {
	const sessions = 100

	ctx := context.Background()

	addr := startTestServer(t, server.Options{
		ConnTimeout:    10 * time.Second,
		MaxConnections: sessions + 10,
	})
	bigDriver, err := neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.NoAuth(),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = sessions
			c.ConnectionAcquisitionTimeout = 30 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() {
		if err := bigDriver.Close(context.Background()); err != nil {
			t.Logf("bigDriver.Close: %v", err)
		}
	})

	type result struct {
		dur time.Duration
		err error
	}
	results := make([]result, sessions)
	var wg sync.WaitGroup

	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start := time.Now()

			sess := bigDriver.NewSession(ctx, neo4j.SessionConfig{})
			defer sess.Close(ctx) //nolint:errcheck

			res, err := sess.Run(ctx,
				"RETURN $i AS n",
				map[string]any{"i": int64(idx)},
			)
			if err != nil {
				results[idx] = result{err: err}
				return
			}
			if !res.Next(ctx) {
				results[idx] = result{err: res.Err()}
				return
			}
			v, ok := res.Record().Get("n")
			if !ok || v.(int64) != int64(idx) {
				results[idx] = result{err: res.Err()}
				return
			}
			if _, err := res.Consume(ctx); err != nil {
				results[idx] = result{err: err}
				return
			}
			results[idx] = result{dur: time.Since(start)}
		}(i)
	}

	wg.Wait()

	// AC#1: all sessions must have completed without error.
	var failures int
	latencies := make([]time.Duration, 0, sessions)
	for i, r := range results {
		if r.err != nil {
			t.Errorf("session %d error: %v", i, r.err)
			failures++
		} else {
			latencies = append(latencies, r.dur)
		}
	}
	if failures > 0 {
		t.Fatalf("%d/%d sessions failed", failures, sessions)
	}

	// AC#4: log p99 latency.
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p99idx := int(float64(len(latencies)) * 0.99)
	if p99idx >= len(latencies) {
		p99idx = len(latencies) - 1
	}
	t.Logf("p99 latency across %d concurrent sessions: %v", sessions, latencies[p99idx])
}
