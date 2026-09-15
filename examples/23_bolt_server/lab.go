package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/examples/internal/exprof"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// Artefact basenames. They are fixed, never configurable, for the same reason
// exprof fixes cpu.pprof and heap.pprof: a reader, a script, or a later cycle
// must find any rung's evidence at the same path without consulting the flags
// that produced it. cpu.pprof and heap.pprof are exprof's and are not repeated
// here.
const (
	mutexProfileName     = "mutex.pprof"
	blockProfileName     = "block.pprof"
	goroutineProfileName = "goroutine.pprof"
	traceName            = "trace.out"
	metricsFileName      = "metrics.json"
	hostFileName         = "host.json"
	benchFileName        = "bench.txt"
	ladderFileName       = "ladder.json"
	runLogName           = "run.log"
)

const (
	// artefactDirPerm and artefactFilePerm keep the evidence owner-and-group
	// readable only: a CPU profile discloses the program's call graph and,
	// through it, its inputs.
	artefactDirPerm  = 0o750
	artefactFilePerm = 0o600

	// samplerInterval is how often the live process is sampled during a
	// measurement window. 25 ms is short enough to catch the goroutine and
	// descriptor peak of a 1024-connection establish phase and long enough that
	// the sampler's own cost (one runtime read and one directory listing) stays
	// far below the workload's.
	samplerInterval = 25 * time.Millisecond

	// quiescePoll and quiesceTimeout bound the wait for the server's
	// live-connection derivation to return to its pre-window value. The server
	// increments conn.closed from the connection goroutine's deferred cleanup,
	// which runs after the client has already gone, so a counter delta read the
	// instant the client returns would under-count the closes of its own
	// window.
	quiescePoll    = 2 * time.Millisecond
	quiesceTimeout = 5 * time.Second

	// connHeadroom is how many slots above the offered connection count the
	// semaphore gets when the operator does not set -max-connections. It leaves
	// room for the driver's own connectivity probe without ever admitting a
	// connection the run did not offer.
	connHeadroom = 4

	// connIdleTimeout is the per-connection idle deadline the served instance
	// is given. It must exceed the establish barrier's worst case: at the 1024
	// rung a connection handshakes and then waits for every other connection to
	// resolve before it sends its first query.
	connIdleTimeout = 120 * time.Second

	// benchmarkName is the benchstat record name. One name with a per-rung
	// sub-name is what lets `benchstat bench.txt` put the rungs side by side.
	benchmarkName = "BenchmarkBoltLab"
)

// defaultLadderLevels is the concurrency ladder the module publishes and
// measures at (CLAUDE.md, Reliability and Concurrency Mandates): 1, 8, 64, 256
// and 1024 concurrent connections.
const defaultLadderLevels = "1,8,64,256,1024"

// metricsSchema versions the machine-readable record so a later cycle can tell
// which fields a file was written with.
const metricsSchema = "gograph.examples.23_bolt_server.lab/v1"

// ─────────────────────────────────────────────────────────────────────────────
// Machine-readable records
// ─────────────────────────────────────────────────────────────────────────────

// configJSON is the run's configuration as written into metrics.json, so a
// measurement can never be read back without the parameters that produced it.
type configJSON struct {
	Nodes            int    `json:"nodes"`
	KnowsMin         int    `json:"knows_min"`
	KnowsMax         int    `json:"knows_max"`
	Queries          int    `json:"queries"`
	Sessions         int    `json:"sessions"`
	Seed             int64  `json:"seed"`
	Connections      int    `json:"connections"`
	MaxConnections   int    `json:"max_connections"`
	Repetitions      int    `json:"repetitions"`
	ConnectTimeout   string `json:"connect_timeout"`
	MutexFraction    int    `json:"mutex_profile_fraction"`
	BlockRate        int    `json:"block_profile_rate_ns"`
	ExpectRejections bool   `json:"expect_rejections"`
}

// runMetrics is one rung's complete record: what was asked for, what the host
// was, what the unprofiled repetitions measured, and what the profiled window
// observed.
//
// Effect and Probe are separate for a reason established in this repository by
// bench/contention/observatory.go: full-rate mutex and block profiling perturbs
// the very contention it measures, so a profiled window supplies the CAUSE
// (attribution per call site) and must never be quoted for the EFFECT
// (throughput, latency). The unprofiled repetitions in Effect are the only
// numbers that may be quoted as the module's performance.
type runMetrics struct {
	Schema    string     `json:"schema"`
	Label     string     `json:"label"`
	Config    configJSON `json:"config"`
	Host      hostInfo   `json:"host"`
	Effect    []repStats `json:"effect"`
	Probe     *repStats  `json:"probe,omitempty"`
	Artefacts []string   `json:"artefacts,omitempty"`
}

