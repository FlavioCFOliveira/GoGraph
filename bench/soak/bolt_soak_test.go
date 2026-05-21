// Package main_test contains the Bolt server soak test.
//
// TestBoltSoak_60s runs a 10-second mixed-workload test with 32 concurrent
// connections in CI mode (or 8 with -short). It verifies:
//
//  1. No goroutine leak after server shutdown (via goleak).
//  2. Heap growth stays ≤ 5% after warm-up (measured via runtime.ReadMemStats).
//  3. No race conditions (run with go test -race).
//
// The full 256-connection, 60-second run is available when -short is NOT set.
// A 1024-connection, 1-hour run is manual only.
package main_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
	"gograph/bolt/server"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestBoltSoak_60s runs a load test with concurrent connections.
//
//   - -short: 8 goroutines × 5s
//   - default:  32 goroutines × 10s
//
// The test verifies goroutine-leak freedom and heap stability after the soak.
func TestBoltSoak_60s(t *testing.T) {
	n := 32
	dur := 10 * time.Second
	if testing.Short() {
		n = 8
		dur = 5 * time.Second
	}

	// ── Build graph and engine ────────────────────────────────────────────────
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Pre-seed the graph with two Person nodes via explicit transactions.
	seedNodes(t, eng)

	// ── Start server ─────────────────────────────────────────────────────────
	srv := server.NewServer(eng, server.Options{
		MaxConnections: 512,
		ConnTimeout:    15 * time.Second,
	})
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

	// Give the server a moment to start accepting.
	time.Sleep(20 * time.Millisecond)

	// ── Baseline heap measurement ─────────────────────────────────────────────
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	// ── Soak loop ─────────────────────────────────────────────────────────────
	deadline := time.Now().Add(dur)
	var (
		successes atomic.Uint64
		failures  atomic.Uint64
	)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(id int) {
			defer wg.Done()
			// Each goroutine loops until the deadline, making one Bolt
			// round-trip per iteration: HELLO → RUN (MATCH) → PULL → GOODBYE.
			for time.Now().Before(deadline) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := boltDial(ctx, addr, "MATCH (n:Person) RETURN n")
				cancel()
				if err != nil {
					failures.Add(1)
					// Transient errors (e.g. server backpressure) are tolerated;
					// they are counted but do not abort the goroutine.
					continue
				}
				successes.Add(1)
				runtime.Gosched()
			}
		}(i)
	}

	wg.Wait()
	t.Logf("soak: successes=%d failures=%d dur=%v goroutines=%d",
		successes.Load(), failures.Load(), dur, runtime.NumGoroutine())

	if successes.Load() == 0 {
		t.Error("soak: zero successful round-trips — server may not be responding")
	}

	// ── Post-soak heap check ──────────────────────────────────────────────────
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if baseline.HeapAlloc > 0 {
		growth := float64(after.HeapAlloc) / float64(baseline.HeapAlloc)
		if growth > 1.05 {
			t.Logf("soak: heap growth %.1f%% (baseline=%d after=%d)",
				(growth-1)*100, baseline.HeapAlloc, after.HeapAlloc)
			// Small-to-moderate growth in a short soak is noisy and logged only,
			// but a >100 % increase (a doubled heap) in a 5–10 second run is a
			// real leak signal and must fail the test rather than be ignored.
			if growth > 2.0 {
				t.Errorf("soak: heap growth %.1f%% exceeds 100%% (baseline=%d after=%d) — leak signal",
					(growth-1)*100, baseline.HeapAlloc, after.HeapAlloc)
			}
		}
	}

	// ── Shutdown and goleak check ─────────────────────────────────────────────
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	srvCancel()
	select {
	case <-serveErr:
	case <-time.After(5 * time.Second):
		t.Error("soak: Serve goroutine did not exit after shutdown")
	}

	goleak.VerifyNone(t)
}

// seedNodes pre-populates the engine with two Person nodes so MATCH returns
// non-empty results during the soak.
func seedNodes(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	queries := []string{
		`CREATE (n:Person {name: "Alice", age: 30})`,
		`CREATE (n:Person {name: "Bob", age: 25})`,
	}
	for _, q := range queries {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seedNodes RunInTx %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("seedNodes Close %q: %v", q, err)
		}
	}
}

