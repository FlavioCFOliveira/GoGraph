package contention

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
	"github.com/FlavioCFOliveira/GoGraph/internal/sim"
)

// Bolt workloads beyond the two the registry already carried.
//
// # The coverage gap these close
//
// Round 2 drove three Bolt-touching workloads: bolt-wire-read (autocommit read),
// bolt-connect-churn (connect, one autocommit read, disconnect) and
// dst-concurrent-bolt (the DST's own mixed driver). NONE of them holds an
// EXPLICIT transaction open across messages, and that is where bolt/server's own
// shared state lives:
//
//   - bolt/server/txregistry.go:104 — ONE process-global sync.Mutex over a
//     map[string]*txEntry. It is taken on BEGIN (nextID at :121 and register at
//     :134), on EVERY inbound message while a transaction is open (update at
//     :153, reached from Session.reportTx, bolt/server/session.go:739, which the
//     message loop calls after every dispatch at bolt/server/serve.go:1359), and
//     again at transaction end (unregister at :146).
//   - bolt/server/txquota.go:47 — a SECOND process-global sync.Mutex over a
//     map[principal]int, taken on every BEGIN (acquire at :81) and every
//     teardown (release at :95). Every connection of one application shares one
//     principal, so they all contend on ONE map key.
//
// An autocommit RUN reaches neither: Session.reportTx returns at its s.txID ==
// "" guard (session.go:740) and no BEGIN is issued. So the published Bolt rows
// measure a path that steps around both locks, and the module's own
// explicit-transaction surface was unmeasured.
//
// # Why the read-only transaction is the sharp probe
//
// bolt-tx-read opens a READ-ONLY explicit transaction and runs RETURN 1 in it.
// The statement itself is as close to free as the engine offers, so almost all
// of what the ladder measures is the Bolt session and registry bookkeeping
// around it. One operation sends four inbound messages — BEGIN, RUN, PULL,
// COMMIT — and costs six acquisitions of the registry mutex (nextID, register,
// one update per message while the transaction is open, unregister) plus two of
// the quota mutex, against an engine call that touches no node. A write transaction would drive
// the same Bolt locks but bury them under the engine write path, which
// docs/contention-inventory-round2.md already ranks separately; measuring both
// separates the layer under audit from the layer beneath it.
//
// # The -noquota arm is a SINGLE-VARIABLE A/B, not a fixture-replication ceiling
//
// Every other ceiling arm in this package removes SHARING by replicating the
// fixture. That construction cannot isolate one of two locks inside one server:
// replicating the server unshares the engine, the plan cache and the graph along
// with the registry, and the ratio would then price all of them together.
//
// server.Options.MaxOpenTxPerPrincipal already documents a NEGATIVE value as
// "disables enforcement" (bolt/server/serve.go:336-341), and newTxQuota honours
// it by returning a quota that records nothing (bolt/server/txquota.go:56-58).
// So bolt-tx-read-noquota is byte-identical to bolt-tx-read except that one
// mutex is never taken. It prices THAT lock and nothing else.
//
// Both arms are built through [sim.NewSimServerTxRegistry] rather than
// [sim.NewSimServer], because that constructor is the only one that takes
// maxOpenTxPerPrincipal. It also installs a discarding logger, which
// NewSimServer does not — so these two rows are NOT directly comparable with
// bolt-wire-read's, whose server logs through slog.Default(). They are
// comparable with EACH OTHER, which is what the A/B needs.
//
// # bolt-wire-rows exists because every committed Bolt workload returns one row
//
// bolt-wire-read runs "MATCH (n) RETURN count(n)" and bolt-connect-churn runs
// "RETURN 1". Both produce exactly ONE RECORD, so neither exercises the
// streaming sink (bolt/server/serve.go:1035) — the path that sets a write
// deadline per record (writeResponse, serve.go:1409) and relies on the chunked
// writer's deferred flush (sendResponse, serve.go:1510). A row-returning read is
// what puts that path under the ladder.
//
// # bolt-wire-read-metrics is a CONTROL, not a proposal
//
// rmp #2698 records that internal/metrics anti-scales at 0.445x once the real
// Prometheus backend is installed. bolt/server emits through that facade on
// every connection accept, every message and every transaction boundary
// (bolt/server/metrics.go:122 and its twelve call sites in serve.go and
// session.go). Any Bolt number taken with the backend installed may therefore be
// measuring internal/metrics rather than Bolt. This arm makes that separable
// instead of assumed: it is bolt-wire-read with the backend installed and
// nothing else changed.

// boltRowLimit is how many rows bolt-wire-rows asks for. It is well below the
// engine's result-row cap and large enough that the per-record path dominates
// the per-message one, without making the window so long the ladder becomes
// unaffordable.
const boltRowLimit = 100

// boltWorkloads returns the Bolt-surface workloads this audit added.
func boltWorkloads() []Workload {
	return []Workload{
		boltTxReadWorkload("bolt-tx-read", 0),
		boltTxReadWorkload("bolt-tx-read-noquota", -1),
		boltWireRowsWorkload(),
		boltWireReadMetricsWorkload(),
		boltWireRowsMetricsWorkload(),
		boltConnectChurnQuietWorkload(),
	}
}

