// Package main_test — Bolt round-trip benchmark suite.
//
// BenchmarkBoltReadOnly, BenchmarkBoltWriteOnly, and BenchmarkBoltMixed
// measure end-to-end Bolt TCP round-trip latency for auto-committed queries
// at concurrency levels 1, 8, 64, 256, and 1024.
//
// Each sub-benchmark spins up a private server on a loopback port, seeds
// the graph, and then drives load via b.RunParallel with GOMAXPROCS set to
// the concurrency level under test. Because every sub-benchmark owns its
// server, results are independent.
//
// Usage:
//
//	go test -bench=BenchmarkBolt -benchmem -count=3 -timeout=300s ./bench/soak/...
//	go test -race -bench=BenchmarkBolt -benchmem -count=3 -timeout=300s ./bench/soak/...
package main_test

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// concurrencyLevels are the GOMAXPROCS values exercised by each Benchmark*.
var concurrencyLevels = [...]int{1, 8, 64, 256, 1024}

// BenchmarkBoltReadOnly measures auto-committed read latency.
// Query: MATCH (n) RETURN count(n).
func BenchmarkBoltReadOnly(b *testing.B) {
	const query = "MATCH (n) RETURN count(n)"
	for _, conc := range concurrencyLevels {
		conc := conc
		b.Run(fmt.Sprintf("conc=%d", conc), func(b *testing.B) {
			addr := newBenchServer(b)
			prev := runtime.GOMAXPROCS(conc)
			b.Cleanup(func() { runtime.GOMAXPROCS(prev) })
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := boltDial(ctx, addr, query); err != nil {
						b.Errorf("boltDial: %v", err)
					}
					cancel()
				}
			})
		})
	}
}

// BenchmarkBoltWriteOnly measures explicit-transaction write latency.
// Query: CREATE (n:BenchNode) via BEGIN / RUN / PULL / COMMIT.
//
// The Bolt server's auto-commit path only handles read queries (queries
// that produce a ProduceResults root). Write queries must go through an
// explicit transaction (BEGIN → RUN → PULL → COMMIT).
func BenchmarkBoltWriteOnly(b *testing.B) {
	const query = "CREATE (n:BenchNode)"
	for _, conc := range concurrencyLevels {
		conc := conc
		b.Run(fmt.Sprintf("conc=%d", conc), func(b *testing.B) {
			addr := newBenchServer(b)
			prev := runtime.GOMAXPROCS(conc)
			b.Cleanup(func() { runtime.GOMAXPROCS(prev) })
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := boltDialWrite(ctx, addr, query); err != nil {
						b.Errorf("boltDialWrite: %v", err)
					}
					cancel()
				}
			})
		})
	}
}

// BenchmarkBoltMixed measures an 80/20 read/write mixed workload.
//
// Each goroutine tracks its own iteration counter to achieve the 80/20 split
// without any shared atomic. Reads use the auto-commit path; writes use an
// explicit transaction (BEGIN / RUN / PULL / COMMIT).
func BenchmarkBoltMixed(b *testing.B) {
	const (
		readQuery  = "MATCH (n) RETURN count(n)"
		writeQuery = "CREATE (n:BenchNode)"
	)
	for _, conc := range concurrencyLevels {
		conc := conc
		b.Run(fmt.Sprintf("conc=%d", conc), func(b *testing.B) {
			addr := newBenchServer(b)
			prev := runtime.GOMAXPROCS(conc)
			b.Cleanup(func() { runtime.GOMAXPROCS(prev) })
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				var i int
				for pb.Next() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					var err error
					if i%5 == 4 {
						// 1 in 5 iterations → write (20 %)
						err = boltDialWrite(ctx, addr, writeQuery)
					} else {
						// 4 in 5 iterations → read (80 %)
						err = boltDial(ctx, addr, readQuery)
					}
					i++
					if err != nil {
						b.Errorf("boltDial iteration %d: %v", i, err)
					}
					cancel()
				}
			})
		})
	}
}

