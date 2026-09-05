// Package boltwire benchmarks the Bolt server over a REAL TCP socket, driven by
// the official neo4j-go-driver/v5 client.
//
// # Why this exists alongside bench/contention
//
// Every committed Bolt workload in bench/contention runs over [sim.SimListener],
// an in-memory net.Conn pair. That is the right instrument for lock attribution
// — it is the genuine server code with the kernel taken out of the picture — but
// it makes three costs of the real server invisible or wrong:
//
//   - conn.SetWriteDeadline is called before EVERY response message, including
//     every RECORD row (bolt/server/serve.go:1409, reached per record through the
//     streaming sink at serve.go:1035). On a *net.TCPConn that reaches the
//     runtime netpoller; on a SimConn it takes a mutex and broadcasts a condvar
//     (internal/sim/simconn.go:193). Neither is the other.
//   - the chunked writer's flush policy (sendResponse, serve.go:1510) decides how
//     many write(2) syscalls a K-row result costs. Over a pipe there is no
//     syscall to count.
//   - the reader goroutine blocks in a real epoll/kqueue wait rather than on a
//     condvar, so the two-goroutines-per-connection handoff has a different cost.
//
// # What may and may not be concluded
//
// The client runs in the SAME PROCESS as the server, so ns/op is a round trip
// including the driver, and -benchmem counts the driver's allocations alongside
// the server's. Neither figure is a server-side cost on its own. What IS valid
// is the DIFFERENCE between two arms whose client half is identical: only the
// server option changes, so the delta is the server's.
//
// # Running
//
//	go test -run='^$' -bench=BenchmarkBolt -benchmem -count=6 ./bench/boltwire/
//
// Compare arms with benchstat. Every benchmark here is opt-in by construction: a
// Benchmark never runs under `go test` without -bench, so this file costs the
// short layer only its compilation.
package boltwire

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
)

// rigOptions are the single variables the arms below change. Everything not
// named here is identical across every arm, which is what makes a delta
// attributable.
type rigOptions struct {
	// connTimeout is server.Options.ConnTimeout. A positive value arms the
	// per-message read AND write deadlines; zero disables both
	// (bolt/server/serve.go:1114 and :1408 both guard on `> 0`).
	connTimeout time.Duration
	// maxOpenTxPerPrincipal is server.Options.MaxOpenTxPerPrincipal. A negative
	// value disables the per-principal quota mutex entirely
	// (bolt/server/txquota.go:56).
	maxOpenTxPerPrincipal int
	// realMetrics installs the Prometheus backend instead of the shipped no-op.
	realMetrics bool
	// seedNodes is how many :N nodes the engine holds.
	seedNodes int
}

// defaultRig mirrors what examples/23_bolt_server configures
// (examples/23_bolt_server/main.go:198-202): a 30 s ConnTimeout, no TLS, no
// auth, and the shipped no-op metrics backend.
func defaultRig() rigOptions {
	return rigOptions{connTimeout: 30 * time.Second, seedNodes: 2000}
}

// rig is a running Bolt server on a real loopback TCP socket plus a connected
// driver.
type rig struct {
	driver  neo4j.DriverWithContext
	srv     *server.Server
	cancel  context.CancelFunc
	served  chan error
	addr    string
	metrics bool
}

