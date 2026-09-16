package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	driverconfig "github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// repStats is one measurement window: the outcome of driving the configured
// load against the served listener exactly once.
//
// Every field is per-window, so a repetition can be compared with the next and
// a profiled window with an unprofiled one. The counter map holds the DELTA of
// the bolt.server.* counters across the window (see [diffCounters]), never the
// process totals, because the counters themselves cannot be reset.
type repStats struct {
	// ConnOffered is how many independent connections the client tried to
	// open; it is zero in pooled mode, where the count is the driver pool's
	// business and only the server's accepted counter knows the truth.
	ConnOffered     int `json:"conn_offered"`
	ConnEstablished int `json:"conn_established"`
	ConnFailed      int `json:"conn_failed"`

	CountPerson   int64 `json:"count_person"`
	QueriesOK     int   `json:"queries_ok"`
	QueriesFailed int   `json:"queries_failed"`

	// Workload names the shape this window drove and RowsPerQuery how many
	// RECORDs one reply carried, so a record can never be read back without
	// knowing what it measured. RecordsOK is the total number of RECORDs the
	// window consumed and verified; for every shape but the RECORD-heavy one it
	// equals QueriesOK.
	Workload     string `json:"workload"`
	RowsPerQuery int    `json:"rows_per_query"`
	RecordsOK    int64  `json:"records_ok"`

	// WriteDelta and WriteExpected are the write shape's committed-effect
	// check: the amount the summed :Counter total actually moved across this
	// window, read back through the engine, against the number of units that
	// reported success. They are equal or the run fails; they are recorded
	// rather than merely asserted so the evidence travels with the measurement.
	// Both are zero for every read shape.
	WriteDelta    int64 `json:"write_delta"`
	WriteExpected int64 `json:"write_expected"`

	// ConnectNS is the wall-clock of the establish phase — from the first dial
	// to the moment every offered connection has either handshaken or failed.
	// LoadNS covers the query phase alone, so Throughput is not diluted by the
	// connection cost at the 1024 rung, where establishing dominates.
	ConnectNS  int64   `json:"connect_ns"`
	LoadNS     int64   `json:"load_ns"`
	Throughput float64 `json:"throughput_qps"`

	P50NS  int64 `json:"p50_ns"`
	P95NS  int64 `json:"p95_ns"`
	P99NS  int64 `json:"p99_ns"`
	P999NS int64 `json:"p999_ns"`

	// PeakGoroutines and PeakOpenFDs are read at the instant every offered
	// connection has resolved and the query phase has begun — the moment the
	// server holds the most sockets. MaxGoroutines and MaxOpenFDs are the
	// highest values a sampler saw anywhere in the window.
	PeakGoroutines int `json:"peak_goroutines"`
	PeakOpenFDs    int `json:"peak_open_fds"`
	MaxGoroutines  int `json:"max_goroutines"`
	MaxOpenFDs     int `json:"max_open_fds"`

	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`

	Counters map[string]uint64 `json:"counters,omitempty"`

	// CountersSettled is false when the server's live-connection derivation had
	// not returned to its pre-window value before the counter delta was read,
	// which means the delta under-counts the window's own closes. It is
	// recorded rather than hidden: a counter that has not settled is a weaker
	// observation, not an invalid run.
	CountersSettled bool `json:"counters_settled"`

	// FirstError is the first client-side failure of the window, recorded even
	// when the window was allowed to complete (a saturation run expects
	// failures and must not abort on them).
	FirstError string `json:"first_error,omitempty"`
}

// meanLatencyNS returns the window's mean per-query latency, derived from the
// query-phase wall-clock and the number of queries that succeeded. It is the
// ns/op value the benchstat record carries.
func (r *repStats) meanLatencyNS() int64 {
	if r.QueriesOK <= 0 {
		return 0
	}
	return r.LoadNS / int64(r.QueriesOK)
}

// driveOnce runs one measurement window against the server at addr and reports
// what it observed. atPeak is called exactly once, from driveOnce's own
// goroutine, at the moment every offered connection has resolved and the query
// phase is under way; it is where the caller samples the live process.
//
// The mode follows cfg.connections: zero drives the shared driver pool
// (the example's original shape), and one or more drives that many independent
// connections.
func driveOnce(ctx context.Context, addr string, cfg *config, atPeak func()) (repStats, error) {
	if cfg.connections > 0 {
		return driveConnections(ctx, addr, cfg, atPeak)
	}
	return drivePooled(ctx, addr, cfg, atPeak)
}

// ─────────────────────────────────────────────────────────────────────────────
// Pooled mode — one driver, one pool, cfg.sessions concurrent sessions
// ─────────────────────────────────────────────────────────────────────────────

// drivePooled connects a single neo4j-go-driver client to the server at addr
// and fires cfg.queries instances of the fixed count query, spread across
// cfg.sessions concurrent sessions over ONE connection pool. It records every
// successful query's latency, verifies each returns the known :Person count,
// and reports throughput plus the latency distribution. The driver, every
// session, and every worker goroutine are torn down before it returns, so it
// leaks no goroutine. It honours ctx cancellation.
//
// How many sockets this opens is the driver pool's decision, not the caller's,
// which is precisely why it cannot drive the server to its MaxConnections
// bound: see [driveConnections].
func drivePooled(ctx context.Context, addr string, cfg *config, atPeak func()) (repStats, error) {
	driver, err := neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth(),
		func(c *driverconfig.Config) {
			c.MaxConnectionPoolSize = cfg.sessions
			c.SocketConnectTimeout = cfg.dialTimeout()
			c.ConnectionAcquisitionTimeout = cfg.dialTimeout()
		})
	if err != nil {
		return repStats{}, fmt.Errorf("driver: %w", err)
	}
	defer driver.Close(ctx) //nolint:errcheck // best-effort close on teardown

	// Verify connectivity once up front so a connection failure is reported
	// here rather than as a confusing per-query error storm.
	connectStart := time.Now()
	if err := driver.VerifyConnectivity(ctx); err != nil {
		return repStats{}, fmt.Errorf("verify connectivity: %w", err)
	}
	connectNS := time.Since(connectStart).Nanoseconds()

	// Spread cfg.queries as evenly as possible across cfg.sessions workers.
	perWorker := splitWork(cfg.queries, cfg.sessions)

	// The shape is resolved ONCE and shared read-only by every worker, so no
	// worker pays for parsing a flag and every worker drives the same
	// statement.
	wl := cfg.workloadSpec()

	var (
		mu        sync.Mutex      // guards latencies and the first worker error
		latencies []time.Duration // one entry per successful query
		firstErr  error
		okCount   atomic.Int64
		recCount  atomic.Int64
	)
	latencies = make([]time.Duration, 0, cfg.queries)

	start := time.Now()
	var wg sync.WaitGroup
	for slot, n := range perWorker {
		if n == 0 {
			continue
		}
		wg.Add(1)
		go func(count, slot int) {
			defer wg.Done()
			local, ok, recs, err := runWorker(ctx, driver, count, wl, slot)
			mu.Lock()
			latencies = append(latencies, local...)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
			okCount.Add(int64(ok))
			recCount.Add(recs)
		}(n, slot)
	}

	// Every worker is running: this is the window's live peak.
	atPeak()

	wg.Wait()
	elapsed := time.Since(start)

	if firstErr != nil {
		return repStats{}, firstErr
	}

	stats := repStats{
		CountPerson:  wl.wantCount,
		QueriesOK:    int(okCount.Load()),
		RecordsOK:    recCount.Load(),
		Workload:     wl.kind,
		RowsPerQuery: wl.rows,
		ConnectNS:    connectNS,
		LoadNS:       elapsed.Nanoseconds(),
	}
	stats.finish(latencies)
	return stats, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Connection mode — N independent connections, the MaxConnections dimension
// ─────────────────────────────────────────────────────────────────────────────

// driveConnections opens cfg.connections INDEPENDENT connections to the server
// at addr and drives the fixed query over all of them.
//
// # Why one driver per connection
//
// The server bounds concurrency with a semaphore of MaxConnections slots and
// refuses the overflow (bolt/server/serve.go, the select on s.sem in Serve).
// Reaching that bound requires the client to control the socket count exactly,
// and a shared driver pool does not offer that control: it opens as many
// connections as its own scheduling decides and reuses an idle one in
// preference to dialling. Each connection here is therefore its own driver with
// MaxConnectionPoolSize 1, so the count of live server-side connections is
// exactly cfg.connections and nothing else.
//
// # The establish barrier
//
// Every connection handshakes first and then WAITS for all the others to
// resolve before any query is sent. Without the barrier the early connections
// would finish their share and release their semaphore slots while the late
// ones were still dialling, so the server would never hold cfg.connections at
// once and a saturation run would silently admit every connection it was
// supposed to refuse.
//
// # Tolerated failures
//
// When more connections are offered than the semaphore admits
// (cfg.expectRejections), a failed establish is the experiment's own result and
// is counted, not returned. Otherwise any failure is returned as an error.
func driveConnections(ctx context.Context, addr string, cfg *config, atPeak func()) (repStats, error) {
	n := cfg.connections
	share := splitWork(cfg.queries, n)
	wl := cfg.workloadSpec()
	tolerate := cfg.expectRejections()

	var (
		mu        sync.Mutex
		latencies = make([]time.Duration, 0, cfg.queries)
		firstErr  error

		established atomic.Int64
		failed      atomic.Int64
		okCount     atomic.Int64
		failedQ     atomic.Int64
		recCount    atomic.Int64
	)
	record := func(local []time.Duration, err error) {
		mu.Lock()
		defer mu.Unlock()
		latencies = append(latencies, local...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// gate is closed once every offered connection has handshaken or failed.
	// queryStart is written by the gate goroutine before the close and read by
	// everyone after it, so the close provides the happens-before edge.
	var (
		gate       = make(chan struct{})
		estWG      sync.WaitGroup
		gateWG     sync.WaitGroup
		queryStart time.Time
	)
	estWG.Add(n)
	connectStart := time.Now()
	gateWG.Add(1)
	go func() {
		defer gateWG.Done()
		estWG.Wait()
		queryStart = time.Now()
		close(gate)
	}()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		count := share[i]
		// Each connection owns one write slot, so no two explicit write
		// transactions ever touch the same :Counter node and the shape measures
		// the transaction path rather than MVCC conflict resolution.
		slot := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			driver, err := newConnDriver(addr, cfg)
			if err != nil {
				failed.Add(1)
				estWG.Done()
				if !tolerate {
					record(nil, fmt.Errorf("driver: %w", err))
				}
				return
			}
			defer driver.Close(ctx) //nolint:errcheck // best-effort close on teardown

			// VerifyConnectivity performs the handshake and RETURNS the
			// connection to the pool rather than closing it, so the socket
			// stays open — which is what makes the barrier below hold
			// cfg.connections sockets against the server's semaphore.
			if err := driver.VerifyConnectivity(ctx); err != nil {
				failed.Add(1)
				estWG.Done()
				if !tolerate {
					record(nil, fmt.Errorf("connect: %w", err))
				}
				return
			}
			established.Add(1)
			estWG.Done()

			<-gate
			if err := ctx.Err(); err != nil {
				record(nil, err)
				return
			}

			// cfg.sessions sessions in turn over this one connection: the
			// socket count stays fixed at cfg.connections while the session
			// lifecycle (HELLO-less re-use, BEGIN/RESET bookkeeping) is
			// exercised cfg.sessions times per connection.
			local := make([]time.Duration, 0, count)
			for _, chunk := range splitWork(count, cfg.sessionsPerConn()) {
				if chunk == 0 {
					continue
				}
				lat, ok, recs, err := runWorker(ctx, driver, chunk, wl, slot)
				local = append(local, lat...)
				okCount.Add(int64(ok))
				recCount.Add(recs)
				if err != nil {
					failedQ.Add(int64(chunk - ok))
					record(local, err)
					return
				}
			}
			record(local, nil)
		}()
	}

	<-gate
	connectNS := queryStart.Sub(connectStart).Nanoseconds()

	// Every offered connection has resolved and the admitted ones are querying:
	// this is the window's live peak, and the only instant at which the
	// server holds every connection the run offered it.
	atPeak()

	wg.Wait()
	loadNS := time.Since(queryStart).Nanoseconds()
	gateWG.Wait()

	stats := repStats{
		ConnOffered:     n,
		ConnEstablished: int(established.Load()),
		ConnFailed:      int(failed.Load()),
		CountPerson:     wl.wantCount,
		QueriesOK:       int(okCount.Load()),
		QueriesFailed:   int(failedQ.Load()),
		RecordsOK:       recCount.Load(),
		Workload:        wl.kind,
		RowsPerQuery:    wl.rows,
		ConnectNS:       connectNS,
		LoadNS:          loadNS,
	}
	stats.finish(latencies)

	if firstErr != nil {
		stats.FirstError = firstErr.Error()
		if !tolerate {
			return stats, firstErr
		}
	}
	return stats, nil
}

// newConnDriver builds a driver that owns exactly one connection.
//
// The two timeouts are the difference between a saturation run that completes
// and one that appears to hang: the driver's default acquisition timeout is 60 s,
// so without them every connection the server refuses would stall the run for a
// minute before reporting the refusal.
//
// Telemetry is disabled so the wire carries only the messages the experiment
// intends — the driver's telemetry message would otherwise be dispatched by the
// server on the very path being profiled.
func newConnDriver(addr string, cfg *config) (neo4j.DriverWithContext, error) {
	return neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth(),
		func(c *driverconfig.Config) {
			c.MaxConnectionPoolSize = 1
			c.SocketConnectTimeout = cfg.dialTimeout()
			c.ConnectionAcquisitionTimeout = cfg.dialTimeout()
			c.TelemetryDisabled = true
		})
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared workers and statistics
// ─────────────────────────────────────────────────────────────────────────────

// runWorker opens one driver session and drives count units of w over it,
// returning the per-query latencies, how many units succeeded, and how many
// RECORDs they consumed in total. It stops early on the first error (including
// ctx cancellation) and always closes its session before returning. Reusing a
// single session for the whole worker keeps the connection hot, which is what a
// real client pool does.
//
// slot is the worker's write slot, used by the explicit-write shape alone and
// ignored by every other.
//
// The session's access mode comes from the shape, not from a constant: an
// explicit WRITE transaction opened on a read-mode session is refused by the
// server, which treats BEGIN's mode field as a capability restriction rather
// than as a routing hint (bolt/server/session.go, handleBegin).
func runWorker(ctx context.Context, driver neo4j.DriverWithContext, count int, w *workload, slot int) ([]time.Duration, int, int64, error) {
	sess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: w.mode})
	defer sess.Close(ctx) //nolint:errcheck // best-effort close on teardown

	// Built once per worker, not once per unit: the driver serialises the map
	// into the outbound message and retains no reference to it, and the client
	// shares this process's cores with the server it is measuring.
	params := w.params(slot)

	latencies := make([]time.Duration, 0, count)
	ok := 0
	var records int64
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return latencies, ok, records, err
		}
		qStart := time.Now()
		n, err := runUnit(ctx, sess, w, params)
		if err != nil {
			return latencies, ok, records, fmt.Errorf("query %d: %w", i, err)
		}
		latencies = append(latencies, time.Since(qStart))
		records += int64(n)
		ok++
	}
	return latencies, ok, records, nil
}

// finish fills the derived fields of a window — throughput, the latency
// percentiles, and the live heap — from the raw latencies. It is the single
// place those derivations happen, so the pooled and the per-connection driver
// cannot drift apart in how they are computed.
//
// It is called after the window's clock has stopped, which is what lets it
// force a collection for an accurate live-heap reading without perturbing any
// number the window reports.
func (r *repStats) finish(latencies []time.Duration) {
	r.P50NS, r.P95NS, r.P99NS, r.P999NS = percentiles(latencies)
	if r.LoadNS > 0 {
		r.Throughput = float64(r.QueriesOK) / (float64(r.LoadNS) / float64(time.Second))
	}
	r.HeapAllocBytes = readMem().HeapAlloc
}

// splitWork divides total into parts buckets as evenly as possible: the first
// total%parts buckets get one extra unit. It is used to spread the query load
// across the session workers.
func splitWork(total, parts int) []int {
	if parts <= 0 {
		return nil
	}
	out := make([]int, parts)
	base, extra := total/parts, total%parts
	for i := range out {
		out[i] = base
		if i < extra {
			out[i]++
		}
	}
	return out
}

// percentiles returns the p50, p95, p99, and p999 of the given latencies, in
// nanoseconds. It sorts a copy so the caller's slice is left untouched; an
// empty input yields zeros.
//
// p999 is carried because it is the percentile a connection-saturation run
// actually moves: at 1024 connections the median is dominated by the queue that
// every request waits in, and only the far tail shows the requests that waited
// behind a scheduling decision rather than behind their turn.
func percentiles(latencies []time.Duration) (p50, p95, p99, p999 int64) {
	if len(latencies) == 0 {
		return 0, 0, 0, 0
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return int64(quantile(sorted, 0.50)), int64(quantile(sorted, 0.95)),
		int64(quantile(sorted, 0.99)), int64(quantile(sorted, 0.999))
}

// quantile returns the q-quantile (0 <= q <= 1) of an already-sorted,
// non-empty slice using the nearest-rank method.
func quantile(sorted []time.Duration, q float64) time.Duration {
	idx := int(q * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