// rungSpec is one step of the ladder: a name, the connections offered, and the
// semaphore the server is given.
type rungSpec struct {
	name           string
	connections    int
	maxConnections int
}

// ladderRung is the parent's record of one child rung.
type ladderRung struct {
	Name           string      `json:"name"`
	Connections    int         `json:"connections"`
	MaxConnections int         `json:"max_connections"`
	Dir            string      `json:"dir"`
	OK             bool        `json:"ok"`
	Error          string      `json:"error,omitempty"`
	WallNS         int64       `json:"wall_ns"`
	Metrics        *runMetrics `json:"metrics,omitempty"`
}

// ladderReport indexes a whole sweep.
type ladderReport struct {
	Schema           string       `json:"schema"`
	Host             hostInfo     `json:"host"`
	Levels           []int        `json:"levels"`
	SaturationOffer  int          `json:"saturation_offer"`
	SaturationAdmit  int          `json:"saturation_admit"`
	Rungs            []ladderRung `json:"rungs"`
	FailedRungs      []string     `json:"failed_rungs,omitempty"`
	ChildPerRungNote string       `json:"child_per_rung_note"`
}

// childPerRungNote records why a sweep forks rather than looping in process.
const childPerRungNote = "each rung runs in a fresh child process: the mutex, block and " +
	"allocation profiles accumulate for a process's lifetime and the runtime offers no reset, " +
	"so two rungs measured in one process would contaminate each other"

// ─────────────────────────────────────────────────────────────────────────────
// One rung
// ─────────────────────────────────────────────────────────────────────────────