// newRig starts the server and the driver, and registers the teardown.
func newRig(tb testing.TB, opts rigOptions) *rig {
	tb.Helper()

	if opts.realMetrics {
		metrics.SetBackend(prometheus.New())
	}

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := range opts.seedNodes {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(id, "N"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(id, "v", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: 100_000})

	srv, err := server.NewServer(eng, server.Options{
		Auth: server.NoAuthHandler{},
		// Discard the server log. NewServer warns about NoAuthHandler and about
		// the absent TLS config on EVERY construction, and every arm builds its
		// own rig, so the default logger would interleave four lines into the
		// benchmark output per arm. Nothing on a clean benchmark path logs, so
		// this removes noise, not evidence: an arm that started failing would
		// show up as b.Error from runParallel, which is not routed through slog.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// MaxConnections is set well above any parallelism these benchmarks
		// reach, so no arm is measuring the accept-loop semaphore's refusal
		// path instead of the message path.
		MaxConnections:        4096,
		ConnTimeout:           opts.connTimeout,
		MaxOpenTxPerPrincipal: opts.maxOpenTxPerPrincipal,
	})
	if err != nil {
		tb.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()

	drv, err := neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth(),
		func(c *config.Config) {
			// Sized above every parallelism these benchmarks reach, so the
			// driver's own pool never becomes the queue under measurement.
			c.MaxConnectionPoolSize = 512
		})
	if err != nil {
		cancel()
		tb.Fatalf("NewDriver: %v", err)
	}
	if err := drv.VerifyConnectivity(ctx); err != nil {
		cancel()
		tb.Fatalf("VerifyConnectivity: %v", err)
	}

	r := &rig{driver: drv, srv: srv, cancel: cancel, served: served, addr: addr, metrics: opts.realMetrics}
	tb.Cleanup(func() { r.close(tb) })
	return r
}

func (r *rig) close(tb testing.TB) {
	tb.Helper()
	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.driver.Close(closeCtx); err != nil {
		tb.Errorf("driver close: %v", err)
	}
	if err := r.srv.Shutdown(closeCtx); err != nil {
		tb.Errorf("server shutdown: %v", err)
	}
	r.cancel()
	<-r.served
	if r.metrics {
		metrics.SetBackend(nil)
	}
}

// runParallel drives fn on every parallel worker, giving each its own driver
// session so no arm measures session sharing.
//
// b.SetParallelism(n) makes the worker count n*GOMAXPROCS, so the -cpu flag and
// this multiplier together set the concurrency. The session is created OUTSIDE
// the pb.Next() loop, so connection setup is amortised and the measured cost is
// the message exchange — bolt-connect-churn is the workload that measures setup.
func runParallel(b *testing.B, r *rig, mode neo4j.AccessMode, fn func(ctx context.Context, s neo4j.SessionWithContext) error) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := r.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: mode})
		defer func() { _ = s.Close(ctx) }()
		for pb.Next() {
			if err := fn(ctx, s); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// countQuery is the single-row read examples/23_bolt_server fires
// (examples/23_bolt_server/main.go:426). One RECORD, so the streaming sink runs
// exactly once per operation.
const countQuery = "MATCH (n:N) RETURN count(n) AS c"

// rowsQuery returns rowCount RECORDs, so the per-record path dominates.
const rowCount = 100

var rowsQuery = fmt.Sprintf("UNWIND range(1, %d) AS i RETURN i", rowCount)

// consume drains a result to its summary, which is what forces every RECORD
// across the wire. A benchmark that only issued the RUN would measure the
// request path and none of the response path.
func consume(ctx context.Context, s neo4j.SessionWithContext, q string) error {
	res, err := s.Run(ctx, q, nil)
	if err != nil {
		return err
	}
	if _, err := res.Collect(ctx); err != nil {
		return err
	}
	return nil
}

// ── Arm 1: the per-message socket deadline ───────────────────────────────────
//
// writeResponse sets a write deadline before EVERY response message
// (bolt/server/serve.go:1408-1410) and the reader sets a read deadline before
// every read (serve.go:1113-1118). Both are guarded on ConnTimeout > 0, so
// setting it to zero removes exactly those calls and changes nothing else.
//
// ConnTimeout=0 is NOT a proposal — it removes the idle bound that protects the
// server against a stalled client. It is a MEASUREMENT of what the bound costs.

func BenchmarkBolt_Count_Deadline(b *testing.B) {
	r := newRig(b, defaultRig())
	runParallel(b, r, neo4j.AccessModeRead, func(ctx context.Context, s neo4j.SessionWithContext) error {
		return consume(ctx, s, countQuery)
	})
}

func BenchmarkBolt_Count_NoDeadline(b *testing.B) {
	o := defaultRig()
	o.connTimeout = 0
	r := newRig(b, o)
	runParallel(b, r, neo4j.AccessModeRead, func(ctx context.Context, s neo4j.SessionWithContext) error {
		return consume(ctx, s, countQuery)
	})
}

// ── Arm 2: the same pair over a 100-ROW result ───────────────────────────────
//
// The deadline is per MESSAGE, so a 100-row result pays it 102 times (RUN
// SUCCESS + 100 RECORDs + PULL SUCCESS) against 3 for the count query. If the
// deadline costs anything, the delta must grow with the row count; if it does
// not, the deadline is not what the delta measures.

func BenchmarkBolt_Rows_Deadline(b *testing.B) {
	r := newRig(b, defaultRig())
	runParallel(b, r, neo4j.AccessModeRead, func(ctx context.Context, s neo4j.SessionWithContext) error {
		return consume(ctx, s, rowsQuery)
	})
}

func BenchmarkBolt_Rows_NoDeadline(b *testing.B) {
	o := defaultRig()
	o.connTimeout = 0
	r := newRig(b, o)
	runParallel(b, r, neo4j.AccessModeRead, func(ctx context.Context, s neo4j.SessionWithContext) error {
		return consume(ctx, s, rowsQuery)
	})
}

// ── Arm 3: the explicit-transaction bookkeeping ──────────────────────────────
//
// An explicit read transaction takes the process-global txRegistry mutex four
// times (mint, register, one update per message, unregister — see
// bolt/server/txregistry.go) and the process-global txQuota mutex twice, on top
// of the autocommit path's zero. The -NoQuota arm disables the quota through the
// documented negative value and changes nothing else.

func BenchmarkBolt_ExplicitTx_Quota(b *testing.B) {
	r := newRig(b, defaultRig())
	runParallel(b, r, neo4j.AccessModeRead, explicitTxOp)
}

func BenchmarkBolt_ExplicitTx_NoQuota(b *testing.B) {
	o := defaultRig()
	o.maxOpenTxPerPrincipal = -1
	r := newRig(b, o)
	runParallel(b, r, neo4j.AccessModeRead, explicitTxOp)
}

// BenchmarkBolt_Autocommit_Baseline is the control the two explicit-tx arms are
// read against: the same statement, same rig, no BEGIN/COMMIT and therefore
// neither global mutex.
func BenchmarkBolt_Autocommit_Baseline(b *testing.B) {
	r := newRig(b, defaultRig())
	runParallel(b, r, neo4j.AccessModeRead, func(ctx context.Context, s neo4j.SessionWithContext) error {
		return consume(ctx, s, "RETURN 1 AS one")
	})
}

// explicitTxOp runs one BEGIN / RUN / PULL / COMMIT round trip. The statement is
// RETURN 1 so the engine work inside the transaction is as close to nothing as
// the wire allows and what remains is the Bolt bookkeeping.
func explicitTxOp(ctx context.Context, s neo4j.SessionWithContext) error {
	tx, err := s.BeginTransaction(ctx)
	if err != nil {
		return err
	}
	res, err := tx.Run(ctx, "RETURN 1 AS one", nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if _, err := res.Collect(ctx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// ── Arm 4: what enabling the metrics backend costs the Bolt path ─────────────
//
// bolt/server emits through internal/metrics on every accept, every message and
// every transaction boundary. rmp #2698 measured that facade anti-scaling at
// 0.445x with the real backend installed; this pair measures what that does to
// Bolt specifically rather than inferring it.

func BenchmarkBolt_Count_MetricsOff(b *testing.B) {
	r := newRig(b, defaultRig())
	runParallel(b, r, neo4j.AccessModeRead, func(ctx context.Context, s neo4j.SessionWithContext) error {
		return consume(ctx, s, countQuery)
	})
}

func BenchmarkBolt_Count_MetricsOn(b *testing.B) {
	o := defaultRig()
	o.realMetrics = true
	r := newRig(b, o)
	runParallel(b, r, neo4j.AccessModeRead, func(ctx context.Context, s neo4j.SessionWithContext) error {
		return consume(ctx, s, countQuery)
	})
}