// boltDialWrite connects to addr, negotiates Bolt v5, sends HELLO, opens an
// explicit transaction with BEGIN, runs the write query, drains with PULL,
// commits with COMMIT, and sends GOODBYE.
//
// Write queries cannot use the auto-commit RUN path because the server's
// auto-commit executor only handles queries that produce a ProduceResults root
// (i.e. queries that return rows). Mutations (CREATE, MERGE without RETURN)
// must be wrapped in an explicit transaction.
func boltDialWrite(ctx context.Context, addr, query string) error {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("boltDialWrite dial: %w", err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) //nolint:errcheck // best-effort SO_LINGER(0)
	}
	defer func() { _ = conn.Close() }() //nolint:errcheck // close on teardown

	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return fmt.Errorf("boltDialWrite SetDeadline: %w", err)
		}
	}

	if err := boltHandshakeRaw(conn); err != nil {
		return fmt.Errorf("boltDialWrite negotiate: %w", err)
	}

	cr := proto.NewChunkedReader(conn)
	cw := proto.NewChunkedWriter(conn)

	// HELLO
	if err := sendMsg(cw, &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "none",
			"principal":   "bench",
			"credentials": "",
			"agent":       "bench/1.0",
		},
	}); err != nil {
		return fmt.Errorf("boltDialWrite sendHello: %w", err)
	}
	if _, err := recvSuccess(cr); err != nil {
		return fmt.Errorf("boltDialWrite recvHello: %w", err)
	}

	// BEGIN — open an explicit transaction
	if err := sendMsg(cw, &proto.Begin{
		Extra: map[string]interface{}{},
	}); err != nil {
		return fmt.Errorf("boltDialWrite sendBegin: %w", err)
	}
	if _, err := recvSuccess(cr); err != nil {
		return fmt.Errorf("boltDialWrite recvBegin: %w", err)
	}

	// RUN
	if err := sendMsg(cw, &proto.Run{
		Query:      query,
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}); err != nil {
		return fmt.Errorf("boltDialWrite sendRun: %w", err)
	}
	if _, err := recvSuccess(cr); err != nil {
		return fmt.Errorf("boltDialWrite recvRun: %w", err)
	}

	// PULL
	if err := sendMsg(cw, &proto.Pull{N: -1, QID: -1}); err != nil {
		return fmt.Errorf("boltDialWrite sendPull: %w", err)
	}
	if err := drainPull(cr); err != nil {
		return fmt.Errorf("boltDialWrite drainPull: %w", err)
	}

	// COMMIT
	if err := sendMsg(cw, &proto.Commit{}); err != nil {
		return fmt.Errorf("boltDialWrite sendCommit: %w", err)
	}
	if _, err := recvSuccess(cr); err != nil {
		return fmt.Errorf("boltDialWrite recvCommit: %w", err)
	}

	// GOODBYE
	if err := sendMsg(cw, &proto.Goodbye{}); err != nil {
		return fmt.Errorf("boltDialWrite sendGoodbye: %w", err)
	}

	return nil
}

// newBenchServer starts a fresh Bolt server backed by a pre-seeded graph and
// registers a b.Cleanup that shuts it down after the sub-benchmark completes.
// It returns the loopback address the server is listening on.
func newBenchServer(b *testing.B) string {
	b.Helper()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Seed a handful of nodes so read queries return non-trivially.
	for i := range 16 {
		res, err := eng.RunInTx(context.Background(),
			fmt.Sprintf(`CREATE (n:BenchSeed {id: %d})`, i), nil)
		if err != nil {
			b.Fatalf("newBenchServer seed %d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			b.Fatalf("newBenchServer seed close %d: %v", i, err)
		}
	}

	// MaxConnections is set to max(concurrencyLevel) + headroom.  Because we
	// share one server across all goroutines in the sub-benchmark, we pick a
	// ceiling that comfortably covers any concurrency level we test.
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: 1200,
		ConnTimeout:    15 * time.Second,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		b.Fatalf("newBenchServer NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("newBenchServer listen: %v", err)
	}
	addr := ln.Addr().String()

	srvCtx, srvCancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(srvCtx, ln)
	}()

	// Allow the server a moment to accept connections before load starts.
	time.Sleep(5 * time.Millisecond)

	b.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			b.Logf("newBenchServer shutdown: %v", err)
		}
		srvCancel()
		select {
		case <-serveErr:
		case <-time.After(10 * time.Second):
			b.Log("newBenchServer: Serve goroutine did not exit after shutdown")
		}
	})

	return addr
}