// runRung seeds the social graph described by cfg, starts a Bolt v5 server on
// an ephemeral port, drives the configured load against it cfg.reps() times
// plus one profiled window when an artefact directory is set, and writes a
// report to w before tearing everything down cleanly.
//
// Bare lines carry deterministic facts (counts and query results, reproducible
// for a fixed seed); lines prefixed with "# " carry volatile telemetry
// (throughput, latency percentiles, counters, heap) that varies per run and per
// machine. All output goes to w so a test can capture and assert on it; runRung
// returns wrapped errors rather than terminating the process, and honours ctx
// cancellation.
//
// The deterministic-fact contract holds while every offered connection is
// admitted. A saturation rung (cfg.expectRejections) deliberately loses the
// queries of the connections the server refuses, so its queries.ok is an
// observation, not an invariant.
func runRung(ctx context.Context, w io.Writer, cfg *config) error {
	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.knows=[%d,%d]\n", cfg.knowsMin, cfg.knowsMax)
	fmt.Fprintf(w, "config.queries=%d\n", cfg.queries)
	fmt.Fprintf(w, "config.sessions=%d\n", cfg.sessions)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)
	fmt.Fprintf(w, "config.connections=%d\n", cfg.connections)
	fmt.Fprintf(w, "config.max_connections=%d\n", cfg.effectiveMaxConnections())
	fmt.Fprintf(w, "config.repetitions=%d\n", cfg.reps())

	// Engine over an in-memory labelled property graph, seeded from cfg.
	// Directed + Multigraph are required for openCypher semantics — relationships
	// are directed, and CREATE always adds a relationship, including a parallel
	// edge between an existing node pair. Weightless drops the per-node edge-weight
	// column: Cypher has no edge-weight concept, so the []float64 holds no
	// information (every relationship is recorded with the zero weight).
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true, Weightless: true})
	eng := cypher.NewEngine(g)
	stats, err := seed(ctx, g, cfg)
	if err != nil {
		return fmt.Errorf("seed graph: %w", err)
	}
	fmt.Fprintf(w, "nodes.person=%d\n", stats.persons)
	fmt.Fprintf(w, "edges.knows=%d\n", stats.knowsEdges)

	// Bolt v5 server with a per-connection idle timeout. The explicit
	// NoAuthHandler{} value is the opt-in that lets this development example
	// run without credentials; the server is secure-by-default and otherwise
	// refuses to start with a nil Auth handler. A production deployment would
	// instead set Options.Auth to a real AuthHandler and start from
	// server.DefaultTLSConfig() with a certificate as Options.TLSConfig.
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: cfg.effectiveMaxConnections(),
		ConnTimeout:    connIdleTimeout,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	// Kernel-assigned port; ln.Addr() reveals the chosen port for the client,
	// so a parallel test run never collides on a fixed port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().String()

	// Serve in the background under a cancellable context so Serve exits
	// cleanly once the load finishes and the context is cancelled.
	serveCtx, serveCancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx, ln) }()

	// The server reaches its metrics through a process-global backend, so the
	// sink is installed for the duration of the rung and removed again. Without
	// it bolt.server.conn.rejected is emitted into the no-op default and the
	// only evidence that the semaphore refused anything is lost.
	sink := newCounterSink()
	restoreSink := installCounterSink(sink)

	host := newHostInfo()
	effect := make([]repStats, 0, cfg.reps())
	var probe *repStats
	loadErr := func() error {
		for i := 0; i < cfg.reps(); i++ {
			st, err := measureWindow(ctx, addr, cfg, sink, nil)
			if err != nil {
				return fmt.Errorf("repetition %d/%d: %w", i+1, cfg.reps(), err)
			}
			effect = append(effect, st)
		}
		if cfg.artifactDir == "" {
			return nil
		}
		st, err := captureProbe(ctx, w, addr, cfg, sink)
		if err != nil {
			return fmt.Errorf("probe window: %w", err)
		}
		probe = &st
		return nil
	}()

	// Graceful shutdown with a deadline, then cancel Serve and drain its
	// goroutine. Serve only returns after every connection goroutine has
	// finished, so the drain guarantees no leaked goroutine.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	shutErr := srv.Shutdown(shutCtx)
	shutCancel()
	serveCancel()
	serveDrain := <-serveErr

	// The sink stays installed across the drain so the closes the shutdown
	// itself performs are counted, and is removed before anything else in the
	// process can observe a stale backend.
	totals := sink.snapshot()
	restoreSink()
	host.finish()

	// Surface the first meaningful failure, preferring the client load.
	if loadErr != nil {
		return fmt.Errorf("load: %w", loadErr)
	}
	if shutErr != nil {
		return fmt.Errorf("shutdown: %w", shutErr)
	}
	if serveDrain != nil {
		return fmt.Errorf("serve: %w", serveDrain)
	}
	if len(effect) == 0 {
		return errors.New("load: no measurement window completed")
	}

	last := effect[len(effect)-1]

	// Deterministic facts: the fixed query's result over the seeded data (the
	// label-scan count equals the known node count) and how many of the fired
	// queries succeeded, both from the final unprofiled repetition.
	fmt.Fprintf(w, "q.count_person=%d\n", last.CountPerson)
	fmt.Fprintf(w, "queries.ok=%d\n", last.QueriesOK)

	reportWindow(w, "load", &last)
	reportHost(w, &host)
	fmt.Fprintf(w, "# counters.total.bolt.server.conn.accepted=%d\n", totals["bolt.server.conn.accepted"])
	fmt.Fprintf(w, "# counters.total.bolt.server.conn.rejected=%d\n", totals["bolt.server.conn.rejected"])
	fmt.Fprintf(w, "# counters.total.bolt.server.conn.closed=%d\n", totals["bolt.server.conn.closed"])
	if probe != nil {
		fmt.Fprintf(w, "# probe.throughput=%.0f queries/s (PROFILED — never quote as the module's throughput)\n", probe.Throughput)
	}

	if cfg.artifactDir != "" {
		m := &runMetrics{
			Schema: metricsSchema,
			Label:  cfg.rungLabel(),
			Config: cfg.asJSON(),
			Host:   host,
			Effect: effect,
			Probe:  probe,
			Artefacts: []string{
				exprof.CPUProfileName, exprof.HeapProfileName,
				mutexProfileName, blockProfileName, goroutineProfileName,
				traceName, metricsFileName, hostFileName, benchFileName,
			},
		}
		if err := writeRunArtefacts(cfg.artifactDir, m); err != nil {
			return fmt.Errorf("write artefacts: %w", err)
		}
		fmt.Fprintf(w, "# artifacts.dir=%s\n", cfg.artifactDir)
	}

	fmt.Fprintln(w, "# server shut down cleanly")
	return nil
}