// boltConnectChurnQuietWorkload is bolt-connect-churn with the server's log
// DISCARDED, and it exists to price a confound in the committed row rather than
// to propose anything.
//
// bolt-connect-churn builds its server with [sim.NewSimServer], which passes a
// nil logger, and [server.NewServer] then falls back to slog.Default()
// (bolt/server/serve.go:616-618). docs/contention-inventory-round2.md records
// that the committed workload reported 1691 / 30000 rejected connections at
// level 1024, and every one of them writes a WARN line
// ("bolt: max connections reached, rejecting", bolt/server/serve.go:776) through
// that default handler DURING the measured window. slog's default handler
// serialises on one mutex and writes to the process's stderr, which in a sweep
// child is a pipe to the parent.
//
// So the published 1024 cell may be partly measuring the log. This arm changes
// the logger and nothing else the workload drives, so the delta prices it.
//
// One difference is NOT under the harness's control and is recorded rather than
// hidden: [sim.NewSimServerTxRegistry] also calls [server.Server.SetClock],
// which REPLACES the server's transaction registry (internal/sim/simserver.go
// comments the reason at length). With clock.Real() on both sides the
// replacement is behaviourally equivalent, but the two arms are not
// byte-identical and the ratio should be read as "logging plus a clock install"
// rather than "logging alone".
func boltConnectChurnQuietWorkload() Workload {
	return Workload{
		Name:    "bolt-connect-churn-quiet",
		Surface: "bolt/server accept loop, session and tx registries [server log discarded]",
		Ops:     30000,
		Setup: func(_ string) (Op, func() error, error) {
			srv, err := sim.NewSimServerTxRegistry(
				sim.SimEngineForServer(),
				clock.Real(), clock.Real(),
				0, 0, 0,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("new sim server: %w", err)
			}
			op := func(ctx context.Context, _, _ int) error {
				c, err := srv.Dial()
				if err != nil {
					return fmt.Errorf("dial: %w", err)
				}
				defer func() { _ = c.Close() }()
				if err := c.Connect(ctx); err != nil {
					return fmt.Errorf("connect: %w", err)
				}
				if _, err := c.Run("RETURN 1", nil); err != nil {
					return err
				}
				_, _, err = c.PullAll()
				return err
			}
			return op, srv.Close, nil
		},
	}
}

// boltTxReadWorkload builds an explicit read-transaction workload over the
// genuine Bolt wire. quota is passed straight to
// server.Options.MaxOpenTxPerPrincipal: 0 takes the default (2048, enforcement
// ON) and a negative value disables enforcement.
func boltTxReadWorkload(name string, quota int) Workload {
	return Workload{
		Name: name,
		Surface: "bolt/server txregistry + txquota, bolt/proto, cypher " +
			fmt.Sprintf("[MaxOpenTxPerPrincipal=%d]", quota),
		// Measured, not guessed: sized in a pilot run so the level-1 window
		// clears one second. See the sizing note in All().
		Ops: 60000,
		Setup: func(_ string) (Op, func() error, error) {
			srv, err := sim.NewSimServerTxRegistry(
				sim.SimEngineForServer(),
				clock.Real(), // listener clock
				clock.Real(), // server clock
				0,            // maxTxIdle: take the default
				0,            // defaultTxTimeout: take the default
				quota,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("new sim server (quota=%d): %w", quota, err)
			}
			clients := newPerWorker[*sim.WireClient]()
			op := func(ctx context.Context, worker, _ int) error {
				c, err := boltClient(ctx, clients, srv, worker)
				if err != nil {
					return err
				}
				// A READ-ONLY transaction: the engine work inside it is as
				// close to nothing as the wire allows, so what the ladder
				// measures is the Bolt bookkeeping around it.
				if _, err := c.BeginMode("r"); err != nil {
					return fmt.Errorf("begin: %w", err)
				}
				if _, err := c.Run("RETURN 1", nil); err != nil {
					return fmt.Errorf("run: %w", err)
				}
				if _, _, err := c.PullAll(); err != nil {
					return fmt.Errorf("pull: %w", err)
				}
				if _, err := c.Commit(); err != nil {
					return fmt.Errorf("commit: %w", err)
				}
				return nil
			}
			return op, boltTeardown(clients, srv), nil
		},
	}
}

