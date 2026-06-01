//go:build soak

// Package main_test — Bolt + Cypher mixed soak test.
//
// TestBoltCypherMixed_Smoke verifies server stability under 32 concurrent
// connections running an 80/20 read/write mixed workload for 10 seconds
// (smoke) or 4 hours (SOAK_FULL=1).
//
// Each goroutine:
//   - 80%: auto-commit MATCH (n) RETURN count(n)
//   - 20%: explicit BEGIN → CREATE (n:BenchNode) → PULL → COMMIT
//
// Think-time between iterations is uniformly random in [1 ms, 10 ms] to
// model realistic client behaviour and prevent spinning.
//
// Verified invariants:
//   - No goroutine leak at teardown (goleak.VerifyNone).
//   - Successful round-trips > 0.
//   - Connection errors attributed to MaxConnections cap are tolerated.
package main_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestBoltCypherMixed_Smoke is the CI-friendly smoke variant (10 s, 32
// connections). Set SOAK_FULL=1 without -short to run the full 4-hour pass.
func TestBoltCypherMixed_Smoke(t *testing.T) {
	nConns := 32
	dur := 10 * time.Second
	if !testing.Short() && os.Getenv("SOAK_FULL") == "1" {
		nConns = 1024
		dur = 4 * time.Hour
	}
	runBoltCypherMixed(t, nConns, dur)
}

// runBoltCypherMixed is the shared harness.
func runBoltCypherMixed(t *testing.T, nConns int, dur time.Duration) {
	t.Helper()

	// ── Build graph + engine ──────────────────────────────────────────────────
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Seed 20 nodes so MATCH count(n) returns a non-trivial result.
	for i := range 20 {
		res, err := eng.RunInTx(context.Background(),
			fmt.Sprintf(`CREATE (n:Node {id: %d})`, i), nil)
		if err != nil {
			t.Fatalf("seed node %d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("seed close %d: %v", i, err)
		}
	}

	// ── Start server ──────────────────────────────────────────────────────────
	maxConn := nConns + 64 // 64-slot headroom above the soak concurrency
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: maxConn,
		ConnTimeout:    15 * time.Second,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	srvCtx, srvCancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(srvCtx, ln)
	}()

	// Allow the server a moment to accept connections.
	time.Sleep(20 * time.Millisecond)

	// ── Soak loop ─────────────────────────────────────────────────────────────
	deadline := time.Now().Add(dur)

	var (
		successes atomic.Uint64
		failures  atomic.Uint64
		capErrors atomic.Uint64 // connection refused due to MaxConnections
	)

	var wg sync.WaitGroup
	wg.Add(nConns)
	for i := range nConns {
		go func(id int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(id)+1, 37)) //nolint:gosec // deterministic per goroutine
			var iter int
			for time.Now().Before(deadline) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				var dialErr error
				if iter%5 == 4 {
					// 20 %: explicit transaction write
					dialErr = boltDialWrite(ctx, addr, "CREATE (n:BenchNode)")
				} else {
					// 80 %: auto-commit read
					dialErr = boltDial(ctx, addr, "MATCH (n) RETURN count(n)")
				}
				cancel()
				iter++

				if dialErr != nil {
					failures.Add(1)
					if strings.Contains(dialErr.Error(), "refused") {
						capErrors.Add(1)
					}
					continue
				}
				successes.Add(1)

				// Think-time: uniform [1, 10] ms.
				time.Sleep(time.Duration(r.IntN(10)+1) * time.Millisecond)
				runtime.Gosched()
			}
		}(i)
	}

	wg.Wait()
	t.Logf("bolt_cypher_mixed: successes=%d failures=%d cap_errors=%d dur=%v goroutines=%d",
		successes.Load(), failures.Load(), capErrors.Load(), dur, runtime.NumGoroutine())

	if successes.Load() == 0 {
		t.Error("bolt_cypher_mixed: zero successful round-trips — server may not be responding")
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Errorf("bolt_cypher_mixed: shutdown: %v", err)
	}
	srvCancel()
	select {
	case <-serveErr:
	case <-time.After(10 * time.Second):
		t.Error("bolt_cypher_mixed: Serve goroutine did not exit after shutdown")
	}

	goleak.VerifyNone(t)
}