// reportWindow prints one window's telemetry under the given prefix.
func reportWindow(w io.Writer, prefix string, s *repStats) {
	fmt.Fprintf(w, "# %s.connect_elapsed=%s\n", prefix, time.Duration(s.ConnectNS).Round(time.Microsecond))
	fmt.Fprintf(w, "# %s.elapsed=%s\n", prefix, time.Duration(s.LoadNS).Round(time.Millisecond))
	fmt.Fprintf(w, "# %s.throughput=%.0f queries/s\n", prefix, s.Throughput)
	fmt.Fprintf(w, "# %s.latency_p50=%s\n", prefix, time.Duration(s.P50NS).Round(time.Microsecond))
	fmt.Fprintf(w, "# %s.latency_p95=%s\n", prefix, time.Duration(s.P95NS).Round(time.Microsecond))
	fmt.Fprintf(w, "# %s.latency_p99=%s\n", prefix, time.Duration(s.P99NS).Round(time.Microsecond))
	fmt.Fprintf(w, "# %s.latency_p999=%s\n", prefix, time.Duration(s.P999NS).Round(time.Microsecond))
	fmt.Fprintf(w, "# conn.offered=%d\n", s.ConnOffered)
	fmt.Fprintf(w, "# conn.established=%d\n", s.ConnEstablished)
	fmt.Fprintf(w, "# conn.failed=%d\n", s.ConnFailed)
	for _, name := range serverCounters {
		if v, ok := s.Counters[name]; ok && (v > 0 || strings.HasPrefix(name, "bolt.server.conn.")) {
			fmt.Fprintf(w, "# counters.%s=%d\n", name, v)
		}
	}
	fmt.Fprintf(w, "# runtime.goroutines_peak=%d\n", s.PeakGoroutines)
	fmt.Fprintf(w, "# runtime.goroutines_max=%d\n", s.MaxGoroutines)
	fmt.Fprintf(w, "# runtime.open_fds_peak=%d\n", s.PeakOpenFDs)
	fmt.Fprintf(w, "# runtime.open_fds_max=%d\n", s.MaxOpenFDs)
	fmt.Fprintf(w, "# mem.heap_alloc=%s\n", humanBytes(s.HeapAllocBytes))
	if s.FirstError != "" {
		fmt.Fprintf(w, "# load.first_error=%s\n", strings.ReplaceAll(s.FirstError, "\n", " "))
	}
}

// reportHost prints the environment the run was measured in, including the idle
// verdict. A run whose host was not idle says so on its own output, not only in
// its artefact file.
func reportHost(w io.Writer, h *hostInfo) {
	fmt.Fprintf(w, "# host.cores=%d\n", h.NumCPU)
	fmt.Fprintf(w, "# host.gomaxprocs=%d\n", h.GOMAXPROCS)
	if h.LoadAvgReadable {
		fmt.Fprintf(w, "# host.loadavg_before=%.2f %.2f %.2f\n", h.LoadAvgBefore[0], h.LoadAvgBefore[1], h.LoadAvgBefore[2])
		if len(h.LoadAvgAfter) == 3 {
			fmt.Fprintf(w, "# host.loadavg_after=%.2f %.2f %.2f\n", h.LoadAvgAfter[0], h.LoadAvgAfter[1], h.LoadAvgAfter[2])
		}
	}
	if h.FDLimitReadable {
		fmt.Fprintf(w, "# host.fd_limit_soft=%d\n", h.FDLimitSoft)
	}
	fmt.Fprintf(w, "# host.idle=%t (%s)\n", h.Idle, h.IdleNote)
}

// ─────────────────────────────────────────────────────────────────────────────
// Measurement windows
// ─────────────────────────────────────────────────────────────────────────────

// measureWindow drives one load window and surrounds it with the observations
// that cannot be taken from inside it: a live sampler, a peak reading, and the
// server-counter delta.
//
// atPeak, when non-nil, is called at the window's peak, after this function's
// own peak reading, and is where the profiled window writes its goroutine
// profile.
func measureWindow(ctx context.Context, addr string, cfg *config, sink *counterSink, atPeak func()) (repStats, error) {
	before := sink.snapshot()
	baseLive := liveConnections(before)

	smp := startSampler(samplerInterval)
	var peakGoroutines, peakFDs int
	st, err := driveOnce(ctx, addr, cfg, func() {
		peakGoroutines = runtime.NumGoroutine()
		if n, ok := openFDs(); ok {
			peakFDs = n
		}
		if atPeak != nil {
			atPeak()
		}
	})
	maxGoroutines, maxFDs := smp.finish()
	if err != nil {
		return st, err
	}

	st.PeakGoroutines, st.PeakOpenFDs = peakGoroutines, peakFDs
	st.MaxGoroutines, st.MaxOpenFDs = maxGoroutines, maxFDs
	st.CountersSettled = quiesceConnections(sink, baseLive)
	st.Counters = diffCounters(before, sink.snapshot())
	return st, nil
}