// boltWireRowsWorkload drives a MULTI-ROW autocommit read over the wire, so the
// per-record streaming sink is on the measured path.
func boltWireRowsWorkload() Workload {
	return Workload{
		Name:    "bolt-wire-rows",
		Surface: "bolt/server streaming sink, bolt/proto chunking, bolt/packstream encode",
		Ops:     20000,
		Setup: func(_ string) (Op, func() error, error) {
			srv, err := sim.NewSimServer(sim.SimEngineForServer(), clock.Real())
			if err != nil {
				return nil, nil, fmt.Errorf("new sim server: %w", err)
			}
			// The engine behind a SimServer starts empty, so a MATCH would
			// return nothing and the workload would measure an empty stream.
			// UNWIND generates the rows in the engine and needs no fixture,
			// which also keeps this workload independent of any seeding that
			// another workload's graph shape would impose.
			query := fmt.Sprintf("UNWIND range(1, %d) AS i RETURN i", boltRowLimit)
			clients := newPerWorker[*sim.WireClient]()
			op := func(ctx context.Context, worker, _ int) error {
				c, err := boltClient(ctx, clients, srv, worker)
				if err != nil {
					return err
				}
				if _, err := c.Run(query, nil); err != nil {
					return err
				}
				recs, _, err := c.PullAll()
				if err != nil {
					return err
				}
				// Assert the stream was actually produced. Without this the
				// workload would pass just as happily against a server that
				// returned an empty result, and would then be measuring the
				// message path it was written to bypass.
				if len(recs) != boltRowLimit {
					return fmt.Errorf("pull: got %d records, want %d", len(recs), boltRowLimit)
				}
				return nil
			}
			return op, boltTeardown(clients, srv), nil
		},
	}
}

// boltWireReadMetricsWorkload is bolt-wire-read with the real metrics backend
// installed. See the package note above for why it is a control.
func boltWireReadMetricsWorkload() Workload {
	return Workload{
		Name:    "bolt-wire-read-metrics",
		Surface: "bolt/server, bolt/proto, cypher read path [real metrics backend installed]",
		Ops:     100000,
		Setup: func(_ string) (Op, func() error, error) {
			metrics.SetBackend(prometheus.New())
			srv, err := sim.NewSimServer(sim.SimEngineForServer(), clock.Real())
			if err != nil {
				metrics.SetBackend(nil)
				return nil, nil, fmt.Errorf("new sim server: %w", err)
			}
			clients := newPerWorker[*sim.WireClient]()
			op := func(ctx context.Context, worker, _ int) error {
				c, err := boltClient(ctx, clients, srv, worker)
				if err != nil {
					return err
				}
				if _, err := c.Run("MATCH (n) RETURN count(n)", nil); err != nil {
					return err
				}
				_, _, err = c.PullAll()
				return err
			}
			inner := boltTeardown(clients, srv)
			teardown := func() error {
				err := inner()
				metrics.SetBackend(nil)
				return err
			}
			return op, teardown, nil
		},
	}
}

// boltWireRowsMetricsWorkload is bolt-wire-rows with the real metrics backend
// installed. It is the row-streaming counterpart of
// [boltWireReadMetricsWorkload] and exists for the same reason, plus one more
// that is specific to the per-message histograms (rmp #2715).
//
// A metric emission that is free when no backend is installed proves nothing:
// rmp #2698's whole finding was that the no-op path and the real path behave
// completely differently, and that the shape which looks obviously right — a
// cached metric handle — measured 0.081x, WORSE than the full lookup. So a
// per-message histogram added to bolt/server has to be priced on the path where
// it actually costs something, and bolt-wire-read-metrics alone prices only the
// one-row shape. This arm prices the hundred-row one, where the RUN/PULL pair
// carries a hundred RECORD writes between the two observations.
//
// It is byte-identical to bolt-wire-rows except for the backend install and its
// removal on teardown, exactly as bolt-wire-read-metrics is to bolt-wire-read.
func boltWireRowsMetricsWorkload() Workload {
	w := boltWireRowsWorkload()
	w.Name = "bolt-wire-rows-metrics"
	w.Surface = "bolt/server streaming sink, bolt/proto chunking, " +
		"bolt/packstream encode [real metrics backend installed]"
	inner := w.Setup
	w.Setup = func(dir string) (Op, func() error, error) {
		metrics.SetBackend(prometheus.New())
		op, teardown, err := inner(dir)
		if err != nil {
			metrics.SetBackend(nil)
			return nil, nil, err
		}
		return op, func() error {
			err := teardown()
			metrics.SetBackend(nil)
			return err
		}, nil
	}
	return w
}

// boltClient returns this worker's connected [sim.WireClient], dialling and
// completing the Bolt handshake on first use. Connections are per worker and
// long-lived, so the ladder measures the message path rather than connection
// setup — bolt-connect-churn is the workload that measures setup.
func boltClient(
	ctx context.Context,
	clients *perWorker[*sim.WireClient],
	srv *sim.SimServer,
	worker int,
) (*sim.WireClient, error) {
	slot, err := clients.get(worker)
	if err != nil {
		return nil, err
	}
	if *slot == nil {
		c, err := srv.Dial()
		if err != nil {
			return nil, fmt.Errorf("dial: %w", err)
		}
		if err := c.Connect(ctx); err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		*slot = c
	}
	return *slot, nil
}

// boltTeardown closes every per-worker connection and then the server. It is
// shared so the three workloads above cannot drift apart in how they tear down,
// which would make their windows incomparable for a reason that has nothing to
// do with the module.
func boltTeardown(clients *perWorker[*sim.WireClient], srv *sim.SimServer) func() error {
	return func() error {
		clients.each(func(c **sim.WireClient) {
			if *c != nil {
				_ = (*c).Close()
			}
		})
		return srv.Close()
	}
}
