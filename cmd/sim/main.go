// Command sim runs the GoGraph deterministic simulation testing (DST) harness.
//
// Usage:
//
//	go run ./cmd/sim [seed] [flags]
//
// With no seed argument a random seed is chosen (from a non-deterministic
// source used solely to pick the seed value) and printed so the run can be
// reproduced. Flags:
//
//	--ticks       number of ticks (operations) to simulate (default 100000)
//	--check-every invariant-check cadence in ticks (default 1 = every tick);
//	              >1 trades check coverage for speed on long runs. The terminal
//	              tick is always checked regardless, so a violation in the final
//	              ticks is never missed.
//	--workload    actor mix: default | write-heavy | read-heavy | bad-actor
//	--verbose     print each operation as it runs
//	--crashes     inject deterministic crash+recovery cycles
//	--checkpoint  enable in-loop checkpointing (full-stack WAL + snapshot store);
//	              periodically publishes a real snapshot and truncates the WAL
//	              prefix, so a crash recovers via the full snapshot+WAL path
//	--checkpoint-every  tick cadence between checkpoints (0 = default cadence)
//	--mode        engine | wire | concurrent | liveness (default "engine")
//	--conns       concurrent connections for the concurrent/liveness modes
//	--ops-per-conn  operations per connection for those modes
//	--scenario      run a named scenario from the catalogue (see --list-scenarios)
//	--list-scenarios  print the scenario catalogue and exit
//	--replay        re-run the seed in verbose full-trace debug; on a violation,
//	                shrink to a minimal reproducer and print it
//	--swarm         run a swarm of many derived seeds across a bounded worker
//	                pool, time/count-boxed; prints a summary (runs/passes/
//	                failures with reproduce lines) and exits non-zero on any
//	                failing seed. Tuned by --workers, --runs, --duration, and an
//	                optional --scenario filter; --bias steers selection toward
//	                under-covered scenarios; --coverage-report prints coverage.
//	                The leading positional seed is the swarm's MASTER seed, so
//	                the whole swarm reproduces from it.
//
// Scenario and replay modes (Phase 4):
//
//	--list-scenarios            prints the catalogue (name, mode, default seed)
//	--scenario=<name> [seed]    runs a named scenario; exit 1 on a violation
//	--replay=<seed> [--scenario=<name>]
//	                            re-runs a DETERMINISTIC workload for the seed,
//	                            printing every op; on a violation it shrinks the
//	                            recorded trace to a minimal reproducer (ddmin) and
//	                            prints it with the reproduce line, exiting 1
//
// The Phase-3 modes drive the REAL Bolt wire protocol against a genuine
// bolt/server over an in-memory connection:
//
//	wire        a deterministic LOCK-STEP single-connection round-trip demo
//	concurrent  N real goroutines, one per connection (NOT bit-reproducible);
//	            asserts eventual oracle==engine consistency, no panic, no leak
//	liveness    a two-phase safety->liveness run with convergence + a
//	            deadlock/resonance watchdog
//
// On a violation the report (which includes a "Reproduce with:" line) is
// printed to stderr and the process exits 1. On success a one-line summary is
// printed and the process exits 0.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/sim"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args, executes a simulation, and returns the process exit code. It
// is separated from main so it can be unit-tested with arbitrary writers.
func run(args []string, stdoutRaw, stderrRaw io.Writer) int {
	stdout := &errWriter{w: stdoutRaw}
	stderr := &errWriter{w: stderrRaw}

	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	fs.SetOutput(stderrRaw)
	ticks := fs.Int("ticks", 100000, "number of ticks (operations) to simulate")
	checkEvery := fs.Int("check-every", 1, "invariant-check cadence in ticks (1 = check every tick; >1 trades coverage for speed on long runs, the terminal tick is always checked)")
	workloadName := fs.String("workload", "default", "actor mix: default | write-heavy | read-heavy | bad-actor")
	verbose := fs.Bool("verbose", false, "print each operation as it runs")
	crashes := fs.Bool("crashes", false, "inject deterministic crash+recovery cycles (drives the real SimDisk-backed persistence stack)")
	checkpoint := fs.Bool("checkpoint", false, "enable in-loop checkpointing: open the store full-stack (WAL + snapshot) and periodically publish a real snapshot and truncate the WAL prefix, so a crash recovers via the full snapshot+WAL path")
	checkpointEvery := fs.Int("checkpoint-every", 0, "tick cadence between checkpoints when -checkpoint is set (0 = the default cadence)")
	mode := fs.String("mode", "engine", "execution mode: engine | wire | concurrent | liveness")
	conns := fs.Int("conns", 16, "concurrent connections (concurrent/liveness modes)")
	opsPerConn := fs.Int("ops-per-conn", 25, "operations per connection (concurrent/liveness modes)")
	scenario := fs.String("scenario", "", "run a named scenario from the catalogue (see -list-scenarios)")
	listScenarios := fs.Bool("list-scenarios", false, "print the scenario catalogue and exit")
	replay := fs.Bool("replay", false, "re-run the seed in verbose full-trace debug; on a violation, shrink to a minimal reproducer")
	swarm := fs.Bool("swarm", false, "run a swarm of many seeds across a bounded worker pool (time/count-boxed)")
	workers := fs.Int("workers", 0, "swarm worker-pool cap (0 = min(GOMAXPROCS, runs))")
	runs := fs.Int("runs", 200, "swarm seed-count budget (ignored when -duration is set without -runs)")
	duration := fs.Duration("duration", 0, "swarm wall-clock budget (e.g. 30s); 0 = bounded by -runs only")
	coverageReport := fs.Bool("coverage-report", false, "with -swarm: print the coverage summary; alone: print an empty coverage template and exit")
	bias := fs.Bool("bias", false, "with -swarm: bias scenario selection toward under-covered scenarios (coverage-driven)")
	injectDemoFault := fs.Bool("inject-demo-fault", false, "DEMO ONLY: inject a synthetic lost-write fault into the replayed trace to demonstrate shrinking")

	// Go's flag package stops parsing at the first non-flag token, so the
	// documented usage `sim <seed> --ticks=N` would otherwise leave --ticks
	// unparsed. Split the optional leading positional seed out first, then parse
	// the remaining tokens as flags, so flags work whether they precede or
	// follow the seed.
	seedArgs, flagArgs := splitSeedArg(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(fs.Args()) > 0 {
		stderr.printf("sim: unexpected arguments: %v\n", fs.Args())
		return 2
	}

	// -list-scenarios prints the catalogue and exits without needing a seed.
	if *listScenarios {
		return runListScenarios(stdout, stderr)
	}

	// -coverage-report WITHOUT -swarm prints the coverage template (every tracked
	// scenario bucket, all unexplored) and exits, so the report shape is
	// inspectable without running a swarm.
	if *coverageReport && !*swarm {
		return runCoverageTemplate(stdout, stderr)
	}

	seed, ok := resolveSeed(seedArgs, stderr)
	if !ok {
		return 2
	}

	// -swarm runs many derived seeds across a bounded worker pool, time/count
	// boxed, and reports failures + coverage. It uses the (positional or random)
	// seed as the swarm's MASTER seed, so the whole swarm reproduces from it.
	if *swarm {
		return runSwarmMode(seed, swarmOptions{
			scenario:       *scenario,
			workers:        *workers,
			runs:           *runs,
			duration:       *duration,
			bias:           *bias,
			coverageReport: *coverageReport,
		}, stdout, stderr)
	}

	// -replay re-runs the (scenario or default) deterministic workload for the
	// seed in verbose full-trace mode and, on a violation, shrinks to a minimal
	// reproducer. It must precede the -scenario branch so `-scenario X -replay`
	// replays scenario X.
	if *replay {
		return runReplayMode(seed, *scenario, *injectDemoFault, stdout, stderr)
	}

	// -scenario runs a named scenario from the catalogue for the seed.
	if *scenario != "" {
		return runScenarioMode(seed, *scenario, *verbose, stdout, stderr)
	}

	// Phase-3 modes drive the real Bolt wire path against a genuine bolt/server.
	switch *mode {
	case "engine":
		// Fall through to the engine-API safety loop below.
	case "wire":
		return runWireMode(seed, *verbose, stdout, stderr)
	case "concurrent":
		return runConcurrentMode(seed, *conns, *opsPerConn, stdout, stderr)
	case "liveness":
		return runLivenessMode(seed, *conns, *opsPerConn, stdout, stderr)
	default:
		stderr.printf("sim: unknown mode %q (want engine|wire|concurrent|liveness)\n", *mode)
		return 2
	}

	wlFactory, ok := workloadByName(*workloadName, stderr)
	if !ok {
		return 2
	}

	cfg := sim.Config{
		Seed:       seed,
		MaxTicks:   *ticks,
		CheckEvery: *checkEvery,
		Workload:   wlFactory(sim.NewSeed(seed)),
	}
	if *crashes {
		cfg.Crash = sim.CrashConfig{Enabled: true}
	}
	if *checkpoint {
		cfg.Checkpoint = sim.CheckpointConfig{Enabled: true, Every: *checkpointEvery}
	}
	if *verbose {
		stdout.printf("Running simulation: seed=%d ticks=%d workload=%s crashes=%v checkpoint=%v\n", seed, *ticks, *workloadName, *crashes, *checkpoint)
		cfg.OnOp = func(tick int64, op sim.Op) {
			stdout.printf("  tick=%d %s %q %v\n", tick, op.Kind, op.Cypher, op.Params)
		}
		cfg.OnCrash = func(tick int64, replayedWALOps int) {
			stdout.printf("  CRASH tick=%d -> recovery replayed %d WAL ops\n", tick, replayedWALOps)
		}
	}
	sm, err := sim.New(cfg)
	if err != nil {
		stderr.printf("sim: %v\n", err)
		return 2
	}
	defer func() { _ = sm.Close() }()

	report, err := sm.Run(context.Background())
	if err != nil {
		stderr.printf("sim: run error: %v\n", err)
		return 1
	}
	if report != nil {
		stderr.printf("%s", report.String())
		return 1
	}
	switch {
	case *crashes && *checkpoint:
		stdout.printf("Simulation passed. Seed: %d, Ticks: %d, Crashes: %d, Replayed WAL ops: %d, Checkpoints: %d\n",
			seed, *ticks, sm.CrashCount(), sm.ReplayedOps(), sm.CheckpointCount())
	case *crashes:
		stdout.printf("Simulation passed. Seed: %d, Ticks: %d, Crashes: %d, Replayed WAL ops: %d\n",
			seed, *ticks, sm.CrashCount(), sm.ReplayedOps())
	case *checkpoint:
		stdout.printf("Simulation passed. Seed: %d, Ticks: %d, Checkpoints: %d\n", seed, *ticks, sm.CheckpointCount())
	default:
		stdout.printf("Simulation passed. Seed: %d, Ticks: %d\n", seed, *ticks)
	}
	return 0
}

// runWireMode runs the deterministic LOCK-STEP single-connection Bolt-wire demo:
// it drives a fixed honest op sequence over the real bolt/server twice and
// verifies the two transcripts are byte-identical (the reproducibility proof).
func runWireMode(seed uint64, verbose bool, stdout, stderr *errWriter) int {
	const nOps = 40
	first, err := sim.RunLockStepWire(seed, nOps)
	if err != nil {
		stderr.printf("sim: wire mode: %v\n", err)
		return 1
	}
	second, err := sim.RunLockStepWire(seed, nOps)
	if err != nil {
		stderr.printf("sim: wire mode: %v\n", err)
		return 1
	}
	if !first.Equal(second) {
		stderr.printf("sim: LOCK-STEP wire transcript NOT reproducible for seed %d\n", seed)
		return 1
	}
	if verbose {
		for i, e := range first.Exchanges {
			stdout.printf("  [%d] %s -> %s\n", i, e.Op, e.Response)
		}
	}
	stdout.printf("Wire round-trip reproducible. Seed: %d, Exchanges: %d (lock-step, byte-identical across two runs)\n",
		seed, len(first.Exchanges))
	return 0
}

// runConcurrentMode runs the concurrent (non-bit-reproducible) Bolt-wire mode and
// reports the eventual-consistency oracle at quiescence.
func runConcurrentMode(seed uint64, conns, opsPerConn int, stdout, stderr *errWriter) int {
	srv, err := sim.NewSimServer(sim.SimEngineForServer(), clock.Real())
	if err != nil {
		stderr.printf("sim: concurrent mode: %v\n", err)
		return 1
	}
	defer func() { _ = srv.Close() }()

	res, err := sim.RunConcurrent(context.Background(), srv, sim.ConcurrentConfig{
		Seed:        seed,
		Connections: conns,
		OpsPerConn:  opsPerConn,
	})
	if err != nil {
		stderr.printf("sim: concurrent mode: %v\n", err)
		return 1
	}
	if !res.Consistent() {
		stderr.printf("CONCURRENT INCONSISTENT seed=%d engine=%d acked=%d panics=%d transport=%d\n",
			seed, res.EngineNodeCount, res.AckedCreates, res.Panics, res.TransportErrors)
		return 1
	}
	stdout.printf("Concurrent run consistent. Seed: %d, Conns: %d, Acked creates: %d (==engine), Bounded rejects: %d (NOT bit-reproducible)\n",
		seed, res.Connections, res.AckedCreates, res.BoundedRejects)
	return 0
}

// runLivenessMode runs the two-phase safety->liveness flow and reports
// convergence, or the liveness failure report (with seed + pending dump) on a
// non-converging run.
func runLivenessMode(seed uint64, conns, opsPerConn int, stdout, stderr *errWriter) int {
	srv, err := sim.NewSimServer(sim.SimEngineForServer(), clock.Real())
	if err != nil {
		stderr.printf("sim: liveness mode: %v\n", err)
		return 1
	}
	defer func() { _ = srv.Close() }()

	// Safety phase: concurrent mixed workload including the bounded overload actor.
	safety, err := sim.RunConcurrent(context.Background(), srv, sim.ConcurrentConfig{
		Seed:        seed,
		Connections: conns,
		OpsPerConn:  opsPerConn,
		Mix:         &sim.ConcurrentMix{WriterWeight: 0.5, ReaderWeight: 0.3, OverloadWeight: 0.2},
	})
	if err != nil {
		stderr.printf("sim: liveness safety phase: %v\n", err)
		return 1
	}
	if !safety.Consistent() {
		stderr.printf("SAFETY INCONSISTENT seed=%d engine=%d acked=%d\n", seed, safety.EngineNodeCount, safety.AckedCreates)
		return 1
	}

	// Liveness phase: faults healed, must converge.
	live, err := sim.RunLiveness(context.Background(), srv, clock.Real(), sim.LivenessConfig{
		Seed:           seed,
		Connections:    conns,
		OpsPerConn:     opsPerConn,
		ConvergeBudget: 30 * time.Second,
	})
	if err != nil {
		stderr.printf("sim: liveness phase: %v\n", err)
		return 1
	}
	if !live.Converged {
		stderr.printf("%s", live.Report())
		return 1
	}
	stdout.printf("Two-phase safety->liveness converged. Seed: %d, Safety acked: %d, Liveness ticks: %d\n",
		seed, safety.AckedCreates, live.Ticks)
	return 0
}

// runListScenarios prints the scenario catalogue (name + description + mode) and
// returns 0. It is the -list-scenarios handler.
func runListScenarios(stdout, stderr *errWriter) int {
	reg, err := sim.DefaultRegistry()
	if err != nil {
		stderr.printf("sim: build catalogue: %v\n", err)
		return 1
	}
	stdout.printf("Scenario catalogue (%d):\n", reg.Len())
	scenarios := reg.Scenarios()
	for i := range scenarios {
		sc := &scenarios[i]
		stdout.printf("  %-16s [%s] seed=%d\n      %s\n", sc.Name, sc.Mode, sc.DefaultSeed, sc.Description)
	}
	return 0
}

// runScenarioMode runs a named scenario for the seed and returns the process
// exit code: 0 when it passes, 1 on a violation (printing the report, which
// carries a "Reproduce with:" line). An unknown scenario name is exit 2.
func runScenarioMode(seed uint64, name string, verbose bool, stdout, stderr *errWriter) int {
	reg, err := sim.DefaultRegistry()
	if err != nil {
		stderr.printf("sim: build catalogue: %v\n", err)
		return 1
	}
	sc, ok := reg.Lookup(name)
	if !ok {
		stderr.printf("sim: unknown scenario %q (use -list-scenarios)\n", name)
		return 2
	}
	if verbose {
		stdout.printf("Running scenario %q [%s] seed=%d: %s\n", sc.Name, sc.Mode, seed, sc.Description)
	}
	report, err := sc.Run(context.Background(), seed)
	if err != nil {
		stderr.printf("sim: scenario %q: %v\n", name, err)
		return 1
	}
	if report != nil {
		stderr.printf("%s", report.String())
		return 1
	}
	stdout.printf("Scenario %q passed. Seed: %d\n", sc.Name, seed)
	return 0
}

// runReplayMode re-runs a deterministic workload for the seed in verbose
// full-trace mode: it records the trace (printing each op), and on a violation
// it shrinks the trace to a minimal reproducer and prints it, exiting 1 with the
// reproduce line + minimal trace. With no violation it exits 0.
//
// When scenarioName is non-empty the replay uses that catalogue scenario's
// deterministic config; the scenario must be a deterministic (bit-replayable)
// mode — replay/shrink do not apply to the concurrent or liveness modes. With no
// scenario it replays the default write-heavy deterministic workload.
func runReplayMode(seed uint64, scenarioName string, injectDemoFault bool, stdout, stderr *errWriter) int {
	cfg, ok := replayConfig(seed, scenarioName, stderr)
	if !ok {
		return 2
	}
	if injectDemoFault {
		// Keep the demo fast: a small trace shrinks quickly and still shows the
		// orders-of-magnitude reduction.
		cfg.MaxTicks = 500
	}

	// Verbose full-trace: print each op as the recorder observes it.
	cfg.OnOp = func(tick int64, op sim.Op) {
		stdout.printf("  tick=%d %s %q %v\n", tick, op.Kind, op.Cypher, op.Params)
	}
	stdout.printf("Replaying seed=%d (deterministic, full trace)\n", seed)

	trace, report, err := sim.RecordTrace(context.Background(), cfg)
	if err != nil {
		stderr.printf("sim: replay record: %v\n", err)
		return 1
	}

	if injectDemoFault {
		// DEMO ONLY: the engine is correct, so a real replay never fails. Inject a
		// synthetic lost-write fault on a CREATE op so the shrinker has a violation
		// to reduce, demonstrating the minimal-reproducer flow end to end.
		report = injectDemoFaultAndReplay(&trace, stdout, stderr)
	}

	if report == nil {
		stdout.printf("Replay passed (no violation). Seed: %d, Ops: %d\n", seed, trace.Len())
		return 0
	}

	// A violation: shrink to a minimal reproducer and attach it to the report.
	shrunk, serr := sim.ShrinkTrace(context.Background(), trace, sim.ShrinkConfig{})
	if serr != nil {
		// Shrinking failed (e.g. the violation is not scripted-replayable); still
		// surface the original failure report.
		stderr.printf("sim: shrink: %v\n", serr)
		stderr.printf("%s", report.String())
		return 1
	}
	report.Shrunk = &shrunk
	stderr.printf("%s", report.String())
	return 1
}

// injectDemoFaultAndReplay mutates trace in place to carry a synthetic lost-write
// fault on its first CREATE op, replays it, and returns the resulting report (a
// deterministic divergence). It is the -inject-demo-fault path: the engine is
// correct, so this is the ONLY way to exhibit a failure-shrink end to end in a
// demo. It is clearly labelled in the output as a demo injection.
func injectDemoFaultAndReplay(trace *sim.Trace, stdout, stderr *errWriter) *sim.SimReport {
	stdout.printf("[DEMO] injecting a synthetic lost-write fault to demonstrate shrinking (the engine itself is correct)\n")
	injected := false
	for i := range trace.Ops {
		if trace.Ops[i].Op.Kind == sim.OpCreate {
			trace.Ops[i].Fault = sim.FaultDropEngineWrite
			injected = true
			break
		}
	}
	if !injected {
		stderr.printf("[DEMO] no CREATE op in the trace to inject into\n")
		return nil
	}
	res, err := sim.ReplayTrace(context.Background(), *trace)
	if err != nil {
		stderr.printf("[DEMO] replay error: %v\n", err)
		return nil
	}
	return res.Report
}

// replayConfig builds the deterministic Config a replay drives: from the named
// scenario when given (rejecting a non-deterministic mode), else the default
// write-heavy deterministic workload. The bool is false (with a message
// written) on an error.
func replayConfig(seed uint64, scenarioName string, stderr *errWriter) (sim.Config, bool) {
	if scenarioName == "" {
		return sim.Config{
			Seed:     seed,
			MaxTicks: 100000,
			Workload: sim.WriteHeavyWorkload(sim.NewSeed(seed)),
		}, true
	}
	reg, err := sim.DefaultRegistry()
	if err != nil {
		stderr.printf("sim: build catalogue: %v\n", err)
		return sim.Config{}, false
	}
	sc, ok := reg.Lookup(scenarioName)
	if !ok {
		stderr.printf("sim: unknown scenario %q (use -list-scenarios)\n", scenarioName)
		return sim.Config{}, false
	}
	if !sc.Mode.Reproducible() {
		stderr.printf("sim: scenario %q is %s mode, which is not bit-replayable (replay/shrink need a deterministic mode)\n",
			scenarioName, sc.Mode)
		return sim.Config{}, false
	}
	return sc.DeterministicConfig(seed), true
}

// swarmOptions carries the parsed swarm flags into runSwarmMode.
type swarmOptions struct {
	scenario       string
	workers        int
	runs           int
	duration       time.Duration
	bias           bool
	coverageReport bool
}

// defaultSwarmScenario is the scenario a swarm runs when -scenario is empty: a
// fast deterministic mix so a large swarm stays cheap and reproducible per seed.
const defaultSwarmScenario = sim.ScenarioReadHeavy

// runSwarmMode runs a bounded-worker, time/count-boxed swarm of derived seeds
// from the master seed, feeding a coverage tracker, and prints the summary
// (runs/passes/failures with reproduce lines) plus the coverage report when
// requested. It exits non-zero if any seed failed.
func runSwarmMode(masterSeed uint64, opt swarmOptions, stdout, stderr *errWriter) int {
	reg, err := sim.DefaultRegistry()
	if err != nil {
		stderr.printf("sim: build catalogue: %v\n", err)
		return 1
	}
	scenario := opt.scenario
	if scenario == "" {
		scenario = defaultSwarmScenario
	}
	if _, ok := reg.Lookup(scenario); !ok {
		stderr.printf("sim: unknown scenario %q (use -list-scenarios)\n", scenario)
		return 2
	}

	// The coverage tracker biases AND reports over a swarm-appropriate scenario
	// universe. The soak-scale long-running scenario (millions of ticks per run)
	// would stall an interactive swarm, so it is excluded from the universe
	// (it remains runnable explicitly via -scenario=long-running). One tracker
	// serves both selection and reporting so the bias reflects what it has seen.
	universe := reg.Names()
	if opt.bias {
		universe = swarmBiasUniverse(reg)
	}
	tracker := sim.NewCoverageTracker(universe)
	cfg := sim.SwarmConfig{
		MasterSeed: masterSeed,
		Scenario:   scenario,
		Workers:    opt.workers,
		Runs:       opt.runs,
		Duration:   opt.duration,
		Observe:    tracker.Record,
	}
	// A pure-duration budget (no positive runs) drops the count bound; -bias
	// steers selection across the universe toward under-covered scenarios.
	if opt.duration > 0 && opt.runs <= 0 {
		cfg.Runs = 0
	}
	if opt.bias {
		cfg.Selector = tracker
	}

	sw, err := sim.NewSwarm(reg, &cfg)
	if err != nil {
		stderr.printf("sim: swarm: %v\n", err)
		return 2
	}

	res, err := sw.Run(context.Background())
	if err != nil {
		stderr.printf("sim: swarm run: %v\n", err)
		return 1
	}

	stdout.printf("%s\n", res.Summary())
	if opt.coverageReport {
		stdout.printf("%s\n", tracker.Summary().String())
		if unobs := tracker.UnobservableSignals(); len(unobs) > 0 {
			stdout.printf("Unobservable without a production hook (not tracked): %v\n", unobs)
		}
	}
	if res.FailureCount() > 0 {
		// The reproduce lines are already in the summary; exit non-zero so CI/the
		// caller treats any failing seed as a failure.
		return 1
	}
	return 0
}

// swarmBiasUniverse returns the scenario names the biased swarm steers over:
// every catalogue scenario EXCEPT the soak-scale long-running one, whose
// millions-of-ticks budget would stall an interactive swarm. The excluded
// scenario stays runnable explicitly via -scenario=long-running.
func swarmBiasUniverse(reg *sim.Registry) []string {
	all := reg.Names()
	out := make([]string, 0, len(all))
	for _, n := range all {
		if n == sim.ScenarioLongRunning {
			continue
		}
		out = append(out, n)
	}
	return out
}

// runCoverageTemplate prints the coverage report shape for a fresh tracker over
// the catalogue (every scenario bucket unexplored) and exits 0. It is the
// -coverage-report-alone handler.
func runCoverageTemplate(stdout, stderr *errWriter) int {
	reg, err := sim.DefaultRegistry()
	if err != nil {
		stderr.printf("sim: build catalogue: %v\n", err)
		return 1
	}
	tracker := sim.NewCoverageTracker(reg.Names())
	stdout.printf("%s\n", tracker.Summary().String())
	stdout.printf("Unobservable without a production hook (not tracked): %v\n", tracker.UnobservableSignals())
	return 0
}

// errWriter latches the first write error from a sequence of formatted writes so
// each call site need not check it individually. A failed write to the
// process's own stdout/stderr is not separately actionable, but latching the
// error keeps every write checked (errcheck-clean) and would surface a broken
// pipe to anyone who inspects Err.
type errWriter struct {
	w   io.Writer
	err error
}

// printf writes a formatted line, recording the first error encountered.
func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

// splitSeedArg separates an optional leading positional seed token (the first
// argument that does not start with '-') from the flag tokens. It returns
// (seedArgs, flagArgs): seedArgs holds the single seed token when present (else
// empty), and flagArgs holds every remaining token in its original order. Only
// a leading positional is treated as the seed; a non-flag token appearing after
// flags is left in flagArgs so flag.Parse reports it as unexpected.
func splitSeedArg(args []string) (seedArgs, flagArgs []string) {
	for i, a := range args {
		if a == "" || a[0] == '-' {
			continue
		}
		flagArgs = make([]string, 0, len(args)-1)
		flagArgs = append(flagArgs, args[:i]...)
		flagArgs = append(flagArgs, args[i+1:]...)
		return []string{a}, flagArgs
	}
	return nil, args
}

// resolveSeed returns the seed to use: the first positional argument parsed as
// an unsigned integer, or a freshly-chosen random seed when none is given. The
// random source is math/rand/v2's auto-seeded top-level generator, used only to
// pick the value (never inside the deterministic simulation). The chosen seed
// is always reported by the caller so the run can be reproduced.
func resolveSeed(positional []string, stderr *errWriter) (uint64, bool) {
	if len(positional) == 0 {
		//nolint:gosec // G404: this is a test-harness seed selector; a
		// non-cryptographic source is intentional and the chosen value is
		// printed so the run is reproducible.
		return mrand.Uint64(), true
	}
	v, err := strconv.ParseUint(positional[0], 10, 64)
	if err != nil {
		stderr.printf("sim: invalid seed %q: %v\n", positional[0], err)
		return 0, false
	}
	return v, true
}

// workloadFactory builds a workload from a seed.
type workloadFactory func(*sim.Seed) *sim.Workload

// workloadByName maps a workload name to its factory.
func workloadByName(name string, stderr *errWriter) (workloadFactory, bool) {
	switch name {
	case "default":
		return sim.DefaultWorkload, true
	case "write-heavy":
		return sim.WriteHeavyWorkload, true
	case "read-heavy":
		return sim.ReadHeavyWorkload, true
	case "bad-actor":
		return sim.BadActorWorkload, true
	default:
		stderr.printf("sim: unknown workload %q (want default|write-heavy|read-heavy|bad-actor)\n", name)
		return nil, false
	}
}