// captureProbe runs one extra window with every profiler enabled and writes the
// five profiles and the trace into cfg.artifactDir.
//
// Ordering is load-bearing, and follows exprof's contract plus two additions of
// its own:
//
//   - the contention rates are raised BEFORE the window and dropped only after
//     the profiles are written, because both accumulate while the rate is set;
//   - the goroutine profile is written AT the window's peak, not after it. A
//     goroutine profile taken once the clients have gone shows an idle server
//     and answers nothing about a connection flood.
func captureProbe(ctx context.Context, w io.Writer, addr string, cfg *config, sink *counterSink) (repStats, error) {
	dir := cfg.artifactDir
	if err := os.MkdirAll(dir, artefactDirPerm); err != nil {
		return repStats{}, fmt.Errorf("artefact dir %q: %w", dir, err)
	}

	prevMutex := runtime.SetMutexProfileFraction(cfg.mutexFraction)
	defer func() {
		runtime.SetMutexProfileFraction(prevMutex)
		// The runtime exposes no getter for the block rate, so this is a reset
		// to the documented default of 0 (disabled), not a restore.
		runtime.SetBlockProfileRate(0)
	}()
	runtime.SetBlockProfileRate(cfg.blockRate)

	prof := &exprof.Config{Dir: dir, Trace: filepath.Join(dir, traceName)}
	sess, err := prof.Start()
	if err != nil {
		return repStats{}, err
	}

	var peakErr error
	st, runErr := measureWindow(ctx, addr, cfg, sink, func() {
		peakErr = writeProfile(dir, goroutineProfileName, "goroutine")
	})

	// Finish stops the CPU profile and the trace, collects, and writes the heap
	// profile. It is called whether or not the window failed, so a failed probe
	// still leaves readable artefacts.
	finErr := sess.Finish(w)

	var contErr error
	for _, p := range [...]struct{ file, name string }{
		{mutexProfileName, "mutex"},
		{blockProfileName, "block"},
	} {
		if err := writeProfile(dir, p.file, p.name); err != nil {
			contErr = err
			break
		}
	}
	if contErr == nil {
		fmt.Fprintf(w, "# pprof.mutex=%s\n", filepath.Join(dir, mutexProfileName))
		fmt.Fprintf(w, "# pprof.block=%s\n", filepath.Join(dir, blockProfileName))
		fmt.Fprintf(w, "# pprof.goroutine=%s\n", filepath.Join(dir, goroutineProfileName))
	}
	return st, errors.Join(runErr, finErr, peakErr, contErr)
}

// writeProfile writes the named runtime profile into dir under file.
func writeProfile(dir, file, name string) error {
	p := pprof.Lookup(name)
	if p == nil {
		return fmt.Errorf("pprof.Lookup(%q) returned nil", name)
	}
	//nolint:gosec // G304: the path is the operator-supplied -artifact-dir joined with a fixed basename.
	f, err := os.Create(filepath.Join(dir, file))
	if err != nil {
		return fmt.Errorf("create %s profile: %w", name, err)
	}
	if err := p.WriteTo(f, 0); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s profile: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s profile: %w", name, err)
	}
	return nil
}

// liveConnections derives the server's live-connection count from a counter
// snapshot, as bolt/server/metrics.go documents: accepted − closed.
//
// The arithmetic stays in uint64 and floors at zero rather than converting to a
// signed type. closed is incremented only for a connection that was accepted, so
// the difference cannot go negative on a consistent snapshot; a snapshot is read
// counter by counter, though, so the floor covers the one case where it could
// appear to — closed read after an increment that the accepted read missed.
func liveConnections(snap map[string]uint64) uint64 {
	accepted, closed := snap["bolt.server.conn.accepted"], snap["bolt.server.conn.closed"]
	if closed >= accepted {
		return 0
	}
	return accepted - closed
}