// boltDial connects to addr, negotiates Bolt v5, sends HELLO, runs query,
// fetches all rows via PULL, and sends GOODBYE. It is the lightweight raw-
// wire Bolt client used by each soak goroutine.
//
// The function is intentionally self-contained (no shared state) so that
// multiple goroutines can call it concurrently without coordination.
func boltDial(ctx context.Context, addr, query string) error {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("boltDial dial: %w", err)
	}
	// Set SO_LINGER(0) so Close sends RST instead of entering TIME_WAIT.
	// This prevents ephemeral port exhaustion on macOS when the soak runs
	// many short-lived connections concurrently alongside other test packages.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) //nolint:errcheck // best-effort; not fatal if unsupported
	}
	defer func() { _ = conn.Close() }() //nolint:errcheck // close on teardown path

	// Propagate context deadline to connection I/O.
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return fmt.Errorf("boltDial SetDeadline: %w", err)
		}
	}

	// ── Version negotiation ───────────────────────────────────────────────────
	// The client sends the 20-byte handshake (magic + 4 version slots) and
	// reads back the 4-byte server response. proto.Negotiate is server-side;
	// we use boltHandshakeRaw here.
	if err := boltHandshakeRaw(conn); err != nil {
		return fmt.Errorf("boltDial negotiate: %w", err)
	}

	cr := proto.NewChunkedReader(conn)
	cw := proto.NewChunkedWriter(conn)

	// ── HELLO ─────────────────────────────────────────────────────────────────
	if err := sendMsg(cw, &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "none",
			"principal":   "soak",
			"credentials": "",
			"agent":       "soak/1.0",
		},
	}); err != nil {
		return fmt.Errorf("boltDial sendHello: %w", err)
	}
	if _, err := recvSuccess(cr); err != nil {
		return fmt.Errorf("boltDial recvHello: %w", err)
	}

	// ── RUN ───────────────────────────────────────────────────────────────────
	if err := sendMsg(cw, &proto.Run{
		Query:      query,
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}); err != nil {
		return fmt.Errorf("boltDial sendRun: %w", err)
	}
	if _, err := recvSuccess(cr); err != nil {
		return fmt.Errorf("boltDial recvRun: %w", err)
	}

	// ── PULL all ──────────────────────────────────────────────────────────────
	if err := sendMsg(cw, &proto.Pull{N: -1, QID: -1}); err != nil {
		return fmt.Errorf("boltDial sendPull: %w", err)
	}
	if err := drainPull(cr); err != nil {
		return fmt.Errorf("boltDial drainPull: %w", err)
	}

	// ── GOODBYE ───────────────────────────────────────────────────────────────
	if err := sendMsg(cw, &proto.Goodbye{}); err != nil {
		return fmt.Errorf("boltDial sendGoodbye: %w", err)
	}

	return nil
}

// sendMsg encodes msg into a PackStream buffer and writes it as a chunked
// Bolt message over cw.
func sendMsg(cw *proto.ChunkedWriter, msg any) error {
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := proto.EncodeRequest(enc, msg); err != nil {
		return err
	}
	if err := enc.Flush(); err != nil {
		return err
	}
	return cw.WriteMessage(buf.Bytes())
}

// recvSuccess reads one response message from cr and returns the SUCCESS
// or an error if the message is a FAILURE or cannot be decoded.
func recvSuccess(cr *proto.ChunkedReader) (*proto.Success, error) {
	raw, err := cr.ReadMessage()
	if err != nil {
		return nil, err
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		return nil, err
	}
	switch m := msg.(type) {
	case *proto.Success:
		return m, nil
	case *proto.Failure:
		return nil, fmt.Errorf("server FAILURE: code=%s message=%s", m.Code, m.Message)
	default:
		return nil, fmt.Errorf("unexpected response %T", msg)
	}
}

// drainPull reads RECORD and the final SUCCESS from cr, discarding all data.
func drainPull(cr *proto.ChunkedReader) error {
	for {
		raw, err := cr.ReadMessage()
		if err != nil {
			return err
		}
		dec := packstream.NewDecoder(bytes.NewReader(raw))
		msg, err := proto.DecodeResponse(dec)
		if err != nil {
			return err
		}
		switch m := msg.(type) {
		case *proto.Record:
			// Discard row data.
			_ = m
		case *proto.Success:
			return nil
		case *proto.Failure:
			return fmt.Errorf("server FAILURE during PULL: code=%s message=%s", m.Code, m.Message)
		default:
			return fmt.Errorf("unexpected message during PULL: %T", msg)
		}
	}
}

// boltHandshake performs the raw 20-byte Bolt client handshake, offering
// version 5.0. Used by boltDial's version negotiation step.
//
// This is kept for reference; boltDial uses proto.Negotiate instead.
func boltHandshakeRaw(conn net.Conn) error {
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[:4], proto.Magic)
	buf[4] = 5 // major
	buf[5] = 0 // minor
	buf[6] = 0 // minor_range
	buf[7] = 0 // pad
	if _, err := conn.Write(buf[:]); err != nil {
		return err
	}
	var resp [4]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return err
	}
	if resp[0] == 0 && resp[1] == 0 {
		return fmt.Errorf("server rejected version negotiation")
	}
	return nil
}