// quiesceConnections waits until the server's live-connection derivation
// returns to base, and reports whether it did so within the bound. A false
// result means the window's counter delta under-counts its own closes and is
// recorded as such rather than silently published.
func quiesceConnections(sink *counterSink, base uint64) bool {
	deadline := time.Now().Add(quiesceTimeout)
	for {
		if liveConnections(sink.snapshot()) <= base {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(quiescePoll)
	}
}

// sampler watches the live process during a measurement window and records the
// highest goroutine count and descriptor count it sees.
//
// It exists because the peak of a connection ladder is not where the window
// starts or ends: at the 1024 rung the establish phase is a transient in which
// goroutines and descriptors both spike and then settle, and a reading taken at
// either edge would miss it entirely.
type sampler struct {
	stop chan struct{}
	done chan struct{}

	// Written by the sampler goroutine only, and read by finish after the done
	// channel is closed, which supplies the happens-before edge.
	maxGoroutines int
	maxOpenFDs    int
}

// startSampler begins sampling every interval. Call finish to stop it and read
// what it saw; the goroutine is always joined, so it can leak none.
func startSampler(interval time.Duration) *sampler {
	s := &sampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				if n := runtime.NumGoroutine(); n > s.maxGoroutines {
					s.maxGoroutines = n
				}
				if n, ok := openFDs(); ok && n > s.maxOpenFDs {
					s.maxOpenFDs = n
				}
			}
		}
	}()
	return s
}

// finish stops the sampler, waits for its goroutine, and returns the maxima.
func (s *sampler) finish() (goroutines, fds int) {
	close(s.stop)
	<-s.done
	return s.maxGoroutines, s.maxOpenFDs
}

// ─────────────────────────────────────────────────────────────────────────────
// Artefacts
// ─────────────────────────────────────────────────────────────────────────────

// writeRunArtefacts writes the machine-readable record, the host record, and
// the benchstat file for one rung.
func writeRunArtefacts(dir string, m *runMetrics) error {
	if err := os.MkdirAll(dir, artefactDirPerm); err != nil {
		return fmt.Errorf("artefact dir %q: %w", dir, err)
	}
	if err := writeJSONFile(filepath.Join(dir, metricsFileName), m); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, hostFileName), &m.Host); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, benchFileName), []byte(benchRecord(m)), artefactFilePerm)
}

// writeJSONFile writes v as indented JSON.
func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return os.WriteFile(path, append(b, '\n'), artefactFilePerm)
}

// readRunMetrics reads back what writeRunArtefacts wrote.
func readRunMetrics(dir string) (*runMetrics, error) {
	//nolint:gosec // G304: the path is the harness's own rung directory joined with a fixed basename.
	b, err := os.ReadFile(filepath.Join(dir, metricsFileName))
	if err != nil {
		return nil, err
	}
	var m runMetrics
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Join(dir, metricsFileName), err)
	}
	return &m, nil
}

// benchHeader is the environment preamble benchstat expects at the top of a
// benchmark file.
func benchHeader() string {
	return fmt.Sprintf("goos: %s\ngoarch: %s\npkg: %s\n",
		runtime.GOOS, runtime.GOARCH,
		"github.com/FlavioCFOliveira/GoGraph/examples/23_bolt_server")
}

// benchRecord renders one rung's UNPROFILED repetitions in Go benchmark format,
// one line per repetition, so `benchstat bench.txt` compares rungs — and two
// sweeps of the same rung — with the spread it needs.
//
// The profiled window is deliberately absent: its throughput is the probe's,
// not the module's.
func benchRecord(m *runMetrics) string {
	var b strings.Builder
	b.WriteString(benchHeader())
	for i := range m.Effect {
		s := &m.Effect[i]
		fmt.Fprintf(&b, "%s/%s-%d\t%d\t%d ns/op\t%.2f queries/sec\t%d p99-ns\t%d p999-ns\t%d connect-ns\t%d peak-goroutines\n",
			benchmarkName, m.Label, m.Host.GOMAXPROCS,
			s.QueriesOK, s.meanLatencyNS(), s.Throughput,
			s.P99NS, s.P999NS, s.ConnectNS, s.PeakGoroutines)
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// The ladder
// ─────────────────────────────────────────────────────────────────────────────

// runLadder sweeps the concurrency ladder in one invocation, running each rung
// in a fresh child process and writing one artefact directory per rung.
//
// # Why a child per rung
//
// The mutex, block, and allocation profiles accumulate for the lifetime of a
// process and the runtime exposes no way to reset them, so a second rung
// measured in the same process inherits the first one's samples and reports
// them as its own. A fresh child also gives each rung a cold heap, which
// matters as much: a rung that inherited a warm heap and a raised GC goal from
// its predecessor would be measured under conditions the next sweep could not
// reproduce.
//
// A failed rung does not abort the sweep — the remaining rungs still carry
// evidence, and a rung that fails at 1024 while succeeding at 256 is itself a
// finding. Every failure is recorded in ladder.json and reported in the error
// runLadder finally returns.
func runLadder(ctx context.Context, w io.Writer, cfg *config) error {
	levels, err := parseLevels(cfg.ladderLevels)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}
	if err := os.MkdirAll(cfg.artifactDir, artefactDirPerm); err != nil {
		return fmt.Errorf("artefact dir %q: %w", cfg.artifactDir, err)
	}

	fmt.Fprintf(w, "config.ladder=%s\n", strings.Join(itoaAll(levels), ","))
	fmt.Fprintf(w, "config.saturation_offer=%d\n", cfg.satOffer)
	fmt.Fprintf(w, "config.saturation_admit=%d\n", cfg.satAdmit)
	fmt.Fprintf(w, "config.repetitions=%d\n", cfg.reps())

	host := newHostInfo()
	specs := ladderSpecs(levels, cfg.satOffer, cfg.satAdmit)
	rungs := make([]ladderRung, 0, len(specs))
	var failed []string
	for _, spec := range specs {
		r := runChildRung(ctx, w, cfg, exe, spec)
		rungs = append(rungs, r)
		if !r.OK {
			failed = append(failed, spec.name)
		}
	}
	host.finish()

	report := &ladderReport{
		Schema:           metricsSchema,
		Host:             host,
		Levels:           levels,
		SaturationOffer:  cfg.satOffer,
		SaturationAdmit:  cfg.satAdmit,
		Rungs:            rungs,
		FailedRungs:      failed,
		ChildPerRungNote: childPerRungNote,
	}
	if err := writeJSONFile(filepath.Join(cfg.artifactDir, ladderFileName), report); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(cfg.artifactDir, hostFileName), &host); err != nil {
		return err
	}
	if err := writeLadderBench(cfg.artifactDir, rungs); err != nil {
		return err
	}
	reportHost(w, &host)
	fmt.Fprintf(w, "# ladder.index=%s\n", filepath.Join(cfg.artifactDir, ladderFileName))
	fmt.Fprintf(w, "# ladder.bench=%s\n", filepath.Join(cfg.artifactDir, benchFileName))
	if len(failed) > 0 {
		return fmt.Errorf("ladder: %d of %d rungs failed: %s", len(failed), len(specs), strings.Join(failed, ", "))
	}
	fmt.Fprintln(w, "# ladder complete")
	return nil
}

// ladderSpecs turns the levels and the saturation pair into the ordered list of
// rungs a sweep runs. Every ladder rung is given a semaphore wide enough to
// admit every connection it offers, so a rejection anywhere on the ladder is a
// defect rather than the design; the saturation rung is the one that offers
// more than the semaphore admits.
func ladderSpecs(levels []int, satOffer, satAdmit int) []rungSpec {
	specs := make([]rungSpec, 0, len(levels)+1)
	for _, lvl := range levels {
		specs = append(specs, rungSpec{
			name:           "conn=" + strconv.Itoa(lvl),
			connections:    lvl,
			maxConnections: lvl + connHeadroom,
		})
	}
	if satOffer > 0 && satAdmit > 0 {
		specs = append(specs, rungSpec{
			name:           "saturation",
			connections:    satOffer,
			maxConnections: satAdmit,
		})
	}
	return specs
}

// runChildRung runs one rung as a child process and reads its record back.
func runChildRung(ctx context.Context, w io.Writer, cfg *config, exe string, spec rungSpec) ladderRung {
	dir := filepath.Join(cfg.artifactDir, spec.name)
	r := ladderRung{
		Name:           spec.name,
		Connections:    spec.connections,
		MaxConnections: spec.maxConnections,
		Dir:            dir,
	}
	if err := os.MkdirAll(dir, artefactDirPerm); err != nil {
		r.Error = err.Error()
		return r
	}
	//nolint:gosec // G304: the path is the harness's own rung directory joined with a fixed basename.
	logFile, err := os.OpenFile(filepath.Join(dir, runLogName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, artefactFilePerm)
	if err != nil {
		r.Error = err.Error()
		return r
	}

	//nolint:gosec // G204: exe is this program's own path from os.Executable, and every
	// argument is rendered from an already-validated config field.
	cmd := exec.CommandContext(ctx, exe, childArgs(cfg, spec, dir)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	start := time.Now()
	runErr := cmd.Run()
	r.WallNS = time.Since(start).Nanoseconds()
	closeErr := logFile.Close()

	switch {
	case runErr != nil:
		r.Error = fmt.Sprintf("child: %v (see %s)", runErr, filepath.Join(dir, runLogName))
	case closeErr != nil:
		r.Error = fmt.Sprintf("close %s: %v", runLogName, closeErr)
	}

	if m, err := readRunMetrics(dir); err != nil {
		if r.Error == "" {
			r.Error = fmt.Sprintf("read %s: %v", metricsFileName, err)
		}
	} else {
		r.Metrics = m
	}
	r.OK = r.Error == ""

	summariseRung(w, &r)
	return r
}

// summariseRung prints a one-line telemetry summary of a finished rung, so a
// sweep is readable as it runs rather than only once ladder.json is parsed.
func summariseRung(w io.Writer, r *ladderRung) {
	if !r.OK {
		fmt.Fprintf(w, "# rung.%s FAILED after %s: %s\n",
			r.Name, time.Duration(r.WallNS).Round(time.Millisecond), r.Error)
		return
	}
	last := repStats{}
	if r.Metrics != nil && len(r.Metrics.Effect) > 0 {
		last = r.Metrics.Effect[len(r.Metrics.Effect)-1]
	}
	fmt.Fprintf(w,
		"# rung.%s wall=%s established=%d/%d rejected=%d throughput=%.0f q/s p99=%s p999=%s goroutines=%d fds=%d\n",
		r.Name, time.Duration(r.WallNS).Round(time.Millisecond),
		last.ConnEstablished, last.ConnOffered, last.Counters["bolt.server.conn.rejected"],
		last.Throughput,
		time.Duration(last.P99NS).Round(time.Microsecond),
		time.Duration(last.P999NS).Round(time.Microsecond),
		last.PeakGoroutines, last.PeakOpenFDs)
}

// childArgs renders the flags for one rung's child process. Every dimension is
// passed explicitly — none is left to the child's own defaults — so a rung can
// be re-run by hand from the command line in ladder.json.
func childArgs(cfg *config, spec rungSpec, dir string) []string {
	return []string{
		"-nodes", strconv.Itoa(cfg.nodes),
		"-knows-min", strconv.Itoa(cfg.knowsMin),
		"-knows-max", strconv.Itoa(cfg.knowsMax),
		"-queries", strconv.Itoa(cfg.queries),
		"-sessions", strconv.Itoa(cfg.sessions),
		"-seed", strconv.FormatInt(cfg.seed, 10),
		"-connections", strconv.Itoa(spec.connections),
		"-max-connections", strconv.Itoa(spec.maxConnections),
		"-repetitions", strconv.Itoa(cfg.reps()),
		"-connect-timeout", cfg.dialTimeout().String(),
		"-mutex-fraction", strconv.Itoa(cfg.mutexFraction),
		"-block-rate", strconv.Itoa(cfg.blockRate),
		"-label", spec.name,
		"-artifact-dir", dir,
	}
}

// writeLadderBench concatenates every rung's benchmark records into one file at
// the sweep's root, so a single `benchstat <dir>/bench.txt` compares the whole
// ladder.
func writeLadderBench(dir string, rungs []ladderRung) error {
	var b strings.Builder
	b.WriteString(benchHeader())
	for _, r := range rungs {
		if r.Metrics == nil {
			continue
		}
		for _, line := range strings.Split(benchRecord(r.Metrics), "\n") {
			if strings.HasPrefix(line, benchmarkName) {
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
	}
	return os.WriteFile(filepath.Join(dir, benchFileName), []byte(b.String()), artefactFilePerm)
}

// parseLevels reads a comma-separated ladder specification into ascending,
// de-duplicated levels. It rejects anything that is not a positive integer, so
// a typo in the flag fails before a sweep spends an hour producing artefacts
// under the wrong name.
func parseLevels(spec string) ([]int, error) {
	fields := strings.Split(spec, ",")
	seen := make(map[int]struct{}, len(fields))
	levels := make([]int, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("ladder level %q: not an integer", f)
		}
		if n <= 0 {
			return nil, fmt.Errorf("ladder level %d: must be > 0", n)
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		levels = append(levels, n)
	}
	if len(levels) == 0 {
		return nil, fmt.Errorf("ladder %q: no levels", spec)
	}
	sort.Ints(levels)
	return levels, nil
}

// itoaAll renders a level list for the configuration echo.
func itoaAll(levels []int) []string {
	out := make([]string, len(levels))
	for i, n := range levels {
		out[i] = strconv.Itoa(n)
	}
	return out
}
